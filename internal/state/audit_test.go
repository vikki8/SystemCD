package state

import (
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/vikki8/systemcd/internal/host"
	"github.com/vikki8/systemcd/internal/paths"
)

// failingSyncHost refuses to publish the state file, the way a full disk
// would, while every other operation behaves normally.
type failingSyncHost struct {
	*host.MemHost
	fail bool
}

func (f *failingSyncHost) WriteFileSync(p string, data []byte, mode fs.FileMode) error {
	if f.fail {
		return errors.New("no space left on device")
	}
	return f.MemHost.WriteFileSync(p, data, mode)
}

func TestSaveNeverRotatesACorruptStateOverAGoodBackup(t *testing.T) {
	// The state file is torn, and the run recovered from .bak. If the next
	// save rotates the torn file into .bak and then fails to write (the disk
	// that tore it is still full), both copies are garbage and every
	// ownership record on the host is gone.
	mem := host.NewMem()
	h := &failingSyncHost{MemHost: mem}
	s := &Store{Host: h, Path: "/var/lib/systemcd/state.json", Warnf: func(string, ...any) {}}

	good := &Snapshot{Resources: map[string]Record{"File/a": {Kind: "File", Name: "a"}}}
	if err := s.Save(good); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(good); err != nil {
		t.Fatal(err)
	}
	mem.SetFile(s.Path, `{"version":2,"resources":{"File/a":`, 0o600, 0, 0)

	snap, err := s.Load()
	if err != nil {
		t.Fatalf("Load should recover from the backup: %v", err)
	}

	h.fail = true
	if err := s.Save(snap); err == nil {
		t.Fatal("setup: the save should fail")
	}
	h.fail = false

	again, err := s.Load()
	if err != nil {
		t.Fatalf("after a failed save the backup must still be usable: %v", err)
	}
	if _, ok := again.Resources["File/a"]; !ok {
		t.Errorf("ownership records were lost: %v", again.Refs())
	}
}

func TestSaveKeepsTheCorruptFileForInspection(t *testing.T) {
	// With no backup to fall back on, the run starts from empty state. The
	// torn file may still be recoverable by hand, so it must not simply be
	// overwritten.
	s, h, _ := newStore(t)
	torn := `{"version":2,"resources":{"File/a":{"kind":"File"`
	h.SetFile(s.Path, torn, 0o600, 0, 0)

	snap, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(snap); err != nil {
		t.Fatal(err)
	}
	var kept bool
	for _, p := range h.Paths() {
		if data, err := h.ReadFile(p); err == nil && string(data) == torn {
			kept = true
		}
	}
	if !kept {
		t.Errorf("the corrupt state file was overwritten without a copy; paths = %v", h.Paths())
	}
}

func TestFirstApplyWritesNoEmptyHistoryEntry(t *testing.T) {
	// Before the first apply there is no previous revision. Recording one
	// anyway leaves an entry with an empty revision and a year-1 timestamp.
	snap := emptySnapshot()
	snap.RecordApply("rev1", 3)
	if len(snap.History) != 0 {
		t.Errorf("history = %+v, want nothing before a second revision exists", snap.History)
	}
}

func TestHistoryCarriesEachRevisionsOwnChangeCount(t *testing.T) {
	// The entry for rev1 must say how much rev1's apply changed, not how
	// much the apply that replaced it changed.
	snap := emptySnapshot()
	snap.RecordApply("rev1", 3)
	// The agent keeps reconciling rev1; in-sync passes change nothing and
	// must not wipe the count, while a healed drift adds to it.
	snap.RecordApply("rev1", 0)
	snap.RecordApply("rev1", 1)
	snap.RecordApply("rev1", 0)
	snap.UpdatedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	snap.RecordApply("rev2", 1)

	if len(snap.History) != 1 {
		t.Fatalf("history = %+v, want one entry for rev1", snap.History)
	}
	if got := snap.History[0]; got.Revision != "rev1" || got.Changed != 4 || got.AppliedAt.IsZero() {
		t.Errorf("history[0] = %+v, want rev1 with 4 changes", got)
	}
}

func TestStoreKeepsItsFilesNextToItsPath(t *testing.T) {
	// A store somewhere other than the default location must create its own
	// directory root-only and lock next to itself, not in /var/lib/systemcd.
	h := host.NewMem()
	a := &Store{Host: h, Path: "/srv/a/state.json"}
	b := &Store{Host: h, Path: "/srv/b/state.json"}

	if err := a.Save(emptySnapshot()); err != nil {
		t.Fatal(err)
	}
	info, err := h.Stat("/srv/a")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode != 0o700 {
		t.Errorf("state directory mode = %o, want 0700", info.Mode)
	}

	unlock, err := a.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if unlockB, err := b.Lock(); err != nil {
		t.Errorf("a store in another directory was locked out: %v", err)
	} else {
		unlockB()
	}
	if _, err := (&Store{Host: h, Path: "/srv/a/state.json"}).Lock(); !errors.Is(err, host.ErrLocked) {
		t.Errorf("a second store on the same path took the lock: %v", err)
	}
	if got := New(h).LockPath(); got != paths.LockFile {
		t.Errorf("default lock = %s, want %s so CLI messages stay accurate", got, paths.LockFile)
	}
}

func TestPendingRefreshSurvivesASaveAndReload(t *testing.T) {
	s, _, _ := newStore(t)
	snap := emptySnapshot()
	snap.PendingRefresh = []string{"Service/nginx"}
	if err := s.Save(snap); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PendingRefresh) != 1 || got.PendingRefresh[0] != "Service/nginx" {
		t.Errorf("pending = %v", got.PendingRefresh)
	}
}

func TestV1MigrationKeepsTheRecordedDesiredStateAndTimestamp(t *testing.T) {
	// v1 kept what it had applied under "desired" and when under
	// "appliedAt". Dropping them on upgrade loses drift attribution and
	// replaces a real timestamp with a guess.
	s, h, _ := newStore(t)
	h.SetFile(s.Path, `{
	  "version": 1,
	  "updatedAt": "2026-03-01T00:00:00Z",
	  "resources": {
	    "File/a": {"kind":"File","name":"a","manifest":"kind: File\n",
	               "desired":{"checksum":"sha256:abc"},
	               "appliedAt":"2026-01-02T03:04:05Z"}
	  }
	}`, 0o600, 0, 0)

	snap, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rec := snap.Resources["File/a"]
	if rec.Applied["checksum"] != "sha256:abc" {
		t.Errorf("applied = %v, want v1's desired state carried over", rec.Applied)
	}
	want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if !rec.LastAppliedAt.Equal(want) {
		t.Errorf("last applied = %v, want v1's appliedAt %v", rec.LastAppliedAt, want)
	}
	if got := rec.OriginOf(map[string]string{"checksum": "sha256:edited"}, []string{"checksum"}); got != OriginExternal {
		t.Errorf("origin after migration = %q, want %q", got, OriginExternal)
	}

	// The migrated form must survive a save and reload unchanged.
	if err := s.Save(snap); err != nil {
		t.Fatal(err)
	}
	reloaded, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Resources["File/a"]; got.Applied["checksum"] != "sha256:abc" || !got.LastAppliedAt.Equal(want) {
		t.Errorf("after save/reload: %+v", got)
	}
}
