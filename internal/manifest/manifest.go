// Package manifest loads and validates the declarative documents that make up
// a systemcd repository.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// APIVersion is the only document version this build understands.
const APIVersion = "systemcd.dev/v1"

// ConfigKind names the repository-level settings document.
const ConfigKind = "Config"

// Metadata is common to every document.
type Metadata struct {
	Name    string            `yaml:"name"`
	Labels  map[string]string `yaml:"labels,omitempty"`
	Targets *Targets          `yaml:"targets,omitempty"`
}

// Targets restricts a document to a subset of the fleet. A nil Targets means
// "every host". Within a Targets, Hosts and Labels are ANDed; entries inside
// Hosts are ORed.
type Targets struct {
	Hosts  []string          `yaml:"hosts,omitempty"`
	Labels map[string]string `yaml:"labels,omitempty"`
}

// Node describes the machine being reconciled, for target matching.
type Node struct {
	Hostname string
	Labels   map[string]string
}

// Matches reports whether t selects the given node.
func (t *Targets) Matches(n Node) bool {
	if t == nil {
		return true
	}
	if len(t.Hosts) > 0 {
		matched := false
		for _, pattern := range t.Hosts {
			if ok, _ := filepath.Match(pattern, n.Hostname); ok {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for k, want := range t.Labels {
		if n.Labels[k] != want {
			return false
		}
	}
	return true
}

// Document is one parsed YAML document from the repository.
type Document struct {
	APIVersion string    `yaml:"apiVersion"`
	Kind       string    `yaml:"kind"`
	Metadata   Metadata  `yaml:"metadata"`
	Spec       yaml.Node `yaml:"spec"`
	DependsOn  []string  `yaml:"dependsOn,omitempty"`
	Notify     []string  `yaml:"notify,omitempty"`

	// Source records where the document was read from, for error messages.
	Source string `yaml:"-"`
	// Index is the document's position within its source file.
	Index int `yaml:"-"`
}

// Ref is the canonical "Kind/name" identifier for a document.
func (d *Document) Ref() string { return d.Kind + "/" + d.Metadata.Name }

// Location renders a human-friendly origin such as "manifests/web.yaml[1]".
func (d *Document) Location() string {
	if d.Source == "" {
		return "<inline>"
	}
	return fmt.Sprintf("%s[%d]", d.Source, d.Index)
}

// DecodeSpec unmarshals the document's spec into out, rejecting unknown fields
// so typos in a manifest surface as errors instead of silent no-ops.
//
// yaml.Node.Decode has no strict mode, so the node is re-encoded and fed
// through a decoder that does.
func (d *Document) DecodeSpec(out any) error {
	if d.Spec.IsZero() {
		return nil
	}
	raw, err := yaml.Marshal(&d.Spec)
	if err != nil {
		return fmt.Errorf("%s: spec: %w", d.Location(), err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: spec: %w", d.Location(), err)
	}
	return nil
}

// Config holds repository-level settings, supplied by a Config document and
// overridable from the command line.
type Config struct {
	// Paths lists directories or files to load, relative to the repo root.
	// Empty means "the whole repo".
	Paths []string `yaml:"paths,omitempty"`
	// Prune removes resources that were applied previously but no longer
	// appear in the repository.
	Prune bool `yaml:"prune,omitempty"`
	// SelfHeal lets the agent reapply on drift instead of only reporting it.
	SelfHeal bool `yaml:"selfHeal,omitempty"`
	// Interval is the agent's reconcile period, e.g. "5m".
	Interval string `yaml:"interval,omitempty"`
	// Repo and Branch configure the git source for `sync` and `agent`.
	Repo   string `yaml:"repo,omitempty"`
	Branch string `yaml:"branch,omitempty"`
	// Labels are node labels declared in-repo; command-line labels win.
	Labels map[string]string `yaml:"labels,omitempty"`
}

// DefaultConfig returns the settings used when a repo declares no Config.
func DefaultConfig() Config {
	return Config{Prune: false, SelfHeal: true, Interval: "5m", Branch: "main"}
}

// Repository is a fully loaded set of documents plus repo settings.
type Repository struct {
	Root      string
	Config    Config
	Documents []*Document
}

// SelectFor returns the documents that target the given node, in load order.
func (r *Repository) SelectFor(n Node) []*Document {
	var out []*Document
	for _, d := range r.Documents {
		if d.Metadata.Targets.Matches(n) {
			out = append(out, d)
		}
	}
	return out
}

// Validate checks structural invariants that hold regardless of kind:
// api version, required names, unique refs, and resolvable references.
func (r *Repository) Validate() error {
	var problems []string
	seen := map[string]*Document{}

	for _, d := range r.Documents {
		if d.APIVersion != APIVersion {
			problems = append(problems, fmt.Sprintf("%s: apiVersion %q is not supported (want %q)", d.Location(), d.APIVersion, APIVersion))
		}
		if d.Kind == "" {
			problems = append(problems, fmt.Sprintf("%s: kind is required", d.Location()))
			continue
		}
		if d.Metadata.Name == "" {
			problems = append(problems, fmt.Sprintf("%s: metadata.name is required", d.Location()))
			continue
		}
		if prev, dup := seen[d.Ref()]; dup {
			problems = append(problems, fmt.Sprintf("%s: duplicate resource %s, already declared at %s", d.Location(), d.Ref(), prev.Location()))
			continue
		}
		seen[d.Ref()] = d
	}

	for _, d := range r.Documents {
		for _, ref := range d.DependsOn {
			if _, ok := seen[ref]; !ok {
				problems = append(problems, fmt.Sprintf("%s: dependsOn references unknown resource %q", d.Location(), ref))
			}
		}
		for _, ref := range d.Notify {
			if _, ok := seen[ref]; !ok {
				problems = append(problems, fmt.Sprintf("%s: notify references unknown resource %q", d.Location(), ref))
			}
		}
	}

	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("invalid repository:\n  - %s", strings.Join(problems, "\n  - "))
}
