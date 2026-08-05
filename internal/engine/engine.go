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
	// ActionOrphan marks a resource systemcd still owns on this host but
	// that the repository no longer declares. It is reported rather than
	// silently ignored, because an unowned-but-still-managed resource is
	// exactly the state operators lose track of.
	ActionOrphan Action = "orphaned"
)

// Changed reports whether the action represents a modification to the host.
func (a Action) Changed() bool {
	switch a {
	case ActionCreate, ActionUpdate, ActionDelete, ActionPrune, ActionRun, ActionRefresh:
		return true
	}
	return false
}

// Ownership is what systemcd knows about its claim over a resource.
type Ownership struct {
	// Owned reports whether systemcd has a recorded claim on this resource.
	Owned bool
	// Adopted means the resource already existed when systemcd first took
	// ownership; systemcd did not create it.
	Adopted bool
	// Claims are the physical things the resource owns, as "kind:key".
	Claims []string
	// Origin says whether drift came from the repository or from outside.
	Origin state.Origin

	FirstAppliedAt time.Time
	LastAppliedAt  time.Time
	LastChangedAt  time.Time
	// Revision is the git revision in effect at the last change.
	Revision string
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
	// Owner carries the ownership record for this resource.
	Owner Ownership
	// ContentBefore and ContentAfter carry the text bodies behind a checksum
	// change, so a plan can show what actually differs rather than two
	// hashes. Empty when the kind has no text body or it is sensitive.
	ContentBefore string
	ContentAfter  string
	HasContent    bool
	// needsConfirm marks a destructive prune withheld pending --confirm.
	needsConfirm bool

	// desired is the full desired state, recorded on a successful apply so a
	// later run can tell external drift from a repository change.
	desired resource.State
	// observed is what the host looked like before this run changed anything.
	observed resource.State
	// baselinePath points at contents preserved on first adoption.
	baselinePath string
}

// OutOfSync reports whether the resource differed from the manifest.
//
// An orphan is deliberately not out of sync: the repository is not asking for
// any change to it. It shows up in reports as its own category, and only
// becomes an actionable difference once pruning is enabled.
func (r Result) OutOfSync() bool {
	switch r.Action {
	case ActionNoop, ActionSkip, ActionOrphan:
		return false
	}
	return true
}

// Report is the outcome of a whole reconcile.
type Report struct {
	Revision string
	DryRun   bool
	// Operation names what produced this report ("apply", "adopt",
	// "release"), so output does not claim to be something it is not.
	Operation string
	Results   []Result
	Started   time.Time
	Finished  time.Time
	// Notes are run-level messages that belong to no single resource.
	Notes []string
}

// Counts summarizes a report.
type Counts struct {
	Total    int
	InSync   int
	Changed  int
	Failed   int
	Skipped  int
	Degraded int
	Orphaned int
	Adopted  int
	Released int
	// ExternalDrift counts resources changed outside systemcd since the last
	// apply, as opposed to resources the repository moved ahead of.
	ExternalDrift int
	// NeedsConfirm counts destructive prunes withheld pending --confirm.
	NeedsConfirm int
}

