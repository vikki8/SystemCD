// Package engine reconciles a repository of desired state against a host.
package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vikki8/systemcd/internal/host"
	"github.com/vikki8/systemcd/internal/manifest"
	"github.com/vikki8/systemcd/internal/resource"
	"github.com/vikki8/systemcd/internal/state"
	"gopkg.in/yaml.v3"
)

// Action classifies what a reconcile did (or would do) to a resource.
type Action string

const (
	ActionNoop    Action = "in-sync"
	ActionCreate  Action = "create"
	ActionUpdate  Action = "update"
	ActionDelete  Action = "delete"
	ActionPrune   Action = "prune"
	ActionRun     Action = "run"
	ActionRefresh Action = "refresh"
	ActionSkip    Action = "skipped"
	ActionError   Action = "error"
)

// Changed reports whether the action represents a modification to the host.
func (a Action) Changed() bool {
	switch a {
	case ActionCreate, ActionUpdate, ActionDelete, ActionPrune, ActionRun, ActionRefresh:
		return true
	}
	return false
}

// Result is the outcome for one resource.
type Result struct {
	ID       resource.ID
	Action   Action
	Diff     resource.Diff
	Err      error
	Notified bool
	Health   resource.HealthStatus
	// HealthDetail explains a non-healthy verdict.
	HealthDetail string
	Messages     []string
	Duration     time.Duration
	// Source is the manifest location, for error reporting.
	Source string
}

// OutOfSync reports whether the resource differed from the manifest.
func (r Result) OutOfSync() bool {
	return r.Action != ActionNoop && r.Action != ActionSkip
}

// Report is the outcome of a whole reconcile.
type Report struct {
	Revision string
	DryRun   bool
	Results  []Result
	Started  time.Time
	Finished time.Time
}

// Counts summarizes a report.
type Counts struct {
	Total    int
	InSync   int
	Changed  int
	Failed   int
	Skipped  int
	Degraded int
}

// Counts tallies the report.
func (r *Report) Counts() Counts {
	var c Counts
	for _, res := range r.Results {
		c.Total++
		switch {
		case res.Err != nil:
			c.Failed++
		case res.Action == ActionSkip:
			c.Skipped++
		case res.Action == ActionNoop:
			c.InSync++
		default:
			c.Changed++
		}
		if res.Health == resource.HealthDegraded {
			c.Degraded++
		}
	}
	return c
}

// OutOfSync reports whether anything differed from the desired state.
func (r *Report) OutOfSync() bool {
	for _, res := range r.Results {
		if res.OutOfSync() {
			return true
		}
	}
	return false
}

