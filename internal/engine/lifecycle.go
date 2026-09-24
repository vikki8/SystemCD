package engine

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	"github.com/vikki8/systemcd/internal/host"
	"github.com/vikki8/systemcd/internal/manifest"
	"github.com/vikki8/systemcd/internal/paths"
	"github.com/vikki8/systemcd/internal/resource"
	"github.com/vikki8/systemcd/internal/state"
	"gopkg.in/yaml.v3"
)

// Lifecycle actions. They complete the ownership state machine:
//
//	unmanaged --adopt--> owned --release--> unmanaged
//	                       |
//	                       +--prune--> deleted
//
// Adoption and release are the two transitions that move ownership without
// necessarily changing the host, which is exactly what a fleet migration
// needs and what most configuration tools have no vocabulary for.
const (
	ActionAdopt   Action = "adopt"
	ActionRelease Action = "release"
	ActionRestore Action = "restore"
)

// AdoptOptions configures taking ownership of resources that already exist.
type AdoptOptions struct {
	Repo   *manifest.Repository
	Host   host.Host
	Node   manifest.Node
	Store  *state.Store
	Only   []string
	DryRun bool
	// Revision labels the adoption in the ownership record.
	Revision string
	Logf     func(format string, args ...any)
}

// Adopt takes ownership of resources that already exist on the host, without
// changing them.
//
// This is the safe on-ramp for an existing fleet. Applying a manifest to a
// five-year-old server rewrites its configuration on day one, which nobody
// will authorize. Adopting records the current state as the baseline instead:
// the resource becomes owned and immediately in sync, and the *next* `plan`
// shows exactly what the repository would change — attributed as a repository
// change, because the host still holds what systemcd recorded.
func Adopt(ctx context.Context, opts AdoptOptions) (*Report, error) {
	if opts.Repo == nil || opts.Host == nil {
		return nil, errors.New("engine: adopt needs a repository and a host")
	}

	selected, err := opts.Repo.SelectForE(opts.Node)
	if err != nil {
		return nil, err
	}
	docs, err := filterDocs(selected, opts.Only)
	if err != nil {
		return nil, err
	}
	g, err := build(docs)
	if err != nil {
		return nil, err
	}

	rc := &resource.Context{
		Ctx: ctx, Host: opts.Host, RepoRoot: opts.Repo.Root,
		DryRun: opts.DryRun, Node: opts.Node,
	}

	snap := &state.Snapshot{Resources: map[string]state.Record{}}
	if opts.Store != nil {
		if snap, err = opts.Store.Load(); err != nil {
			return nil, err
		}
	}

	report := &Report{Revision: opts.Revision, DryRun: opts.DryRun, Operation: "adopt", Started: time.Now()}

	for _, n := range g.sorted {
		report.Results = append(report.Results, adoptOne(rc, n, snap, opts))
	}
	report.Finished = time.Now()

	if !opts.DryRun && opts.Store != nil {
		if err := opts.Store.Save(snap); err != nil {
			return report, fmt.Errorf("save state: %w", err)
		}
	}
	return report, nil
}

