// Package state persists what systemcd owns on a host and what it last
// applied.
//
// This record is the trust anchor. It turns a two-way comparison (git vs
// host) into a three-way one, which is what makes three things possible that
// otherwise are not: pruning resources deleted from the repository, telling
// an external change apart from a repository change, and answering "does
// systemcd own this file?" without guessing.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/vikki8/systemcd/internal/host"
	"github.com/vikki8/systemcd/internal/paths"
)

// Version is the schema version of the state file.
//
//	1 — resources, revision, history
//	2 — ownership: claims, adoption, first/last applied, last changed
const Version = 2

// Origin classifies why a resource differs from its manifest. It is only
// answerable because we record what we last applied.
type Origin string

const (
	// OriginNew means systemcd has never applied this resource.
	OriginNew Origin = "new"
	// OriginRepo means the host still matches what we last applied, so the
	// manifest is what moved.
	OriginRepo Origin = "repo"
	// OriginExternal means the host no longer matches what we last applied:
	// something outside systemcd changed it.
	OriginExternal Origin = "external"
)

// Record is systemcd's ownership claim over one resource.
type Record struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Manifest is the YAML document that produced the resource, kept so a
	// pruned resource can be rebuilt and asked to delete itself.
	Manifest string `json:"manifest"`
	// Claims are the physical things on the host this resource owns, as
	// "kind:key" strings. This is what answers "who owns /etc/nginx.conf".
	Claims []string `json:"claims,omitempty"`
	// Applied is the complete desired state at the last successful apply.
	// Comparing the host against this, rather than only against the current
	// manifest, is what distinguishes external drift from a repo change.
	Applied map[string]string `json:"applied,omitempty"`
	// Adopted records that the resource already existed on the host when
	// systemcd first took ownership, rather than being created by it. It
	// matters when deciding whether pruning is safe.
	Adopted bool `json:"adopted,omitempty"`
	// PriorState is what the host looked like immediately before systemcd
	// first touched an adopted resource.
	PriorState map[string]string `json:"priorState,omitempty"`
	// BaselinePath points at a preserved copy of the resource's contents at
	// adoption. A checksum records that a file changed; only the bytes let
	// `release --restore` put the original back.
	BaselinePath string `json:"baselinePath,omitempty"`

	FirstAppliedAt time.Time `json:"firstAppliedAt"`
	LastAppliedAt  time.Time `json:"lastAppliedAt"`
	// LastChangedAt is when systemcd last actually modified the resource, as
	// opposed to merely confirming it was already in sync.
	LastChangedAt time.Time `json:"lastChangedAt,omitempty"`
	// Revision is the git revision in effect at the last change.
	Revision string `json:"revision,omitempty"`
}

// Ref is the "Kind/name" key for a record.
func (r Record) Ref() string { return r.Kind + "/" + r.Name }

// OriginOf classifies drift for a set of changed fields by asking whether the
// host still holds the values systemcd wrote.
func (r Record) OriginOf(observed map[string]string, fields []string) Origin {
	if len(r.Applied) == 0 {
		return OriginNew
	}
	for _, f := range fields {
		applied, tracked := r.Applied[f]
		if !tracked {
			// A field we never applied cannot have been changed out from
			// under us; it is new expectation from the repository.
			continue
		}
		if observed[f] != applied {
			return OriginExternal
		}
	}
	return OriginRepo
}

// Snapshot is the whole state file.
type Snapshot struct {
	Version   int               `json:"version"`
	Revision  string            `json:"revision,omitempty"`
	UpdatedAt time.Time         `json:"updatedAt"`
	Resources map[string]Record `json:"resources"`
	// History holds the revisions applied before the current one, newest
	// first, so `rollback` has somewhere to go.
	History []HistoryEntry `json:"history,omitempty"`
}

// HistoryEntry records one successful apply.
type HistoryEntry struct {
	Revision  string    `json:"revision"`
	AppliedAt time.Time `json:"appliedAt"`
	Changed   int       `json:"changed"`
}

// maxHistory bounds the rollback log.
const maxHistory = 20

// Store reads and writes the state file through a Host, so tests can exercise
// it without touching the real filesystem.
type Store struct {
	Host host.Host
	Path string
	// Warnf receives recoverable problems, such as a corrupt state file that
	// was restored from its backup.
	Warnf func(format string, args ...any)
}

// New returns a store at the default location.
func New(h host.Host) *Store { return &Store{Host: h, Path: paths.StateFile} }

func (s *Store) warn(format string, args ...any) {
	if s.Warnf != nil {
		s.Warnf(format, args...)
	}
}

// backupPath is the previous good state, kept so a truncated or corrupt write
// does not cost the operator every ownership record on the machine.
func (s *Store) backupPath() string { return s.Path + ".bak" }

