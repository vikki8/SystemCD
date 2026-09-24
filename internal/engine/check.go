package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/vikki8/systemcd/internal/manifest"
)

// Check validates a repository without contacting the host: every document
// must build into a resource whose spec passes its own validation, and the
// dependency graph must be acyclic.
//
// Targeting is deliberately ignored here so `systemcd validate` catches a
// broken manifest for web servers even when run on a database host.
func Check(repo *manifest.Repository) error {
	var resources []*manifest.Document
	var patches []*manifest.Document
	declared := map[string]bool{}

	for _, doc := range repo.Documents {
		if doc.Kind == manifest.PatchKind {
			patches = append(patches, doc)
			continue
		}
		resources = append(resources, doc)
		declared[doc.Ref()] = true
	}

	// A patch resolves against whichever documents its node selects, so
	// validation can only check that the target exists *somewhere* in the
	// repository. A patch aimed at nothing at all is always a mistake.
	var problems []string
	for _, p := range patches {
		var spec manifest.PatchSpec
		if err := p.DecodeSpec(&spec); err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if spec.Target == "" {
			problems = append(problems, fmt.Sprintf("%s: spec.target is required", p.Location()))
			continue
		}
		if !declared[spec.Target] {
			problems = append(problems, fmt.Sprintf("%s: patch targets %q, which no document in this repository declares", p.Location(), spec.Target))
		}
		// Edges a patch adds are dropped at plan time when they point outside
		// the working set, so a typo here would silently lose the ordering.
		for _, ref := range spec.DependsOn {
			if !declared[ref] {
				problems = append(problems, fmt.Sprintf("%s: dependsOn references unknown resource %q", p.Location(), ref))
			}
		}
		for _, ref := range spec.Notify {
			if !declared[ref] {
				problems = append(problems, fmt.Sprintf("%s: notify references unknown resource %q", p.Location(), ref))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("invalid repository:\n  - %s", strings.Join(problems, "\n  - "))
	}

	_, err := build(resources)
	return err
}