func adoptOne(rc *resource.Context, n *node, snap *state.Snapshot, opts AdoptOptions) (out Result) {
	start := time.Now()
	out = Result{ID: n.id, Source: n.doc.Location(), Owner: ownershipOf(snap, n.id)}
	out.Owner.Claims = resource.ClaimStrings(resource.ClaimsOf(n.res))
	defer func() { out.Duration = time.Since(start) }()

	if _, already := snap.Resources[n.id.String()]; already {
		out.Action = ActionNoop
		out.Messages = []string{"already owned by systemcd"}
		return out
	}

	desired, err := n.res.Desired(rc)
	if err != nil {
		out.Action, out.Err = ActionError, fmt.Errorf("desired state: %w", err)
		return out
	}
	observed, err := n.res.Observe(rc)
	if err != nil {
		out.Action, out.Err = ActionError, fmt.Errorf("observe: %w", err)
		return out
	}

	// Nothing to adopt: the resource does not exist yet, so `apply` is the
	// right verb, not `adopt`.
	if classify(resource.Compare(desired, observed, nil)) == ActionCreate {
		out.Action = ActionSkip
		out.Messages = []string{"does not exist on this host yet; `systemcd apply` will create it"}
		return out
	}

	out.Action = ActionAdopt
	out.observed = observed
	// The diff shown is what the repository *would* change once this resource
	// is applied — the operator sees the pending work before agreeing to own
	// it, rather than discovering it on the next reconcile.
	out.Diff = resource.Compare(desired, observed, sensitiveOf(n.res))
	out.Owner.Origin = state.OriginNew

	if opts.DryRun {
		return out
	}

	var messages []string
	rc.Logf = func(format string, args ...any) { messages = append(messages, fmt.Sprintf(format, args...)) }

	baseline, err := captureBaseline(rc, n.res, n.id)
	if err != nil {
		out.Action, out.Err = ActionError, fmt.Errorf("capture baseline: %w", err)
		return out
	}

	raw, err := storedManifest(n)
	if err != nil {
		out.Action, out.Err = ActionError, err
		return out
	}
	now := time.Now().UTC()
	snap.Resources[n.id.String()] = state.Record{
		Kind: n.id.Kind, Name: n.id.Name,
		Manifest: string(raw),
		Claims:   resource.ClaimStrings(resource.ClaimsOf(n.res)),
		// The recorded "applied" state is what is on the host right now, not
		// what the manifest wants. That is the whole point: adoption changes
		// ownership, not configuration.
		Applied:        map[string]string(observed),
		Adopted:        true,
		PriorState:     map[string]string(observed),
		BaselinePath:   baseline,
		FirstAppliedAt: now,
		LastAppliedAt:  now,
		Revision:       opts.Revision,
	}
	out.Messages = append(messages, "ownership recorded; the host was not modified")
	out.Owner = ownershipOf(snap, n.id)
	out.Owner.Claims = resource.ClaimStrings(resource.ClaimsOf(n.res))
	return out
}

// captureBaseline preserves the contents a resource had at adoption, so a
// later `release --restore` can put them back. Kinds with no content to
// capture return an empty path.
func captureBaseline(rc *resource.Context, res resource.Resource, id resource.ID) (string, error) {
	b, ok := res.(resource.Baseliner)
	if !ok {
		return "", nil
	}
	data, err := b.CaptureBaseline(rc)
	if err != nil || data == nil {
		return "", err
	}
	dest := filepath.Join(paths.BaselineDir, baselineName(id))
	if err := rc.Host.MkdirAll(paths.BaselineDir, 0o700); err != nil {
		return "", err
	}
	// Baselines can hold secrets from adopted config; keep them root-only.
	if err := rc.Host.WriteFile(dest, data, 0o600); err != nil {
		return "", err
	}
	return dest, nil
}

// baselineName maps a resource to one file name under the baseline
// directory. The name is percent-escaped rather than having '/' replaced, so
// it stays a single path element and "a/b" and "a_b" cannot share a baseline;
// kinds contain no '_', so the first one always ends the kind.
func baselineName(id resource.ID) string {
	return id.Kind + "_" + url.PathEscape(id.Name)
}

// storedManifest renders the document kept in state, from which prune and
// release rebuild the resource. For a sensitive resource the inline content
// is left out: state must not hold the value (README), and neither deleting
// nor restoring needs it.
func storedManifest(n *node) ([]byte, error) {
	doc := n.doc
	if len(sensitiveOf(n.res)) > 0 {
		cp := *doc
		cp.Spec = withoutKey(doc.Spec, "content")
		doc = &cp
	}
	return yaml.Marshal(doc)
}

// withoutKey returns a copy of a mapping node without one key.
func withoutKey(spec yaml.Node, key string) yaml.Node {
	m := &spec
	if m.Kind == yaml.DocumentNode && len(m.Content) > 0 {
		m = m.Content[0]
	}
	if m.Kind != yaml.MappingNode {
		return spec
	}
	out := *m
	out.Content = nil
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != key {
			out.Content = append(out.Content, m.Content[i], m.Content[i+1])
		}
	}
	return out
}