// Load reads the snapshot, returning an empty one when no state exists yet.
//
// A corrupt state file falls back to the previous good copy rather than
// failing the run: losing ownership records silently is worse than a warning,
// and refusing to run at all is worse than both.
func (s *Store) Load() (*Snapshot, error) {
	snap, err := s.load(s.Path)
	if err == nil {
		return snap, nil
	}
	if errors.Is(err, host.ErrNotExist) {
		return emptySnapshot(), nil
	}

	var corrupt *corruptError
	if !errors.As(err, &corrupt) {
		return nil, err
	}

	backup, backupErr := s.load(s.backupPath())
	if backupErr != nil {
		if errors.Is(backupErr, host.ErrNotExist) {
			s.warn("state file %s is unreadable (%v) and no backup exists; starting from empty state, so previously applied resources will look unmanaged", s.Path, corrupt.cause)
			return emptySnapshot(), nil
		}
		return nil, fmt.Errorf("state file %s is corrupt (%w) and its backup is unusable: %v", s.Path, corrupt.cause, backupErr)
	}
	s.warn("state file %s is corrupt (%v); recovered the previous good copy from %s", s.Path, corrupt.cause, s.backupPath())
	return backup, nil
}

// corruptError marks a state file that exists but cannot be parsed.
type corruptError struct{ cause error }

func (e *corruptError) Error() string { return e.cause.Error() }
func (e *corruptError) Unwrap() error { return e.cause }

func (s *Store) load(path string) (*Snapshot, error) {
	data, err := s.Host.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, &corruptError{cause: err}
	}
	if snap.Version > Version {
		return nil, fmt.Errorf("state file %s was written by a newer systemcd (schema %d, this build understands %d); upgrade systemcd rather than downgrading the state", path, snap.Version, Version)
	}
	if snap.Resources == nil {
		snap.Resources = map[string]Record{}
	}
	migrate(&snap)
	return &snap, nil
}

// migrate brings an older snapshot up to the current schema.
func migrate(snap *Snapshot) {
	if snap.Version >= Version {
		return
	}
	// v1 had no ownership timestamps. Backfilling them from the file's own
	// UpdatedAt is the most honest guess available: it is when the record was
	// last written, and marking these as adopted would be a fabrication.
	for ref, rec := range snap.Resources {
		if rec.LastAppliedAt.IsZero() {
			rec.LastAppliedAt = snap.UpdatedAt
		}
		if rec.FirstAppliedAt.IsZero() {
			rec.FirstAppliedAt = snap.UpdatedAt
		}
		snap.Resources[ref] = rec
	}
	snap.Version = Version
}

func emptySnapshot() *Snapshot {
	return &Snapshot{Version: Version, Resources: map[string]Record{}}
}

// Save writes the snapshot, rotating the previous copy to .bak first.
func (s *Store) Save(snap *Snapshot) error {
	snap.Version = Version
	snap.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	if err := s.Host.MkdirAll(paths.DataDir, 0o700); err != nil {
		return err
	}

	// Rotate before overwriting so a crash mid-write leaves a recoverable
	// copy behind.
	if prev, err := s.Host.ReadFile(s.Path); err == nil {
		if err := s.Host.WriteFile(s.backupPath(), prev, 0o600); err != nil {
			s.warn("could not rotate state backup: %v", err)
		}
	} else if !errors.Is(err, host.ErrNotExist) {
		s.warn("could not read state for rotation: %v", err)
	}

	// State can contain hashes of sensitive config; keep it root-only, and
	// fsync the directory so the rename survives a power loss.
	return s.Host.WriteFileSync(s.Path, append(data, '\n'), 0o600)
}

// Lock takes the host-wide reconcile lock. Two systemcd processes converging
// the same machine at once would interleave writes and race each other's
// state, so the second one waits its turn or gives up.
func (s *Store) Lock() (func() error, error) {
	return s.Host.TryLock(paths.LockFile)
}

// RecordApply updates the snapshot after a successful reconcile.
func (snap *Snapshot) RecordApply(revision string, changed int) {
	if revision != "" && revision != snap.Revision {
		snap.History = append([]HistoryEntry{{
			Revision:  snap.Revision,
			AppliedAt: snap.UpdatedAt,
			Changed:   changed,
		}}, snap.History...)
		if len(snap.History) > maxHistory {
			snap.History = snap.History[:maxHistory]
		}
		snap.Revision = revision
	}
}

// PreviousRevision returns the most recent revision before the current one.
func (snap *Snapshot) PreviousRevision() (string, bool) {
	for _, h := range snap.History {
		if h.Revision != "" && h.Revision != snap.Revision {
			return h.Revision, true
		}
	}
	return "", false
}

// Refs lists the recorded resources, sorted.
func (snap *Snapshot) Refs() []string {
	out := make([]string, 0, len(snap.Resources))
	for ref := range snap.Resources {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

// Owner finds the resource that claims a physical thing, such as
// "path:/etc/nginx/nginx.conf". This is the reverse lookup an operator needs
// mid-incident.
func (snap *Snapshot) Owner(claim string) (Record, bool) {
	for _, ref := range snap.Refs() {
		rec := snap.Resources[ref]
		for _, c := range rec.Claims {
			if c == claim {
				return rec, true
			}
		}
	}
	return Record{}, false
}
