package manifest

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// PatchKind names a document that structurally overrides part of another.
const PatchKind = "Patch"

// PatchSpec declares a structural override of one resource's spec.
//
// This is composition without templating. A base layer declares the resource;
// a role or a single host narrows it. Nothing is interpolated into a string,
// so the value in git is always the value on the machine:
//
//	kind: Patch
//	metadata:
//	  name: web01-nginx-workers
//	  targets:
//	    hosts: ["web01"]
//	spec:
//	  target: File/nginx-conf
//	  patch:
//	    mode: "0600"
//
// The alternative — `worker_processes {{ .cores }}` inside a config blob —
// is how every configuration tool eventually grows a second, worse programming
// language, and how operators stop being able to tell what is actually
// deployed. Transform structured data; do not interpolate strings.
type PatchSpec struct {
	// Target is the "Kind/name" of the document to modify.
	Target string `yaml:"target"`
	// Patch is merged over the target's spec: maps merge key by key, scalars
	// and sequences replace outright.
	Patch yaml.Node `yaml:"patch"`
	// Metadata optionally merges over the target's metadata, so a patch can
	// add labels without restating the resource.
	Metadata *PatchMetadata `yaml:"metadata,omitempty"`
	// DependsOn and Notify are appended to the target's existing edges rather
	// than replacing them: a patch that narrows a resource should not silently
	// drop ordering the base layer established.
	DependsOn []string `yaml:"dependsOn,omitempty"`
	Notify    []string `yaml:"notify,omitempty"`
}

// PatchMetadata is the subset of metadata a patch may override.
type PatchMetadata struct {
	Labels map[string]string `yaml:"labels,omitempty"`
}

// ApplyPatches merges every Patch document that targets this node into the
// documents it names, and removes the patches from the returned set.
//
// Patches are applied in load order, so a later layer wins — the same rule
// Kustomize uses, and the one people expect from "base, then role, then host".
func ApplyPatches(docs []*Document, node Node) ([]*Document, error) {
	var patches []*Document
	var out []*Document
	index := map[string]int{}

	for _, doc := range docs {
		if doc.Kind == PatchKind {
			patches = append(patches, doc)
			continue
		}
		index[doc.Ref()] = len(out)
		out = append(out, doc)
	}
	if len(patches) == 0 {
		return out, nil
	}

	// Patching copies the document first: the repository is loaded once and
	// may be selected for several nodes, and a patch scoped to web01 must not
	// leak into what db01 sees.
	copied := map[string]bool{}
	claim := func(ref string) (*Document, bool) {
		i, ok := index[ref]
		if !ok {
			return nil, false
		}
		if !copied[ref] {
			dup := *out[i]
			dup.DependsOn = append([]string(nil), out[i].DependsOn...)
			dup.Notify = append([]string(nil), out[i].Notify...)
			if out[i].Metadata.Labels != nil {
				labels := make(map[string]string, len(out[i].Metadata.Labels))
				for k, v := range out[i].Metadata.Labels {
					labels[k] = v
				}
				dup.Metadata.Labels = labels
			}
			out[i] = &dup
			copied[ref] = true
		}
		return out[i], true
	}

	var problems []string
	for _, p := range patches {
		if !p.Metadata.Targets.Matches(node) {
			continue
		}
		var spec PatchSpec
		if err := p.DecodeSpec(&spec); err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if spec.Target == "" {
			problems = append(problems, fmt.Sprintf("%s: spec.target is required", p.Location()))
			continue
		}
		target, ok := claim(spec.Target)
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"%s: patch targets %q, which is not declared for this host. "+
					"A patch cannot create a resource, only narrow one that a base layer already declares",
				p.Location(), spec.Target))
			continue
		}
		if err := mergeInto(target, spec); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", p.Location(), err))
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("invalid patches:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return out, nil
}

func mergeInto(target *Document, spec PatchSpec) error {
	if !spec.Patch.IsZero() {
		merged, err := mergeNodes(&target.Spec, &spec.Patch)
		if err != nil {
			return err
		}
		target.Spec = *merged
	}
	if spec.Metadata != nil && len(spec.Metadata.Labels) > 0 {
		if target.Metadata.Labels == nil {
			target.Metadata.Labels = map[string]string{}
		}
		for k, v := range spec.Metadata.Labels {
			target.Metadata.Labels[k] = v
		}
	}
	target.DependsOn = appendUnique(target.DependsOn, spec.DependsOn)
	target.Notify = appendUnique(target.Notify, spec.Notify)
	return nil
}

// mergeNodes deep-merges patch over base.
//
// Mappings merge key by key so a patch can set one field without restating the
// resource. Scalars and sequences replace outright: there is no defensible
// universal merge for an unkeyed list, and quietly appending would make
// `groups: [docker]` mean something different in a patch than in a base.
func mergeNodes(base, patch *yaml.Node) (*yaml.Node, error) {
	base = unwrapDocument(base)
	patch = unwrapDocument(patch)

	if base == nil || base.Kind == 0 {
		return patch, nil
	}
	if patch == nil || patch.Kind == 0 {
		return base, nil
	}
	if base.Kind != yaml.MappingNode || patch.Kind != yaml.MappingNode {
		return patch, nil
	}

	merged := *base
	merged.Content = append([]*yaml.Node(nil), base.Content...)

	for i := 0; i+1 < len(patch.Content); i += 2 {
		key, value := patch.Content[i], patch.Content[i+1]
		replaced := false
		for j := 0; j+1 < len(merged.Content); j += 2 {
			if merged.Content[j].Value != key.Value {
				continue
			}
			// An explicit null removes the field, which is how a patch says
			// "stop managing this attribute" without a special keyword.
			if value.Tag == "!!null" {
				merged.Content = append(merged.Content[:j], merged.Content[j+2:]...)
			} else {
				sub, err := mergeNodes(merged.Content[j+1], value)
				if err != nil {
					return nil, err
				}
				merged.Content[j+1] = sub
			}
			replaced = true
			break
		}
		if !replaced && value.Tag != "!!null" {
			merged.Content = append(merged.Content, key, value)
		}
	}
	return &merged, nil
}

func unwrapDocument(n *yaml.Node) *yaml.Node {
	if n != nil && n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		return n.Content[0]
	}
	return n
}

func appendUnique(existing, extra []string) []string {
	if len(extra) == 0 {
		return existing
	}
	seen := map[string]bool{}
	for _, e := range existing {
		seen[e] = true
	}
	for _, e := range extra {
		if !seen[e] {
			existing = append(existing, e)
			seen[e] = true
		}
	}
	return existing
}