func sensitiveOf(r resource.Resource) map[string]bool {
	if s, ok := r.(resource.Sensitiver); ok {
		return s.SensitiveFields()
	}
	return nil
}

// ReleaseMode selects what happens to the host when ownership is handed back.
type ReleaseMode string

const (
	// ReleasePreserve leaves the host exactly as it is and forgets the
	// resource. The configuration systemcd applied stays in place.
	ReleasePreserve ReleaseMode = "preserve"
	// ReleaseRestore returns the resource to its pre-adoption state first.
	ReleaseRestore ReleaseMode = "restore"
)

// ReleaseOptions configures handing ownership back.
type ReleaseOptions struct {
	Repo  *manifest.Repository
	Host  host.Host
	Node  manifest.Node
	Store *state.Store
	// Refs are the resources to release. Empty with All set releases every
	// owned resource.
	Refs   []string
	All    bool
	Mode   ReleaseMode
	DryRun bool
	// Force releases a resource the repository still declares. Without it,
	// releasing something git still asks for would be a lie: the next
	// reconcile takes it straight back.
	Force bool
	Logf  func(format string, args ...any)
}

// Release hands ownership of resources back, with or without restoring what
// was there before systemcd adopted them.
//
// The migration-out path is the mirror of the migration-in path, and a fleet
// tool without one is a trap: teams will not adopt something they cannot back
// out of.
func Release(ctx context.Context, opts ReleaseOptions) (*Report, error) {
	if opts.Host == nil || opts.Store == nil {
		return nil, errors.New("engine: release needs a host and a state store")
	}
	if opts.Mode == "" {
		opts.Mode = ReleasePreserve
	}

	snap, err := opts.Store.Load()
	if err != nil {
		return nil, err
	}

	// Anything the repository still declares for this node is "live", and
	// releasing it without --force would be undone on the next reconcile.
	// Targeting alone decides that: patches only narrow a declared resource,
	// and a patch that fails to apply must not make the repository look empty.
	live := map[string]bool{}
	if opts.Repo != nil {
		for _, doc := range opts.Repo.Documents {
			if doc.Kind != manifest.PatchKind && doc.Metadata.Targets.Matches(opts.Node) {
				live[doc.Ref()] = true
			}
		}
	}

	refs := opts.Refs
	if opts.All {
		refs = snap.Refs()
	}
	if len(refs) == 0 {
		return nil, errors.New("engine: no resources named to release (pass a reference or --all)")
	}

	rc := &resource.Context{Ctx: ctx, Host: opts.Host, DryRun: opts.DryRun, Node: opts.Node}
	if opts.Repo != nil {
		rc.RepoRoot = opts.Repo.Root
	}

	report := &Report{DryRun: opts.DryRun, Operation: "release", Started: time.Now()}
	for _, ref := range refs {
		report.Results = append(report.Results, releaseOne(rc, ref, snap, live, opts))
	}
	report.Finished = time.Now()

	if !opts.DryRun {
		if err := opts.Store.Save(snap); err != nil {
			return report, fmt.Errorf("save state: %w", err)
		}
	}
	return report, nil
}

