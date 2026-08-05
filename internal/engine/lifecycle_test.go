package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/vikki8/systemcd/internal/host"
	"github.com/vikki8/systemcd/internal/manifest"
	"github.com/vikki8/systemcd/internal/state"
)

const legacyConf = "# hand-edited in 2019, nobody remembers why\nworkers = 2\n"

const adoptRepo = `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: app-conf
spec:
  path: /etc/app.conf
  content: "workers = 8\n"
`

func adoptHost(t *testing.T) (*host.MemHost, *state.Store) {
	t.Helper()
	h := host.NewMem()
	h.SetFile("/etc/app.conf", legacyConf, 0o644, 0, 0)
	return h, &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
}

func adopt(t *testing.T, h host.Host, store *state.Store, repo *manifest.Repository, dryRun bool) *Report {
	t.Helper()
	rep, err := Adopt(context.Background(), AdoptOptions{
		Repo: repo, Host: h, Node: manifest.Node{Hostname: "legacy-01"},
		Store: store, DryRun: dryRun, Revision: "rev1",
	})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	return rep
}

func TestAdoptTakesOwnershipWithoutChangingTheHost(t *testing.T) {
	// The whole point of onboarding an existing fleet: day one must not
	// rewrite the machine, or nobody will authorize it.
	h, store := adoptHost(t)
	rep := adopt(t, h, store, repoFrom(t, adoptRepo), false)

	res := result(t, rep, "File/app-conf")
	if res.Action != ActionAdopt {
		t.Fatalf("action = %v, want adopt", res.Action)
	}
	if got, _ := h.ReadFile("/etc/app.conf"); string(got) != legacyConf {
		t.Errorf("adopt must not modify the file, got %q", got)
	}

	snap, _ := store.Load()
	rec, ok := snap.Resources["File/app-conf"]
	if !ok {
		t.Fatalf("ownership was not recorded: %v", snap.Refs())
	}
	if !rec.Adopted {
		t.Error("an adopted resource must be marked adopted")
	}
	// The recorded state is what is on the host, not what the manifest wants.
	// That is what makes the resource immediately in sync.
	if rec.Applied["checksum"] != rec.PriorState["checksum"] {
		t.Error("adoption should record the host's own state as the applied baseline")
	}
	if rec.BaselinePath == "" {
		t.Fatal("adoption must capture the original contents for a later restore")
	}
	if data, _ := h.ReadFile(rec.BaselinePath); string(data) != legacyConf {
		t.Errorf("baseline = %q, want the original contents", data)
	}
}

func TestAdoptShowsThePendingChangeBeforeYouOwnIt(t *testing.T) {
	h, store := adoptHost(t)
	rep := adopt(t, h, store, repoFrom(t, adoptRepo), true)

	res := result(t, rep, "File/app-conf")
	if !res.Diff.Has("checksum") {
		t.Errorf("adopt should preview what the repository would change, got %+v", res.Diff)
	}
	snap, _ := store.Load()
	if len(snap.Resources) != 0 {
		t.Error("a dry-run adopt must not record ownership")
	}
}

func TestAfterAdoptTheRepoChangeIsAttributedToTheRepo(t *testing.T) {
	// This is the payoff. Post-adoption, the pending change reads as "the
	// repository wants this", not "someone edited the box" — because the host
	// still holds exactly what systemcd recorded.
	h, store := adoptHost(t)
	adopt(t, h, store, repoFrom(t, adoptRepo), false)

	rep := run(t, h, repoFrom(t, adoptRepo), func(o *Options) {
		o.Store = store
		o.DryRun = true
	})
	res := result(t, rep, "File/app-conf")
	if res.Owner.Origin != state.OriginRepo {
		t.Errorf("origin = %q, want %q", res.Owner.Origin, state.OriginRepo)
	}
	if len(rep.ExternalDrift()) != 0 {
		t.Error("a freshly adopted resource must not look like an external edit")
	}
}

func TestAdoptSkipsResourcesThatDoNotExistYet(t *testing.T) {
	h := host.NewMem()
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	rep := adopt(t, h, store, repoFrom(t, adoptRepo), false)

	res := result(t, rep, "File/app-conf")
	if res.Action != ActionSkip {
		t.Errorf("action = %v, want skipped", res.Action)
	}
	if !strings.Contains(strings.Join(res.Messages, " "), "apply") {
		t.Errorf("the message should point at the right verb, got %v", res.Messages)
	}
}