// Err aggregates resource failures into a single error, or nil.
func (r *Report) Err() error {
	var failed []string
	for _, res := range r.Results {
		if res.Err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", res.ID, res.Err))
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("%d resource(s) failed:\n  - %s", len(failed), strings.Join(failed, "\n  - "))
}

// Degraded lists resources whose post-apply health check failed.
func (r *Report) Degraded() []Result {
	var out []Result
	for _, res := range r.Results {
		if res.Health == resource.HealthDegraded {
			out = append(out, res)
		}
	}
	return out
}

// Options configures a reconcile.
type Options struct {
	Repo *manifest.Repository
	Host host.Host
	Node manifest.Node
	// DryRun plans without touching the host.
	DryRun bool
	// Prune deletes resources recorded in state but absent from the repo.
	Prune bool
	// Revision labels the applied state, normally a git commit.
	Revision string
	// Only restricts the run to matching resources ("Service/nginx",
	// "File/*", or a bare kind). Empty means everything.
	Only []string
	// Store persists applied state; nil disables persistence.
	Store *state.Store
	// Logf receives progress messages.
	Logf func(format string, args ...any)
	// HealthChecks runs post-apply health verification.
	HealthChecks bool
}

// Reconcile plans, and unless DryRun is set, applies the repository.
func Reconcile(ctx context.Context, opts Options) (*Report, error) {
	if opts.Repo == nil {
		return nil, errors.New("engine: no repository provided")
	}
	if opts.Host == nil {
		return nil, errors.New("engine: no host provided")
	}

	docs := opts.Repo.SelectFor(opts.Node)
	docs, err := filterDocs(docs, opts.Only)
	if err != nil {
		return nil, err
	}

	g, err := build(docs)
	if err != nil {
		return nil, err
	}

	rc := &Context{
		resource: &resource.Context{
			Ctx:      ctx,
			Host:     opts.Host,
			RepoRoot: opts.Repo.Root,
			DryRun:   opts.DryRun,
			Node:     opts.Node,
		},
		opts: opts,
	}

	report := &Report{Revision: opts.Revision, DryRun: opts.DryRun, Started: time.Now()}

	var snap *state.Snapshot
	if opts.Store != nil {
		snap, err = opts.Store.Load()
		if err != nil {
			return nil, err
		}
	} else {
		snap = &state.Snapshot{Resources: map[string]state.Record{}}
	}

	preds := g.predecessors()
	blocked := map[resource.ID]string{}
	notified := map[resource.ID]bool{}
	changedThisRun := map[resource.ID]bool{}

	for _, n := range g.sorted {
		if reason, isBlocked := blocked[n.id]; isBlocked {
			report.Results = append(report.Results, Result{
				ID: n.id, Action: ActionSkip, Source: n.doc.Location(),
				Messages: []string{reason},
			})
			blockDependents(g, preds, n.id, blocked, fmt.Sprintf("depends on skipped %s", n.id))
			continue
		}

		res := rc.reconcileOne(n, notified[n.id])
		report.Results = append(report.Results, res)

		if res.Err != nil {
			blockDependents(g, preds, n.id, blocked, fmt.Sprintf("depends on failed %s", n.id))
			continue
		}
		if res.Action.Changed() {
			changedThisRun[n.id] = true
			// Notifications propagate during a dry run too, so a plan shows
			// the restart that a config change would trigger.
			for _, target := range n.notifies {
				notified[target] = true
			}
		}
		if !opts.DryRun && res.Err == nil {
			recordState(snap, n, res, opts.Revision)
		}
	}

	if opts.Prune {
		pruned := rc.prune(g, snap)
		report.Results = append(report.Results, pruned...)
	}

	if opts.HealthChecks {
		rc.checkHealth(g, report, changedThisRun)
	}

	report.Finished = time.Now()

	if !opts.DryRun && opts.Store != nil {
		snap.RecordApply(opts.Revision, report.Counts().Changed)
		if err := opts.Store.Save(snap); err != nil {
			return report, fmt.Errorf("save state: %w", err)
		}
	}
	return report, nil
}

// Context threads shared state through a single reconcile.
type Context struct {
	resource *resource.Context
	opts     Options
}

func (rc *Context) reconcileOne(n *node, wasNotified bool) Result {
	start := time.Now()
	out := Result{ID: n.id, Action: ActionNoop, Source: n.doc.Location(), Notified: wasNotified}

	var messages []string
	rc.resource.Logf = func(format string, args ...any) {
		messages = append(messages, fmt.Sprintf(format, args...))
		if rc.opts.Logf != nil {
			rc.opts.Logf("  %s: "+format, append([]any{n.id}, args...)...)
		}
	}
	defer func() {
		out.Messages = messages
		out.Duration = time.Since(start)
	}()

	desired, err := n.res.Desired(rc.resource)
	if err != nil {
		out.Action, out.Err = ActionError, fmt.Errorf("desired state: %w", err)
		return out
	}
	observed, err := n.res.Observe(rc.resource)
	if err != nil {
		out.Action, out.Err = ActionError, fmt.Errorf("observe: %w", err)
		return out
	}

	var sensitive map[string]bool
	if s, ok := n.res.(resource.Sensitiver); ok {
		sensitive = s.SensitiveFields()
	}
	diff := resource.Compare(desired, observed, sensitive)
	out.Diff = diff
	out.Action = classify(diff)

	if diff.Empty() && !wasNotified {
		return out
	}

	if rc.opts.DryRun {
		if diff.Empty() && wasNotified {
			out.Action = ActionRefresh
		}
		return out
	}

	if !diff.Empty() {
		if err := n.res.Apply(rc.resource, diff); err != nil {
			out.Action, out.Err = ActionError, err
			return out
		}
	}

	// A resource that just started as part of its own apply does not also
	// need the restart a notification would trigger.
	if wasNotified && !startedByThisApply(diff) {
		if r, ok := n.res.(resource.Refreshable); ok {
			if err := r.Refresh(rc.resource); err != nil {
				out.Action, out.Err = ActionError, fmt.Errorf("refresh: %w", err)
				return out
			}
			if diff.Empty() {
				out.Action = ActionRefresh
			}
		}
	}
	return out
}

// startedByThisApply reports whether the diff already brought a service up,
// making a notify-driven restart redundant.
func startedByThisApply(d resource.Diff) bool {
	want, ok := d.Want("active")
	return ok && want == "active"
}

// classify turns a diff into the verb shown in a plan. Kinds do not all carry
// a "state" field — Package tracks per-package keys, Sysctl tracks per-key
// values — so the general rule is about direction: everything appearing is a
// create, everything disappearing is a delete, anything else is an update.
func classify(d resource.Diff) Action {
	if d.Empty() {
		return ActionNoop
	}
	if want, ok := d.Want("run"); ok && want == execSatisfied {
		return ActionRun
	}

	allAppearing, allDisappearing := true, true
	for _, f := range d {
		if !isAbsent(f.Have) {
			allAppearing = false
		}
		if !isAbsent(f.Want) {
			allDisappearing = false
		}
	}
	switch {
	case allDisappearing:
		return ActionDelete
	case allAppearing:
		return ActionCreate
	default:
		return ActionUpdate
	}
}

// execSatisfied mirrors the Exec provider's "already done" marker.
const execSatisfied = "satisfied"

// isAbsent covers both the provider-reported absence ("absent") and the
// marker Compare uses when a key is missing entirely ("<absent>").
func isAbsent(v string) bool {
	return v == resource.Absent || v == "<absent>"
}

// blockDependents marks everything downstream of a failed resource as
// unrunnable, so a missing package does not produce a cascade of confusing
// secondary failures.
func blockDependents(g *graph, preds map[resource.ID]map[resource.ID]bool, failed resource.ID, blocked map[resource.ID]string, reason string) {
	for id, p := range preds {
		if p[failed] {
			if _, already := blocked[id]; !already {
				blocked[id] = reason
			}
		}
	}
}

// prune deletes resources recorded in state that the repository no longer
// declares. Deletions run in reverse dependency order.
func (rc *Context) prune(g *graph, snap *state.Snapshot) []Result {
	var stale []*manifest.Document
	for _, ref := range snap.Refs() {
		rec := snap.Resources[ref]
		id := resource.ID{Kind: rec.Kind, Name: rec.Name}
		if _, live := g.nodes[id]; live {
			continue
		}
		docs, err := manifest.Parse([]byte(rec.Manifest), "state:"+ref)
		if err != nil || len(docs) == 0 {
			continue
		}
		stale = append(stale, docs[0])
	}
	if len(stale) == 0 {
		return nil
	}

	staleGraph, err := build(stale)
	if err != nil {
		// Stale manifests may reference resources that no longer exist;
		// fall back to plain reverse-alphabetical order.
		return rc.pruneUnordered(stale, snap)
	}

	var out []Result
	for i := len(staleGraph.sorted) - 1; i >= 0; i-- {
		out = append(out, rc.pruneOne(staleGraph.sorted[i].res, staleGraph.sorted[i].id, snap))
	}
	return out
}

func (rc *Context) pruneUnordered(stale []*manifest.Document, snap *state.Snapshot) []Result {
	sort.Slice(stale, func(i, j int) bool { return stale[i].Ref() > stale[j].Ref() })
	var out []Result
	for _, doc := range stale {
		res, err := resource.Build(doc)
		if err != nil {
			out = append(out, Result{ID: resource.ID{Kind: doc.Kind, Name: doc.Metadata.Name}, Action: ActionError, Err: err})
			continue
		}
		out = append(out, rc.pruneOne(res, res.ID(), snap))
	}
	return out
}

func (rc *Context) pruneOne(res resource.Resource, id resource.ID, snap *state.Snapshot) Result {
	start := time.Now()
	out := Result{ID: id, Action: ActionPrune, Source: "state"}

	del, ok := res.(resource.Deletable)
	if !ok {
		out.Action = ActionSkip
		out.Messages = []string{"kind does not support pruning; remove it manually"}
		return out
	}
	out.Diff = resource.Diff{{Field: "state", Have: resource.Present, Want: resource.Absent}}

	if rc.opts.DryRun {
		out.Duration = time.Since(start)
		return out
	}

	var messages []string
	rc.resource.Logf = func(format string, args ...any) {
		messages = append(messages, fmt.Sprintf(format, args...))
	}
	if err := del.Delete(rc.resource); err != nil {
		out.Action, out.Err = ActionError, fmt.Errorf("prune: %w", err)
	} else {
		delete(snap.Resources, id.String())
	}
	out.Messages = messages
	out.Duration = time.Since(start)
	return out
}

// checkHealth runs post-apply verification on resources that support it.
func (rc *Context) checkHealth(g *graph, report *Report, changed map[resource.ID]bool) {
	for i := range report.Results {
		res := &report.Results[i]
		if res.Err != nil || res.Action == ActionSkip || res.Action == ActionPrune {
			continue
		}
		n, ok := g.nodes[res.ID]
		if !ok {
			continue
		}
		checker, ok := n.res.(resource.Checker)
		if !ok {
			continue
		}
		if rc.opts.DryRun {
			continue
		}
		status, detail, err := checker.Health(rc.resource)
		if err != nil {
			res.Health = resource.HealthUnknown
			res.HealthDetail = err.Error()
			continue
		}
		res.Health = status
		res.HealthDetail = detail
	}
}

// recordState stores what was applied so a later run can detect removal.
func recordState(snap *state.Snapshot, n *node, res Result, revision string) {
	raw, err := yaml.Marshal(n.doc)
	if err != nil {
		return
	}
	desired := map[string]string{}
	for _, f := range res.Diff {
		desired[f.Field] = f.Want
	}
	if prev, ok := snap.Resources[n.id.String()]; ok && len(res.Diff) == 0 {
		desired = prev.Desired
	}
	snap.Resources[n.id.String()] = state.Record{
		Kind:      n.id.Kind,
		Name:      n.id.Name,
		Manifest:  string(raw),
		Desired:   desired,
		AppliedAt: time.Now().UTC(),
		Revision:  revision,
	}
}

// filterDocs applies --only selectors. A selector is a bare kind ("Service"),
// a full ref ("Service/nginx"), or a glob over either.
func filterDocs(docs []*manifest.Document, only []string) ([]*manifest.Document, error) {
	if len(only) == 0 {
		return docs, nil
	}
	var out []*manifest.Document
	for _, doc := range docs {
		for _, sel := range only {
			match, err := selectorMatches(sel, doc)
			if err != nil {
				return nil, err
			}
			if match {
				out = append(out, doc)
				break
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no resources matched %s", strings.Join(only, ", "))
	}
	return out, nil
}

func selectorMatches(sel string, doc *manifest.Document) (bool, error) {
	if !strings.Contains(sel, "/") {
		ok, err := filepath.Match(sel, doc.Kind)
		return ok, err
	}
	return filepath.Match(sel, doc.Ref())
}