// Counts tallies the report.
func (r *Report) Counts() Counts {
	var c Counts
	for _, res := range r.Results {
		c.Total++
		switch {
		case res.Err != nil:
			c.Failed++
		case res.Action == ActionOrphan:
			c.Orphaned++
		case res.Action == ActionAdopt:
			c.Adopted++
		case res.Action == ActionRelease, res.Action == ActionRestore:
			c.Released++
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
		if res.Owner.Origin == state.OriginExternal {
			c.ExternalDrift++
		}
		if res.needsConfirm {
			c.NeedsConfirm++
		}
	}
	return c
}

// ExternalDrift lists resources that were changed outside systemcd since the
// last apply. These are the ones worth paging someone about: the repository
// did not ask for them to change.
func (r *Report) ExternalDrift() []Result {
	var out []Result
	for _, res := range r.Results {
		if res.Owner.Origin == state.OriginExternal {
			out = append(out, res)
		}
	}
	return out
}

// Orphans lists resources systemcd owns that the repository no longer
// declares.
func (r *Report) Orphans() []Result {
	var out []Result
	for _, res := range r.Results {
		if res.Action == ActionOrphan {
			out = append(out, res)
		}
	}
	return out
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
	// ConfirmDestructive permits pruning resources whose deletion systemcd
	// cannot undo (packages, accounts, recursive directory removal).
	ConfirmDestructive bool
}

// Reconcile plans, and unless DryRun is set, applies the repository.
func Reconcile(ctx context.Context, opts Options) (*Report, error) {
	if opts.Repo == nil {
		return nil, errors.New("engine: no repository provided")
	}
	if opts.Host == nil {
		return nil, errors.New("engine: no host provided")
	}

	docs, err := opts.Repo.SelectForE(opts.Node)
	if err != nil {
		return nil, err
	}
	docs, err = filterDocs(docs, opts.Only)
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
				Owner:    ownershipOf(snap, n.id),
			})
			blockDependents(g, preds, n.id, blocked, fmt.Sprintf("depends on skipped %s", n.id))
			continue
		}

		res := rc.reconcileOne(n, notified[n.id], snap)
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

	// Ownership bookkeeping only makes sense over a complete view of the
	// repository. With --only, the unselected resources are absent by
	// request, not orphaned, so neither orphan reporting nor pruning may run.
	if len(opts.Only) == 0 {
		report.Results = append(report.Results, rc.reconcileOwnedButUndeclared(g, snap)...)
	} else if opts.Prune {
		report.Notes = append(report.Notes,
			"pruning was skipped because --only narrows the run; ownership cannot be judged from a partial view")
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

// ownershipOf reads what state knows about a resource, without any host
// observation.
func ownershipOf(snap *state.Snapshot, id resource.ID) Ownership {
	rec, ok := snap.Resources[id.String()]
	if !ok {
		return Ownership{Origin: state.OriginNew}
	}
	return Ownership{
		Owned:          true,
		Adopted:        rec.Adopted,
		Claims:         rec.Claims,
		FirstAppliedAt: rec.FirstAppliedAt,
		LastAppliedAt:  rec.LastAppliedAt,
		LastChangedAt:  rec.LastChangedAt,
		Revision:       rec.Revision,
	}
}

func (rc *Context) reconcileOne(n *node, wasNotified bool, snap *state.Snapshot) Result {
	start := time.Now()
	out := Result{
		ID: n.id, Action: ActionNoop, Source: n.doc.Location(), Notified: wasNotified,
		Owner: ownershipOf(snap, n.id),
	}
	out.Owner.Claims = resource.ClaimStrings(resource.ClaimsOf(n.res))

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
	out.desired, out.observed = desired, observed

	// A checksum change is not reviewable. Capture the text so the report can
	// show the lines that differ.
	if diff.Has("checksum") {
		if d, ok := n.res.(resource.ContentDiffer); ok {
			before, after, usable, err := d.ContentDiff(rc.resource)
			if err == nil && usable {
				out.ContentBefore, out.ContentAfter, out.HasContent = before, after, true
			}
		}
	}

	// Attribute the drift. If the host still holds every value systemcd last
	// wrote, the repository is what moved; if it does not, something outside
	// systemcd edited the machine. Only the second one is an incident.
	if rec, owned := snap.Resources[n.id.String()]; owned && !diff.Empty() {
		out.Owner.Origin = rec.OriginOf(observed, fieldsOf(diff))
	}

	if diff.Empty() && !wasNotified {
		return out
	}

	if rc.opts.DryRun {
		if diff.Empty() && wasNotified {
			out.Action = ActionRefresh
		}
		return out
	}

	// If systemcd is about to take over something that already exists and has
	// never been recorded, preserve the original contents first. After Apply
	// the original is gone, and "release --restore" would have nothing to put
	// back.
	if _, owned := snap.Resources[n.id.String()]; !owned && out.Action != ActionCreate {
		path, err := captureBaseline(rc.resource, n.res, n.id)
		if err != nil {
			out.Action, out.Err = ActionError, fmt.Errorf("capture baseline before adopting: %w", err)
			return out
		}
		out.baselinePath = path
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

// reconcileOwnedButUndeclared handles resources systemcd owns that the
// repository no longer declares. With pruning on they are deleted, in reverse
// dependency order; with pruning off they are reported as orphans rather than
// vanishing from the operator's view.
func (rc *Context) reconcileOwnedButUndeclared(g *graph, snap *state.Snapshot) []Result {
	var stale []*manifest.Document
	for _, ref := range snap.Refs() {
		rec := snap.Resources[ref]
		id := resource.ID{Kind: rec.Kind, Name: rec.Name}
		if _, live := g.nodes[id]; live {
			continue
		}
		docs, err := manifest.Parse([]byte(rec.Manifest), "state:"+ref)
		if err != nil || len(docs) == 0 {
			// The stored manifest is unusable, so the resource can neither be
			// rebuilt nor deleted. Say so instead of dropping it silently.
			stale = append(stale, nil)
			continue
		}
		stale = append(stale, docs[0])
	}

	var docs []*manifest.Document
	var out []Result
	for i, doc := range stale {
		if doc == nil {
			ref := snap.Refs()[i]
			out = append(out, Result{
				ID: idFromRef(ref), Action: ActionSkip, Source: "state",
				Messages: []string{"state holds an unparsable manifest for this resource; it cannot be pruned automatically"},
				Owner:    ownershipOf(snap, idFromRef(ref)),
			})
			continue
		}
		docs = append(docs, doc)
	}
	if len(docs) == 0 {
		return out
	}

	ordered, err := build(docs)
	if err != nil {
		// Stale manifests can reference resources that no longer exist;
		// fall back to reverse-alphabetical order.
		return append(out, rc.handleStaleUnordered(docs, snap)...)
	}
	for i := len(ordered.sorted) - 1; i >= 0; i-- {
		n := ordered.sorted[i]
		out = append(out, rc.handleStale(n.res, n.id, snap))
	}
	return out
}

func (rc *Context) handleStaleUnordered(stale []*manifest.Document, snap *state.Snapshot) []Result {
	sort.Slice(stale, func(i, j int) bool { return stale[i].Ref() > stale[j].Ref() })
	var out []Result
	for _, doc := range stale {
		res, err := resource.Build(doc)
		if err != nil {
			out = append(out, Result{ID: resource.ID{Kind: doc.Kind, Name: doc.Metadata.Name}, Action: ActionError, Err: err})
			continue
		}
		out = append(out, rc.handleStale(res, res.ID(), snap))
	}
	return out
}

func (rc *Context) handleStale(res resource.Resource, id resource.ID, snap *state.Snapshot) Result {
	start := time.Now()
	out := Result{ID: id, Action: ActionOrphan, Source: "state", Owner: ownershipOf(snap, id)}
	defer func() { out.Duration = time.Since(start) }()

	if !rc.opts.Prune {
		out.Messages = []string{"owned by systemcd but no longer declared in the repository; run with --prune to remove it"}
		return out
	}

	del, ok := res.(resource.Deletable)
	if !ok {
		out.Action = ActionSkip
		out.Messages = []string{"kind does not support pruning; remove it manually"}
		return out
	}

	// Deleting a package or an account is not something systemcd can undo,
	// so it needs a second, explicit signal beyond "--prune".
	if resource.IsDestructive(res) && !rc.opts.ConfirmDestructive {
		out.Action = ActionSkip
		out.needsConfirm = true
		out.Messages = []string{fmt.Sprintf("pruning %s would be irreversible; re-run with --confirm to allow it", id)}
		return out
	}

	out.Action = ActionPrune
	out.Diff = resource.Diff{{Field: "state", Have: resource.Present, Want: resource.Absent}}

	if rc.opts.DryRun {
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
	return out
}

func idFromRef(ref string) resource.ID {
	id, err := resource.ParseID(ref)
	if err != nil {
		return resource.ID{Kind: "Unknown", Name: ref}
	}
	return id
}

func fieldsOf(d resource.Diff) []string {
	out := make([]string, 0, len(d))
	for _, f := range d {
		out = append(out, f.Field)
	}
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

// recordState stores systemcd's ownership claim and the complete desired
// state it just applied.
//
// Recording the *whole* desired state, not just the fields that changed, is
// what lets the next run tell an external edit from a repository change: the
// question "does the host still hold what we wrote?" needs everything we
// wrote, not the subset that happened to differ last time.
func recordState(snap *state.Snapshot, n *node, res Result, revision string) {
	raw, err := yaml.Marshal(n.doc)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	ref := n.id.String()
	prev, existed := snap.Resources[ref]

	rec := state.Record{
		Kind:     n.id.Kind,
		Name:     n.id.Name,
		Manifest: string(raw),
		Claims:   resource.ClaimStrings(resource.ClaimsOf(n.res)),
		Applied:  map[string]string(res.desired),
		Revision: revision,
	}

	switch {
	case existed:
		rec.FirstAppliedAt = prev.FirstAppliedAt
		rec.Adopted = prev.Adopted
		rec.PriorState = prev.PriorState
		// The baseline is captured once, at adoption. Losing it on a later
		// apply would silently turn `release --restore` into a promise the
		// tool could no longer keep.
		rec.BaselinePath = prev.BaselinePath
		rec.LastChangedAt = prev.LastChangedAt
		if prev.Revision != "" && !res.Action.Changed() {
			// An unchanged resource keeps the revision that last moved it,
			// so "last changed at commit X" stays truthful.
			rec.Revision = prev.Revision
		}
	default:
		rec.FirstAppliedAt = now
		// Anything that was not created by this run already existed, which
		// means systemcd adopted it rather than owning it from birth. That
		// distinction matters when deciding whether pruning is safe.
		rec.Adopted = res.Action != ActionCreate
		if rec.Adopted {
			rec.PriorState = map[string]string(res.observed)
			rec.BaselinePath = res.baselinePath
		}
	}

	rec.LastAppliedAt = now
	if res.Action.Changed() {
		rec.LastChangedAt = now
	}
	if rec.Applied == nil && existed {
		rec.Applied = prev.Applied
	}
	snap.Resources[ref] = rec
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