func TestAdoptIsIdempotent(t *testing.T) {
	h, store := adoptHost(t)
	adopt(t, h, store, repoFrom(t, adoptRepo), false)
	rep := adopt(t, h, store, repoFrom(t, adoptRepo), false)

	if got := result(t, rep, "File/app-conf").Action; got != ActionNoop {
		t.Errorf("second adopt = %v, want in-sync", got)
	}
}

func release(t *testing.T, h host.Host, store *state.Store, repo *manifest.Repository, opts func(*ReleaseOptions)) *Report {
	t.Helper()
	o := ReleaseOptions{
		Repo: repo, Host: h, Node: manifest.Node{Hostname: "legacy-01"},
		Store: store, Refs: []string{"File/app-conf"},
	}
	if opts != nil {
		opts(&o)
	}
	rep, err := Release(context.Background(), o)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	return rep
}

func TestReleaseRestorePutsBackThePreAdoptionContents(t *testing.T) {
	h, store := adoptHost(t)
	adopt(t, h, store, repoFrom(t, adoptRepo), false)

	// systemcd now owns it and applies the repository's version.
	run(t, h, repoFrom(t, adoptRepo), func(o *Options) { o.Store = store })
	if got, _ := h.ReadFile("/etc/app.conf"); string(got) != "workers = 8\n" {
		t.Fatalf("setup: apply did not take effect, got %q", got)
	}

	// The repository no longer declares it, so releasing is legitimate.
	rep := release(t, h, store, repoFrom(t, "apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: other\nspec:\n  path: /etc/other\n  content: \"o\\n\"\n"),
		func(o *ReleaseOptions) { o.Mode = ReleaseRestore })

	res := result(t, rep, "File/app-conf")
	if res.Action != ActionRestore {
		t.Fatalf("action = %v (err %v), want restore", res.Action, res.Err)
	}
	if got, _ := h.ReadFile("/etc/app.conf"); string(got) != legacyConf {
		t.Errorf("content = %q, want the 2019 hand-edited original back", got)
	}
	snap, _ := store.Load()
	if _, still := snap.Resources["File/app-conf"]; still {
		t.Error("release must drop the ownership record")
	}
}

func TestReleasePreserveLeavesTheHostAlone(t *testing.T) {
	h, store := adoptHost(t)
	adopt(t, h, store, repoFrom(t, adoptRepo), false)
	run(t, h, repoFrom(t, adoptRepo), func(o *Options) { o.Store = store })

	rep := release(t, h, store, nil, nil)

	if got := result(t, rep, "File/app-conf").Action; got != ActionRelease {
		t.Fatalf("action = %v, want release", got)
	}
	if got, _ := h.ReadFile("/etc/app.conf"); string(got) != "workers = 8\n" {
		t.Errorf("preserve must not touch the file, got %q", got)
	}
	snap, _ := store.Load()
	if len(snap.Resources) != 0 {
		t.Error("ownership should be gone")
	}
}

func TestReleaseRefusesWhatTheRepositoryStillDeclares(t *testing.T) {
	// Releasing something git still asks for would be undone on the next
	// reconcile, so claiming it was released would be a lie.
	h, store := adoptHost(t)
	adopt(t, h, store, repoFrom(t, adoptRepo), false)

	rep := release(t, h, store, repoFrom(t, adoptRepo), nil)
	res := result(t, rep, "File/app-conf")
	if res.Err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(res.Err.Error(), "Remove it from the repository first") {
		t.Errorf("the error should explain the workflow, got: %v", res.Err)
	}
	snap, _ := store.Load()
	if _, still := snap.Resources["File/app-conf"]; !still {
		t.Error("a refused release must leave ownership intact")
	}
}

func TestReleaseForceOverridesTheRefusal(t *testing.T) {
	h, store := adoptHost(t)
	adopt(t, h, store, repoFrom(t, adoptRepo), false)

	rep := release(t, h, store, repoFrom(t, adoptRepo), func(o *ReleaseOptions) { o.Force = true })
	if got := result(t, rep, "File/app-conf").Action; got != ActionRelease {
		t.Errorf("action = %v, want release under --force", got)
	}
}

