// Package resource defines the provider contract and the built-in resource
// kinds systemcd can reconcile on a Linux host.
package resource

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/vikki8/systemcd/internal/host"
	"github.com/vikki8/systemcd/internal/manifest"
)

// ID identifies a resource within a repository.
type ID struct {
	Kind string
	Name string
}

func (id ID) String() string { return id.Kind + "/" + id.Name }

// ParseID splits a "Kind/name" reference.
func ParseID(ref string) (ID, error) {
	kind, name, ok := strings.Cut(ref, "/")
	if !ok || kind == "" || name == "" {
		return ID{}, fmt.Errorf("invalid resource reference %q, want Kind/name", ref)
	}
	return ID{Kind: kind, Name: name}, nil
}

// State is a resource's observable configuration, flattened to strings so the
// engine can diff and render any kind uniformly. Keys beginning with "_" are
// informational: shown in output but never compared.
type State map[string]string

// Informational marks a key as report-only.
func Informational(key string) bool { return strings.HasPrefix(key, "_") }

// FieldDiff is one field that differs between desired and observed state.
type FieldDiff struct {
	Field string
	Have  string
	Want  string
	// Sensitive hides values in rendered output.
	Sensitive bool
}

// Diff is the set of fields that must change for a resource to converge.
type Diff []FieldDiff

// Empty reports whether the resource is already in sync.
func (d Diff) Empty() bool { return len(d) == 0 }

// Has reports whether the diff touches the named field.
func (d Diff) Has(field string) bool {
	for _, f := range d {
		if f.Field == field {
			return true
		}
	}
	return false
}

// Want returns the desired value for a field, and whether it is in the diff.
func (d Diff) Want(field string) (string, bool) {
	for _, f := range d {
		if f.Field == field {
			return f.Want, true
		}
	}
	return "", false
}

// Compare produces the diff between desired and observed state. Only keys
// present in desired are compared, so a provider can report extra observed
// detail without forcing it to be declared in the manifest.
func Compare(desired, observed State, sensitive map[string]bool) Diff {
	keys := make([]string, 0, len(desired))
	for k := range desired {
		if !Informational(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	var out Diff
	for _, k := range keys {
		have, ok := observed[k]
		if !ok {
			have = "<absent>"
		}
		if have == desired[k] {
			continue
		}
		out = append(out, FieldDiff{Field: k, Have: have, Want: desired[k], Sensitive: sensitive[k]})
	}
	return out
}

// Context carries everything a provider needs to observe or change a host.
type Context struct {
	Ctx  context.Context
	Host host.Host
	// RepoRoot resolves manifest-relative asset paths (File.source).
	RepoRoot string
	// DryRun is set during `plan`; providers must not mutate the host.
	DryRun bool
	// Node describes the machine, for template-free host targeting.
	Node manifest.Node
	// Logf receives provider progress messages.
	Logf func(format string, args ...any)
}

// Log emits a provider message if a logger is attached.
func (c *Context) Log(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// Resource is the contract every kind implements.
type Resource interface {
	ID() ID
	// Validate checks the spec in isolation, before any host access.
	Validate() error
	// Desired returns the state the manifest asks for.
	Desired(c *Context) (State, error)
	// Observe returns the state currently on the host.
	Observe(c *Context) (State, error)
	// Apply converges the resource. It is given the diff that Compare
	// produced so providers can make the minimum necessary change.
	Apply(c *Context, d Diff) error
}

// Sensitiver lets a kind mark fields whose values must not be printed.
type Sensitiver interface {
	SensitiveFields() map[string]bool
}

// Refreshable is implemented by kinds that respond to `notify` — typically by
// restarting or reloading. Refresh runs at most once per reconcile, after the
// resource's own Apply.
type Refreshable interface {
	Refresh(c *Context) error
}

// Deletable is implemented by kinds that can be pruned when they disappear
// from the repository.
type Deletable interface {
	Delete(c *Context) error
}

// HealthStatus is the post-apply verdict for a resource.
type HealthStatus string

const (
	HealthHealthy  HealthStatus = "Healthy"
	HealthDegraded HealthStatus = "Degraded"
	HealthUnknown  HealthStatus = "Unknown"
)

// Checker is implemented by kinds that can report health after apply, which
// is what makes `apply --rollback-on-failure` meaningful.
type Checker interface {
	Health(c *Context) (HealthStatus, string, error)
}

// Builder constructs a resource from its manifest document.
type Builder func(doc *manifest.Document) (Resource, error)

var registry = map[string]Builder{}

// Register makes a kind available to the loader. It panics on duplicate
// registration, which can only be a programming error.
func Register(kind string, b Builder) {
	if _, dup := registry[kind]; dup {
		panic("resource: duplicate registration for kind " + kind)
	}
	registry[kind] = b
}

// Kinds lists every registered kind, sorted.
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Build turns a document into a resource.
func Build(doc *manifest.Document) (Resource, error) {
	b, ok := registry[doc.Kind]
	if !ok {
		return nil, fmt.Errorf("%s: unknown kind %q (known kinds: %s)", doc.Location(), doc.Kind, strings.Join(Kinds(), ", "))
	}
	r, err := b(doc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", doc.Location(), err)
	}
	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", doc.Location(), err)
	}
	return r, nil
}

// Presence is the conventional value of the "state" field.
const (
	Present = "present"
	Absent  = "absent"
)

// normalizePresence validates a user-supplied state field, defaulting to
// present when empty.
func normalizePresence(s string) (string, error) {
	switch s {
	case "", Present:
		return Present, nil
	case Absent:
		return Absent, nil
	default:
		return "", fmt.Errorf("state must be %q or %q, got %q", Present, Absent, s)
	}
}