func releaseOne(rc *resource.Context, ref string, snap *state.Snapshot, live map[string]bool, opts ReleaseOptions) (out Result) {
	start := time.Now()
	id := idFromRef(ref)
	out = Result{ID: id, Source: "state", Owner: ownershipOf(snap, id)}
	defer func() { out.Duration = time.Since(start) }()

	rec, owned := snap.Resources[ref]
	if !owned {
		out.Action = ActionSkip
		out.Messages = []string{"not owned by systemcd on this host; nothing to release"}
		return out
	}

	if live[ref] && !opts.Force {
		out.Action = ActionError
		out.Err = fmt.Errorf(
			"the repository still declares %s for this host, so releasing it would be undone by the next reconcile. "+
				"Remove it from the repository first, then release it; or pass --force if you know the agent is stopped", ref)
		return out
	}

	out.Action = ActionRelease
	if opts.Mode == ReleaseRestore {
		out.Action = ActionRestore
	}

	// Rebuild the resource from the manifest stored at adoption, so we can
	// ask it to restore itself.
	var res resource.Resource
	if docs, err := manifest.Parse([]byte(rec.Manifest), "state:"+ref); err == nil && len(docs) > 0 {
		res, _ = resource.Build(docs[0])
	}

	if opts.Mode == ReleaseRestore {
		if res == nil {
			out.Action, out.Err = ActionError, errors.New("state holds an unusable manifest for this resource, so it cannot be restored; release with --preserve instead")
			return out
		}
		restorer, ok := res.(resource.Restorer)
		if !ok {
			out.Action = ActionSkip
			_, why := resource.CanRestore(res)
			out.Messages = []string{why + "; release it with --preserve instead"}
			return out
		}
		if len(rec.PriorState) == 0 {
			out.Action = ActionSkip
			out.Messages = []string{"systemcd created this resource rather than adopting it, so there is no earlier state to restore; use --preserve, or remove it from the repository and prune"}
			return out
		}
		out.Diff = restoreDiff(rec, sensitiveOf(res))

		if !opts.DryRun {
			var messages []string
			rc.Logf = func(format string, args ...any) { messages = append(messages, fmt.Sprintf(format, args...)) }

			var blob []byte
			if restorer.NeedsBaseline() {
				if rec.BaselinePath == "" {
					out.Action, out.Err = ActionError, errors.New("no baseline contents were captured at adoption, so the original cannot be put back; release with --preserve instead")
					return out
				}
				data, err := rc.Host.ReadFile(rec.BaselinePath)
				if err != nil {
					out.Action, out.Err = ActionError, fmt.Errorf("read baseline %s: %w", rec.BaselinePath, err)
					return out
				}
				blob = data
			}
			if err := restorer.Restore(rc, resource.State(rec.PriorState), blob); err != nil {
				out.Action, out.Err = ActionError, fmt.Errorf("restore: %w", err)
				return out
			}
			out.Messages = messages
		}
	} else {
		out.Messages = []string{"ownership dropped; the host is left exactly as it is"}
	}

	if opts.DryRun {
		return out
	}

	delete(snap.Resources, ref)
	if rec.BaselinePath != "" {
		// The baseline exists to serve a restore; once ownership is gone it is
		// just an unreferenced copy of someone's config.
		if err := rc.Host.Remove(rec.BaselinePath); err != nil && !errors.Is(err, host.ErrNotExist) {
			out.Messages = append(out.Messages, fmt.Sprintf("could not remove the baseline copy at %s: %v", rec.BaselinePath, err))
		}
	}
	out.Messages = append(out.Messages, "systemcd no longer tracks this resource")
	return out
}

// restoreDiff renders the restore as a diff from what systemcd applied back
// to what was there before, so `--dry-run` shows the actual consequence.
//
// Only fields systemcd applied are listed: a field the manifest never
// managed (a Service's `enabled` left unset, `state: unmanaged`) was not
// changed by systemcd, and restore leaves it alone. Sensitive fields are
// marked so their values are not printed.
func restoreDiff(rec state.Record, sensitive map[string]bool) resource.Diff {
	var out resource.Diff
	seen := map[string]bool{}
	for _, k := range sortedStateKeys(rec.PriorState) {
		seen[k] = true
		have, applied := rec.Applied[k]
		if !applied || have == rec.PriorState[k] {
			continue
		}
		out = append(out, resource.FieldDiff{Field: k, Have: have, Want: rec.PriorState[k], Sensitive: sensitive[k]})
	}
	for _, k := range sortedStateKeys(rec.Applied) {
		if !seen[k] {
			out = append(out, resource.FieldDiff{Field: k, Have: rec.Applied[k], Want: "<absent>", Sensitive: sensitive[k]})
		}
	}
	return out
}

func sortedStateKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if !resource.Informational(k) {
			out = append(out, k)
		}
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
