package gitsync

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/vikki8/systemcd/internal/host"
)

// gitCmd runs git in dir, failing the test on error.
func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// originRepo creates a bare-ish source repository with one commit on main.
func originRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	gitCmd(t, dir, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(dir, "manifests.yaml"), []byte("apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-m", "initial")
	return dir
}

func TestSyncClonesThenFetches(t *testing.T) {
	origin := originRepo(t)
	checkout := filepath.Join(t.TempDir(), "repo")

	s := New(host.NewOS())
	ctx := context.Background()
	src := Source{URL: origin, Ref: "main", Dir: checkout, Depth: 1}

	rev1, err := s.Sync(ctx, src)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if len(rev1) != 40 {
		t.Errorf("revision = %q, want a full sha", rev1)
	}
	if _, err := os.Stat(filepath.Join(checkout, "manifests.yaml")); err != nil {
		t.Fatalf("clone did not produce the manifests: %v", err)
	}

	// A second commit upstream must be picked up by the fetch path.
	if err := os.WriteFile(filepath.Join(origin, "second.yaml"), []byte("apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, origin, "add", ".")
	gitCmd(t, origin, "commit", "-m", "second")

	rev2, err := s.Sync(ctx, src)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if rev2 == rev1 {
		t.Error("the fetch did not advance the checkout")
	}
	if _, err := os.Stat(filepath.Join(checkout, "second.yaml")); err != nil {
		t.Errorf("the new file was not fetched: %v", err)
	}
}

func TestSyncDiscardsLocalEdits(t *testing.T) {
	// The checkout is a cache of the remote. An edit made there is drift, and
	// the next sync must throw it away rather than preserve it.
	origin := originRepo(t)
	checkout := filepath.Join(t.TempDir(), "repo")

	s := New(host.NewOS())
	ctx := context.Background()
	src := Source{URL: origin, Ref: "main", Dir: checkout, Depth: 1}

	if _, err := s.Sync(ctx, src); err != nil {
		t.Fatalf("sync: %v", err)
	}
	target := filepath.Join(checkout, "manifests.yaml")
	if err := os.WriteFile(target, []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sync(ctx, src); err != nil {
		t.Fatalf("resync: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "tampered\n" {
		t.Error("a local edit in the checkout should have been reset")
	}
}

func TestRevisionOnANonRepoIsEmpty(t *testing.T) {
	// Running systemcd against a plain directory of manifests is supported;
	// it just has no revision to report.
	s := New(host.NewOS())
	rev, err := s.Revision(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	if rev != "" {
		t.Errorf("revision = %q, want empty", rev)
	}
}

func TestCheckoutMovesToARevision(t *testing.T) {
	origin := originRepo(t)
	first := gitCmd(t, origin, "rev-parse", "HEAD")
	first = first[:40]

	if err := os.WriteFile(filepath.Join(origin, "third.yaml"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, origin, "add", ".")
	gitCmd(t, origin, "commit", "-m", "third")

	checkout := filepath.Join(t.TempDir(), "repo")
	s := New(host.NewOS())
	ctx := context.Background()
	if _, err := s.Sync(ctx, Source{URL: origin, Ref: "main", Dir: checkout}); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if err := s.Checkout(ctx, checkout, first); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	rev, err := s.Revision(ctx, checkout)
	if err != nil {
		t.Fatal(err)
	}
	if rev != first {
		t.Errorf("revision after rollback = %q, want %q", rev, first)
	}
	if _, err := os.Stat(filepath.Join(checkout, "third.yaml")); err == nil {
		t.Error("rolling back should have removed the newer file")
	}
}
