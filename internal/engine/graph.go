package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/vikki8/systemcd/internal/manifest"
	"github.com/vikki8/systemcd/internal/resource"
)

// node couples a resource with its manifest document and edges.
type node struct {
	doc       *manifest.Document
	res       resource.Resource
	id        resource.ID
	dependsOn []resource.ID
	notifies  []resource.ID
	// order is the document's position in the repository, used to keep the
	// topological sort stable and predictable.
	order int
}

// graph is the dependency DAG for one reconcile.
type graph struct {
	nodes map[resource.ID]*node
	// sorted is the topological order Apply walks.
	sorted []*node
}

// build constructs the graph from documents, resolving edges and ordering.
//
// A `notify` edge implies ordering: the notifier must be reconciled before the
// resource it wakes, otherwise a config change could restart a service before
// the new config is on disk.
func build(docs []*manifest.Document) (*graph, error) {
	return buildFor(docs, nil)
}

// buildFor is build for a set of documents that need not all target one
// host. disjoint, when set, reports two documents that can never be selected
// for the same host; their claims cannot conflict.
func buildFor(docs []*manifest.Document, disjoint func(a, b *manifest.Document) bool) (*graph, error) {
	g := &graph{nodes: map[resource.ID]*node{}}

	for i, doc := range docs {
		res, err := resource.Build(doc)
		if err != nil {
			return nil, err
		}
		id := res.ID()
		if _, dup := g.nodes[id]; dup {
			return nil, fmt.Errorf("%s: duplicate resource %s", doc.Location(), id)
		}
		n := &node{doc: doc, res: res, id: id, order: i}
		for _, ref := range doc.DependsOn {
			depID, err := resource.ParseID(ref)
			if err != nil {
				return nil, fmt.Errorf("%s: dependsOn: %w", doc.Location(), err)
			}
			n.dependsOn = append(n.dependsOn, depID)
		}
		for _, ref := range doc.Notify {
			target, err := resource.ParseID(ref)
			if err != nil {
				return nil, fmt.Errorf("%s: notify: %w", doc.Location(), err)
			}
			n.notifies = append(n.notifies, target)
		}
		g.nodes[id] = n
	}

	if err := g.checkClaims(disjoint); err != nil {
		return nil, err
	}

	// Edges pointing outside the working set are dropped rather than
	// rejected: host targeting and --only legitimately narrow the set, and a
	// reference that is unresolvable across the *whole* repository is caught
	// by manifest.Repository.Validate before we ever get here.
	for _, n := range g.nodes {
		n.dependsOn = g.resolvable(n.dependsOn)
		n.notifies = g.resolvable(n.notifies)
	}

	sorted, err := g.topoSort()
	if err != nil {
		return nil, err
	}
	g.sorted = sorted
	return g, nil
}

// checkClaims refuses a repository in which two resources would take
// ownership of the same thing.
//
// This is caught before anything is observed, let alone applied: two File
// resources pointing at one path do not "mostly work", they produce a machine
// whose contents depend on document order, and last-writer-wins is not a
// property anyone can reason about at 3am.
func (g *graph) checkClaims(disjoint func(a, b *manifest.Document) bool) error {
	type holder struct {
		id  resource.ID
		loc string
		doc *manifest.Document
	}
	owners := map[string][]holder{}

	// Sort for a deterministic error message regardless of map iteration.
	ids := make([]resource.ID, 0, len(g.nodes))
	for id := range g.nodes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return g.nodes[ids[i]].order < g.nodes[ids[j]].order })

	var conflicts []string
	for _, id := range ids {
		n := g.nodes[id]
		for _, claim := range resource.ClaimsOf(n.res) {
			key := claim.String()
			clash := false
			for _, prev := range owners[key] {
				if disjoint != nil && disjoint(prev.doc, n.doc) {
					continue
				}
				conflicts = append(conflicts, fmt.Sprintf(
					"%s and %s both claim %s (declared at %s and %s)",
					prev.id, id, key, prev.loc, n.doc.Location()))
				clash = true
				break
			}
			if !clash {
				owners[key] = append(owners[key], holder{id: id, loc: n.doc.Location(), doc: n.doc})
			}
		}
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("conflicting ownership:\n  - %s", strings.Join(conflicts, "\n  - "))
	}
	return nil
}

// resolvable keeps only the references present in this graph.
func (g *graph) resolvable(ids []resource.ID) []resource.ID {
	var out []resource.ID
	for _, id := range ids {
		if _, ok := g.nodes[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

// predecessors returns, for each node, the set of nodes that must run first.
func (g *graph) predecessors() map[resource.ID]map[resource.ID]bool {
	preds := map[resource.ID]map[resource.ID]bool{}
	for id := range g.nodes {
		preds[id] = map[resource.ID]bool{}
	}
	for id, n := range g.nodes {
		for _, dep := range n.dependsOn {
			preds[id][dep] = true
		}
		for _, target := range n.notifies {
			preds[target][id] = true
		}
	}
	return preds
}

// topoSort returns a deterministic dependency order (Kahn's algorithm, with
// ties broken by document order so plans are reproducible).
func (g *graph) topoSort() ([]*node, error) {
	preds := g.predecessors()
	remaining := make(map[resource.ID]int, len(g.nodes))
	for id, p := range preds {
		remaining[id] = len(p)
	}

	successors := map[resource.ID][]resource.ID{}
	for id, p := range preds {
		for dep := range p {
			successors[dep] = append(successors[dep], id)
		}
	}

	var ready []*node
	for id, count := range remaining {
		if count == 0 {
			ready = append(ready, g.nodes[id])
		}
	}
	sortNodes(ready)

	var out []*node
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		out = append(out, n)

		var unlocked []*node
		for _, succ := range successors[n.id] {
			remaining[succ]--
			if remaining[succ] == 0 {
				unlocked = append(unlocked, g.nodes[succ])
			}
		}
		if len(unlocked) > 0 {
			ready = append(ready, unlocked...)
			sortNodes(ready)
		}
	}

	if len(out) != len(g.nodes) {
		return nil, fmt.Errorf("dependency cycle detected among: %s", strings.Join(cycleMembers(remaining), ", "))
	}
	return out, nil
}

func sortNodes(ns []*node) {
	sort.SliceStable(ns, func(i, j int) bool { return ns[i].order < ns[j].order })
}

// cycleMembers lists the resources still blocked when the sort stalls.
func cycleMembers(remaining map[resource.ID]int) []string {
	var stuck []string
	for id, count := range remaining {
		if count > 0 {
			stuck = append(stuck, id.String())
		}
	}
	sort.Strings(stuck)
	return stuck
}
