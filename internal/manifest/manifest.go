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
	"unicode"

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
	spec := &d.Spec
	if hasAlias(spec) {
		// Encoded on its own, an alias whose anchor lives outside the spec
		// (`name: &n nginx` in metadata, `*n` in spec) no longer resolves.
		expanded, err := expandAliases(spec, new(int))
		if err != nil {
			return fmt.Errorf("%s: spec: %w", d.Location(), err)
		}
		spec = expanded
	}
	raw, err := yaml.Marshal(spec)
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

// maxAliasExpansion bounds how many nodes expanding a spec's aliases may
// produce, so a pathological chain of aliases fails instead of exhausting
// memory.
const maxAliasExpansion = 100_000

func hasAlias(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	if n.Kind == yaml.AliasNode {
		return true
	}
	for _, c := range n.Content {
		if hasAlias(c) {
			return true
		}
	}
	return false
}

// expandAliases returns a copy of n in which every alias is replaced by a copy
// of the node it refers to, and no anchors remain.
func expandAliases(n *yaml.Node, count *int) (*yaml.Node, error) {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	*count++
	if *count > maxAliasExpansion {
		return nil, errors.New("aliases expand to too many nodes")
	}
	dup := *n
	dup.Anchor = ""
	dup.Content = nil
	for _, c := range n.Content {
		e, err := expandAliases(c, count)
		if err != nil {
			return nil, err
		}
		dup.Content = append(dup.Content, e)
	}
	return &dup, nil
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

// SelectFor returns the documents that target the given node, in load order,
// with any Patch documents merged into the resources they name.
//
// Errors from patching are surfaced through SelectForE; this form keeps the
// common call site simple by returning the unpatched set on failure, which
// then fails loudly at build time rather than silently applying half a patch.
func (r *Repository) SelectFor(n Node) []*Document {
	docs, err := r.SelectForE(n)
	if err != nil {
		return r.targeted(n)
	}
	return docs
}

// SelectForE is SelectFor with the patch error surfaced.
func (r *Repository) SelectForE(n Node) ([]*Document, error) {
	declared := map[string]bool{}
	for _, d := range r.Documents {
		if d.Kind != PatchKind {
			declared[d.Ref()] = true
		}
	}
	return applyPatches(r.targeted(n), n, declared)
}

// targeted returns the documents, patches included, that select n.
func (r *Repository) targeted(n Node) []*Document {
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
	patches := map[string]*Document{}

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
		if err := checkName(d.Metadata.Name); err != nil {
			problems = append(problems, fmt.Sprintf("%s: metadata.name %q %v", d.Location(), d.Metadata.Name, err))
			continue
		}
		if d.Metadata.Targets != nil {
			for _, pattern := range d.Metadata.Targets.Hosts {
				// A malformed glob matches no hostname at all, which would
				// quietly drop the document from every host.
				if _, err := filepath.Match(pattern, ""); err != nil {
					problems = append(problems, fmt.Sprintf("%s: metadata.targets.hosts: %q is not a valid glob: %v", d.Location(), pattern, err))
				}
			}
		}
		// A patch's target is resolved per node, since a patch and the
		// resource it narrows can be scoped differently; patches are kept
		// apart so nothing can depend on one.
		declared := seen
		if d.Kind == PatchKind {
			declared = patches
		}
		if prev, dup := declared[d.Ref()]; dup {
			problems = append(problems, fmt.Sprintf("%s: duplicate resource %s, already declared at %s", d.Location(), d.Ref(), prev.Location()))
			continue
		}
		declared[d.Ref()] = d
	}

	for _, d := range r.Documents {
		if d.Kind == PatchKind {
			problems = append(problems, validatePatch(d, seen)...)
			continue
		}
		problems = append(problems, unknownRefs(d, "dependsOn", d.DependsOn, seen)...)
		problems = append(problems, unknownRefs(d, "notify", d.Notify, seen)...)
	}

	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("invalid repository:\n  - %s", strings.Join(problems, "\n  - "))
}

// checkName rejects names that cannot serve as one path component. A name
// ends up in "Kind/name" references and in file names (a Sysctl's drop-in is
// 60-systemcd-<name>.conf), so "../../tmp/x" would write outside the
// directory it belongs in.
func checkName(name string) error {
	if strings.Contains(name, "/") || strings.Contains(name, "..") {
		return errors.New(`must not contain "/" or ".."`)
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("must not contain whitespace or control characters")
		}
	}
	return nil
}

func unknownRefs(d *Document, field string, refs []string, declared map[string]*Document) []string {
	var problems []string
	for _, ref := range refs {
		if _, ok := declared[ref]; !ok {
			problems = append(problems, fmt.Sprintf("%s: %s references unknown resource %q", d.Location(), field, ref))
		}
	}
	return problems
}

// validatePatch checks what a patch can be checked for without a node: that
// its target and the edges it appends exist somewhere in the repository.
// Those edges are otherwise dropped silently at reconcile time, where a
// reference outside the working set is indistinguishable from host targeting.
func validatePatch(d *Document, declared map[string]*Document) []string {
	var problems []string
	if len(d.DependsOn) > 0 || len(d.Notify) > 0 {
		problems = append(problems, fmt.Sprintf(
			"%s: dependsOn and notify on a Patch document itself are ignored; put them under spec to append them to the target", d.Location()))
	}
	var spec PatchSpec
	if err := d.DecodeSpec(&spec); err != nil {
		return append(problems, err.Error())
	}
	if spec.Target == "" {
		return append(problems, fmt.Sprintf("%s: spec.target is required", d.Location()))
	}
	if err := checkPatchNode(&spec.Patch); err != nil {
		problems = append(problems, fmt.Sprintf("%s: %v", d.Location(), err))
	}
	if _, ok := declared[spec.Target]; !ok {
		problems = append(problems, fmt.Sprintf("%s: patch targets %q, which no document in this repository declares", d.Location(), spec.Target))
	}
	problems = append(problems, unknownRefs(d, "spec.dependsOn", spec.DependsOn, declared)...)
	problems = append(problems, unknownRefs(d, "spec.notify", spec.Notify, declared)...)
	return problems
}
