// Package state persists the last successfully applied configuration.
//
// This record is what turns a two-way comparison (git vs host) into a
// three-way one. Without it, a resource deleted from the repository is simply
// invisible; with it, systemcd knows the resource used to exist and can prune.
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
const Version = 1

// Record is the last applied state of one resource.
type Record struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Manifest is the YAML document that produced the resource, kept so a
	// pruned resource can be rebuilt and asked to delete itself.
	Manifest string `json:"manifest"`
	// Desired is the state that was applied, for drift attribution.
	Desired   map[string]string `json:"desired,omitempty"`
	AppliedAt time.Time         `json:"appliedAt"`
	// Revision is the git revision that introduced this state.
	Revision string `json:"revision,omitempty"`
}

// Ref is the "Kind/name" key for a record.
func (r Record) Ref() string { return r.Kind + "/" + r.Name }

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
}

// New returns a store at the default location.
func New(h host.Host) *Store { return &Store{Host: h, Path: paths.StateFile} }

// Load reads the snapshot, returning an empty one when no state exists yet.
func (s *Store) Load() (*Snapshot, error) {
	data, err := s.Host.ReadFile(s.Path)
	if errors.Is(err, host.ErrNotExist) {
		return &Snapshot{Version: Version, Resources: map[string]Record{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state %s: %w", s.Path, err)
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", s.Path, err)
	}
	if snap.Version > Version {
		return nil, fmt.Errorf("state file %s was written by a newer systemcd (schema %d, this build understands %d)", s.Path, snap.Version, Version)
	}
	if snap.Resources == nil {
		snap.Resources = map[string]Record{}
	}
	return &snap, nil
}

// Save writes the snapshot atomically.
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
	// State can contain hashes of sensitive config; keep it root-only.
	return s.Host.WriteFile(s.Path, append(data, '\n'), 0o600)
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
