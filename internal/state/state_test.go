package state

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vikki8/systemcd/internal/host"
)

func newStore(t *testing.T) (*Store, *host.MemHost, *[]string) {
	t.Helper()
	h := host.NewMem()
	var warnings []string
	s := &Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	s.Warnf = func(format string, args ...any) {
		warnings = append(warnings, format)
	}
	return s, h, &warnings
}

func TestLoadMissingStateIsEmptyNotAnError(t *testing.T) {
	s, _, _ := newStore(t)
	snap, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(snap.Resources) != 0 || snap.Version != Version {
		t.Errorf("snapshot = %+v, want an empty current-version snapshot", snap)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s, _, _ := newStore(t)
	now := time.Now().UTC().Truncate(time.Second)

	snap := &Snapshot{Resources: map[string]Record{
		"File/a": {
			Kind: "File", Name: "a",
			Claims:         []string{"path:/etc/a"},
			Applied:        map[string]string{"checksum": "sha256:abc", "mode": "0644"},
			Adopted:        true,
			PriorState:     map[string]string{"checksum": "sha256:old"},
			FirstAppliedAt: now, LastAppliedAt: now, LastChangedAt: now,
			Revision: "deadbeef",
		},
	}}
	if err := s.Save(snap); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rec := got.Resources["File/a"]
	if !rec.Adopted || rec.Revision != "deadbeef" || rec.Applied["checksum"] != "sha256:abc" {
		t.Errorf("record did not round-trip: %+v", rec)
	}
	if rec.PriorState["checksum"] != "sha256:old" {
		t.Errorf("prior state did not round-trip: %+v", rec.PriorState)
	}
	if !rec.LastChangedAt.Equal(now) {
		t.Errorf("timestamp = %v, want %v", rec.LastChangedAt, now)
	}
}

func TestCorruptStateRecoversFromBackup(t *testing.T) {
	// Losing every ownership record because one write was torn would be the
	// worst possible failure mode for a tool whose whole value is knowing
	// what it owns.
	s, h, warnings := newStore(t)

	if err := s.Save(&Snapshot{Resources: map[string]Record{
		"File/a": {Kind: "File", Name: "a", Claims: []string{"path:/etc/a"}},
	}}); err != nil {
		t.Fatal(err)
	}
	// A second save rotates the good copy into .bak.
	if err := s.Save(&Snapshot{Resources: map[string]Record{
		"File/a": {Kind: "File", Name: "a", Claims: []string{"path:/etc/a"}},
		"File/b": {Kind: "File", Name: "b"},
	}}); err != nil {
		t.Fatal(err)
	}

	h.SetFile(s.Path, `{"version":2,"resources":{"File/a":`, 0o600, 0, 0)

	snap, err := s.Load()
	if err != nil {
		t.Fatalf("Load should recover, got: %v", err)
	}
	if _, ok := snap.Resources["File/a"]; !ok {
		t.Errorf("recovered snapshot lost its records: %v", snap.Refs())
	}
	if len(*warnings) == 0 {
		t.Error("recovery must be reported, not silent")
	}
}

func TestCorruptStateWithNoBackupStartsEmptyAndWarns(t *testing.T) {
	s, h, warnings := newStore(t)
	h.SetFile(s.Path, "not json at all", 0o600, 0, 0)

	snap, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(snap.Resources) != 0 {
		t.Errorf("want an empty snapshot, got %v", snap.Refs())
	}
	if len(*warnings) == 0 {
		t.Fatal("starting from empty state silently would hide that ownership was lost")
	}
	if !strings.Contains((*warnings)[0], "unmanaged") {
		t.Errorf("the warning should spell out the consequence, got %q", (*warnings)[0])
	}
}

func TestStateFromANewerBuildIsRefused(t *testing.T) {
	s, h, _ := newStore(t)
	h.SetFile(s.Path, `{"version":99,"resources":{}}`, 0o600, 0, 0)

	if _, err := s.Load(); err == nil || !strings.Contains(err.Error(), "newer systemcd") {
		t.Fatalf("error = %v, want a refusal to interpret a newer schema", err)
	}
}

func TestV1StateMigratesWithoutLosingResources(t *testing.T) {
	s, h, _ := newStore(t)
	h.SetFile(s.Path, `{
	  "version": 1,
	  "revision": "abc123",
	  "updatedAt": "2026-01-02T03:04:05Z",
	  "resources": {
	    "File/a": {"kind":"File","name":"a","manifest":"kind: File\n","desired":{"mode":"0644"}}
	  }
	}`, 0o600, 0, 0)

	snap, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if snap.Version != Version {
		t.Errorf("version = %d, want %d", snap.Version, Version)
	}
	rec, ok := snap.Resources["File/a"]
	if !ok {
		t.Fatalf("migration dropped the resource: %v", snap.Refs())
	}
	if rec.LastAppliedAt.IsZero() || rec.FirstAppliedAt.IsZero() {
		t.Error("migration should backfill timestamps from the file's own updatedAt")
	}
	if rec.Adopted {
		t.Error("migration must not invent an adoption flag it cannot know")
	}
}

func TestOwnerFindsTheClaimingResource(t *testing.T) {
	snap := &Snapshot{Resources: map[string]Record{
		"File/nginx-conf": {Kind: "File", Name: "nginx-conf", Claims: []string{"path:/etc/nginx/nginx.conf"}},
		"Service/nginx":   {Kind: "Service", Name: "nginx", Claims: []string{"unit:nginx.service"}},
	}}

	rec, ok := snap.Owner("path:/etc/nginx/nginx.conf")
	if !ok || rec.Ref() != "File/nginx-conf" {
		t.Errorf("owner = %v (%v), want File/nginx-conf", rec.Ref(), ok)
	}
	if _, ok := snap.Owner("path:/etc/hosts"); ok {
		t.Error("an unmanaged path must report no owner")
	}
}

func TestOriginOfClassifiesDrift(t *testing.T) {
	rec := Record{Applied: map[string]string{"checksum": "sha256:written", "mode": "0644"}}

	cases := []struct {
		name     string
		observed map[string]string
		fields   []string
		want     Origin
	}{
		{
			"host still holds what we wrote, so the repo moved",
			map[string]string{"checksum": "sha256:written", "mode": "0644"},
			[]string{"checksum"},
			OriginRepo,
		},
		{
			"host no longer holds what we wrote",
			map[string]string{"checksum": "sha256:someone-else", "mode": "0644"},
			[]string{"checksum"},
			OriginExternal,
		},
		{
			"a field we never applied is a new expectation, not drift",
			map[string]string{"checksum": "sha256:written", "mode": "0644", "owner": "root"},
			[]string{"owner"},
			OriginRepo,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rec.OriginOf(tc.observed, tc.fields); got != tc.want {
				t.Errorf("OriginOf() = %q, want %q", got, tc.want)
			}
		})
	}

	if got := (Record{}).OriginOf(nil, []string{"x"}); got != OriginNew {
		t.Errorf("a record with no applied state = %q, want %q", got, OriginNew)
	}
}

func TestLockIsExclusive(t *testing.T) {
	h := host.NewMem()
	s := &Store{Host: h, Path: "/var/lib/systemcd/state.json"}

	unlock, err := s.Lock()
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	if _, err := s.Lock(); !errors.Is(err, host.ErrLocked) {
		t.Fatalf("second lock error = %v, want ErrLocked", err)
	}
	if err := unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if _, err := s.Lock(); err != nil {
		t.Fatalf("lock after release: %v", err)
	}
}

func TestHistoryDrivesRollback(t *testing.T) {
	snap := emptySnapshot()
	snap.RecordApply("rev1", 3)
	snap.RecordApply("rev2", 1)

	if snap.Revision != "rev2" {
		t.Errorf("revision = %q", snap.Revision)
	}
	prev, ok := snap.PreviousRevision()
	if !ok || prev != "rev1" {
		t.Errorf("previous = %q (%v), want rev1", prev, ok)
	}

	// Reapplying the same revision must not push a duplicate history entry.
	before := len(snap.History)
	snap.RecordApply("rev2", 0)
	if len(snap.History) != before {
		t.Errorf("history grew from a no-op reapply: %+v", snap.History)
	}
}