func TestReleaseRestoreRefusesKindsItCannotHonestlyRestore(t *testing.T) {
	// A removed package cannot be put back at a version that may no longer
	// exist in any repository. Saying so beats pretending.
	h := host.NewMem()
	h.Binaries["apt-get"] = true
	h.AddStub(host.CommandStub{Match: "dpkg-query", Stdout: "installed 1.2.3"})
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}

	repo := repoFrom(t, "apiVersion: systemcd.dev/v1\nkind: Package\nmetadata:\n  name: nginx\n")
	if _, err := Adopt(context.Background(), AdoptOptions{
		Repo: repo, Host: h, Node: manifest.Node{Hostname: "t"}, Store: store,
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := Release(context.Background(), ReleaseOptions{
		Host: h, Node: manifest.Node{Hostname: "t"}, Store: store,
		Refs: []string{"Package/nginx"}, Mode: ReleaseRestore,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := result(t, rep, "Package/nginx")
	if res.Action != ActionSkip {
		t.Fatalf("action = %v, want skipped", res.Action)
	}
	joined := strings.Join(res.Messages, " ")
	if !strings.Contains(joined, "cannot be restored") || !strings.Contains(joined, "--preserve") {
		t.Errorf("message should explain and offer the alternative, got %v", res.Messages)
	}
}

func TestReleaseDryRunChangesNothing(t *testing.T) {
	h, store := adoptHost(t)
	adopt(t, h, store, repoFrom(t, adoptRepo), false)
	run(t, h, repoFrom(t, adoptRepo), func(o *Options) { o.Store = store })

	release(t, h, store, nil, func(o *ReleaseOptions) { o.Mode = ReleaseRestore; o.DryRun = true })

	if got, _ := h.ReadFile("/etc/app.conf"); string(got) != "workers = 8\n" {
		t.Errorf("a dry run must not restore, got %q", got)
	}
	snap, _ := store.Load()
	if _, still := snap.Resources["File/app-conf"]; !still {
		t.Error("a dry run must not drop ownership")
	}
}

func TestReleaseOfSomethingUnownedIsNotAnError(t *testing.T) {
	h := host.NewMem()
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	rep, err := Release(context.Background(), ReleaseOptions{
		Host: h, Store: store, Refs: []string{"File/ghost"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := result(t, rep, "File/ghost").Action; got != ActionSkip {
		t.Errorf("action = %v, want skipped", got)
	}
}

func TestImplicitAdoptionDuringApplyAlsoCapturesABaseline(t *testing.T) {
	// Someone who runs `apply` straight at a legacy box without adopting
	// first should still be able to back out.
	h, store := adoptHost(t)
	run(t, h, repoFrom(t, adoptRepo), func(o *Options) { o.Store = store })

	snap, _ := store.Load()
	rec := snap.Resources["File/app-conf"]
	if !rec.Adopted || rec.BaselinePath == "" {
		t.Fatalf("implicit adoption should record a baseline, got %+v", rec)
	}
	if data, _ := h.ReadFile(rec.BaselinePath); string(data) != legacyConf {
		t.Errorf("baseline = %q, want the pre-apply contents", data)
	}
}

func TestContentDiffIsCapturedForReview(t *testing.T) {
	h, store := adoptHost(t)
	rep := run(t, h, repoFrom(t, adoptRepo), func(o *Options) { o.Store = store; o.DryRun = true })

	res := result(t, rep, "File/app-conf")
	if !res.HasContent {
		t.Fatal("a checksum change should carry the text behind it")
	}
	if !strings.Contains(res.ContentBefore, "workers = 2") || !strings.Contains(res.ContentAfter, "workers = 8") {
		t.Errorf("content = %q -> %q", res.ContentBefore, res.ContentAfter)
	}
}

func TestSensitiveFilesNeverExposeTheirContents(t *testing.T) {
	h := host.NewMem()
	h.SetFile("/etc/app/secret.env", "TOKEN=old-secret\n", 0o600, 0, 0)
	rep := run(t, h, repoFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: secret
spec:
  path: /etc/app/secret.env
  content: "TOKEN=new-secret\n"
  sensitive: true
`), func(o *Options) { o.DryRun = true })

	res := result(t, rep, "File/secret")
	if res.HasContent {
		t.Fatal("a sensitive file must not have its contents captured for display")
	}
	for _, f := range res.Diff {
		if strings.Contains(f.Have, "secret") || strings.Contains(f.Want, "secret") {
			t.Errorf("sensitive values leaked into the diff: %+v", f)
		}
	}
}
