package gitsync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vikki8/systemcd/internal/host"
)

func commitFile(t *testing.T, repo, name, content, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo, "add", ".")
	gitCmd(t, repo, "commit", "-m", msg)
	return strings.TrimSpace(gitCmd(t, repo, "rev-parse", "HEAD"))
}

func TestSyncAfterALocalEditDoesNotRollBackToCloneTime(t *testing.T) {
	// A tampered checkout used to make `checkout FETCH_HEAD` fail, and the
	// fallback `checkout main` then picked the local branch the clone made,
	// which is never updated: the host was silently rolled back to the
	// clone-time revision with no error.
	origin := originRepo(t)
	commitFile(t, origin, "t.yaml", "t1\n", "add t")
	checkout := filepath.Join(t.TempDir(), "repo")
	s := New(host.NewOS())
	ctx := context.Background()
	src := Source{URL: origin, Ref: "main", Dir: checkout, Depth: 1}
	if _, err := s.Sync(ctx, src); err != nil {
		t.Fatal(err)
	}
	commitFile(t, origin, "manifests.yaml", "v2\n", "v2")
	if _, err := s.Sync(ctx, src); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(checkout, "t.yaml"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := commitFile(t, origin, "t.yaml", "t3\n", "v3")

	got, err := s.Sync(ctx, src)
	if err != nil {
		t.Fatalf("sync after a local edit: %v", err)
	}
	if got != want {
		t.Errorf("revision = %s, want the new upstream head %s", got, want)
	}
	for name, content := range map[string]string{"t.yaml": "t3\n", "manifests.yaml": "v2\n"} {
		data, _ := os.ReadFile(filepath.Join(checkout, name))
		if string(data) != content {
			t.Errorf("%s = %q, want %q", name, data, content)
		}
	}
}

func TestSyncRemovesUntrackedFiles(t *testing.T) {
	// An untracked manifest dropped into the checkout would be loaded and
	// applied as if it had come from git.
	origin := originRepo(t)
	checkout := filepath.Join(t.TempDir(), "repo")
	s := New(host.NewOS())
	ctx := context.Background()
	src := Source{URL: origin, Ref: "main", Dir: checkout, Depth: 1}
	if _, err := s.Sync(ctx, src); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(checkout, "planted.yaml")
	if err := os.WriteFile(planted, []byte("kind: Exec\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sync(ctx, src); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(planted); err == nil {
		t.Error("an untracked file survived the sync")
	}
}

func TestSyncToACommitTheServerWillNotServeByNameFindsThatCommit(t *testing.T) {
	// When fetching the ref by name fails, the fallback fetched everything
	// and checked out FETCH_HEAD, which is the remote's first branch, not the
	// commit asked for: a host pinned to a SHA silently ran the branch head.
	origin := originRepo(t)
	pinned := strings.TrimSpace(gitCmd(t, origin, "rev-parse", "HEAD"))
	commitFile(t, origin, "manifests.yaml", "v2\n", "v2")
	checkout := filepath.Join(t.TempDir(), "repo")
	s := New(host.NewOS())
	ctx := context.Background()

	// Protocol v0 refuses to serve an unadvertised commit by id.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.version")
	t.Setenv("GIT_CONFIG_VALUE_0", "0")
	url := "file://" + origin
	// A shallow clone does not already hold the pinned commit.
	if _, err := s.Sync(ctx, Source{URL: url, Ref: "main", Dir: checkout, Depth: 1}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Sync(ctx, Source{URL: url, Ref: pinned, Dir: checkout, Depth: 1})
	if err != nil {
		t.Fatalf("sync to a pinned commit: %v", err)
	}
	if got != pinned {
		t.Errorf("revision = %s, want the pinned commit %s", got, pinned)
	}
}

func TestSyncFailsRatherThanUsingAStaleLocalBranch(t *testing.T) {
	// A tracked branch that vanished upstream must be an error, never a
	// quiet checkout of the local branch left over from the clone.
	origin := originRepo(t)
	gitCmd(t, origin, "branch", "release")
	checkout := filepath.Join(t.TempDir(), "repo")
	s := New(host.NewOS())
	ctx := context.Background()
	src := Source{URL: origin, Ref: "release", Dir: checkout}
	if _, err := s.Sync(ctx, src); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, origin, "branch", "-D", "release")
	if rev, err := s.Sync(ctx, src); err == nil {
		t.Errorf("sync of a deleted branch succeeded at %s", rev)
	}
}

func TestSyncRejectsOptionLikeArguments(t *testing.T) {
	s := New(host.NewMem())
	ctx := context.Background()
	for _, src := range []Source{
		{URL: "--upload-pack=touch /tmp/pwned", Ref: "main", Dir: "/repo"},
		{URL: "https://example.com/r.git", Ref: "--upload-pack=touch /tmp/pwned", Dir: "/repo"},
	} {
		if _, err := s.Sync(ctx, src); err == nil || !strings.Contains(err.Error(), "must not start with '-'") {
			t.Errorf("Sync(%+v) err = %v, want a refusal", src, err)
		}
	}
	if err := s.Checkout(ctx, "/repo", "--orphan=x"); err == nil {
		t.Error("Checkout accepted an option-like revision")
	}
}

func TestCredentialsNeverReachLogsOrErrors(t *testing.T) {
	var logs []string
	s := New(host.NewOS())
	s.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	checkout := filepath.Join(t.TempDir(), "repo")
	_, err := s.Sync(context.Background(), Source{
		URL: "https://deploy:s3cr3t-token@127.0.0.1:1/fleet.git", Ref: "main", Dir: checkout, Depth: 1,
	})
	if err == nil {
		t.Fatal("expected the clone to fail")
	}
	all := strings.Join(append(logs, err.Error()), "\n")
	if strings.Contains(all, "s3cr3t-token") || strings.Contains(all, "deploy:") {
		t.Errorf("credentials leaked:\n%s", all)
	}
	if !strings.Contains(all, "127.0.0.1:1/fleet.git") {
		t.Errorf("the redacted URL should still identify the remote:\n%s", all)
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:tok@github.com/o/r.git": "https://xxxxx@github.com/o/r.git",
		"https://ghp_abc@github.com/o/r.git":  "https://xxxxx@github.com/o/r.git",
		"ssh://git@github.com/o/r.git":        "ssh://git@github.com/o/r.git",
		"ssh://git:pw@host/r.git":             "ssh://git:xxxxx@host/r.git",
		"git@github.com:o/r.git":              "git@github.com:o/r.git",
		"/srv/fleet":                          "/srv/fleet",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckoutDiscardsLocalEditsAndFetchesMissingCommits(t *testing.T) {
	origin := originRepo(t)
	first := strings.TrimSpace(gitCmd(t, origin, "rev-parse", "HEAD"))
	commitFile(t, origin, "manifests.yaml", "v2\n", "v2")
	commitFile(t, origin, "manifests.yaml", "v3\n", "v3")

	// A depth-1 clone does not hold the first commit at all, and over
	// protocol v0 the server will not hand it over by id either.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.version")
	t.Setenv("GIT_CONFIG_VALUE_0", "0")
	checkout := filepath.Join(t.TempDir(), "repo")
	s := New(host.NewOS())
	ctx := context.Background()
	if _, err := s.Sync(ctx, Source{URL: "file://" + origin, Ref: "main", Dir: checkout, Depth: 1}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "manifests.yaml"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkout(ctx, checkout, first); err != nil {
		t.Fatalf("rollback to a commit outside the shallow clone: %v", err)
	}
	rev, _ := s.Revision(ctx, checkout)
	if rev != first {
		t.Errorf("revision = %s, want %s", rev, first)
	}
	data, _ := os.ReadFile(filepath.Join(checkout, "manifests.yaml"))
	if string(data) == "tampered\n" {
		t.Error("a local edit survived the rollback checkout")
	}
}

func TestRestoreReturnsToHEAD(t *testing.T) {
	origin := originRepo(t)
	checkout := filepath.Join(t.TempDir(), "repo")
	s := New(host.NewOS())
	ctx := context.Background()
	want, err := s.Sync(ctx, Source{URL: origin, Ref: "main", Dir: checkout})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "manifests.yaml"), []byte("half-updated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.Restore(ctx, checkout)
	if err != nil || got != want {
		t.Fatalf("Restore = %s, %v; want %s", got, err, want)
	}
	if data, _ := os.ReadFile(filepath.Join(checkout, "manifests.yaml")); string(data) == "half-updated\n" {
		t.Error("Restore left a modified working tree")
	}
	if _, err := s.Restore(ctx, t.TempDir()); err == nil {
		t.Error("Restore of a non-checkout succeeded")
	}
}
