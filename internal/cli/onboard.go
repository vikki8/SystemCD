package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/vikki8/systemcd/internal/engine"
	"github.com/vikki8/systemcd/internal/inventory"
	"github.com/vikki8/systemcd/internal/manifest"
)

// runInventory scans the host for configuration systemcd does not manage.
//
// This is step one of onboarding an existing fleet, and it is the step no
// other tool offers: before you can declare what a machine should be, you
// need to know what it already is and which parts somebody changed by hand.
func runInventory(app *App, args []string) int {
	fs := newFlagSet(app, "inventory", `Discover configuration on this host that systemcd does not manage.

Uses the package manager's own checksums to find files that were edited after
installation, plus locally-created config, units, packages and accounts.`)
	asJSON := fs.Bool("json", false, "emit machine-readable JSON")
	all := fs.Bool("all", false, "include items systemcd already owns")
	categories := stringList{}
	fs.Var(&categories, "category", "restrict to a category; repeatable (see the summary for names)")
	scanPaths := stringList{}
	fs.Var(&scanPaths, "path", "filesystem root to scan; repeatable (default /etc)")
	limit := fs.Int("limit", inventory.DefaultLimit, "maximum items per category")
	generate := fs.String("generate", "", "write starter manifests and file payloads into this repository directory")
	genLabels := stringList{}
	fs.Var(&genLabels, "generate-label", "scope generated documents to this label, as key=value; repeatable")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}

	ctx, cancel := signalContext()
	defer cancel()

	snap, err := app.snapshot()
	if err != nil {
		return app.errf("%v", err)
	}

	cats := make([]inventory.Category, 0, len(categories))
	for _, c := range categories {
		cats = append(cats, inventory.Category(c))
	}

	rep, err := inventory.Scan(ctx, inventory.Options{
		Host:       app.Host,
		Snapshot:   snap,
		Categories: cats,
		Paths:      scanPaths,
		Limit:      *limit,
	})
	if err != nil {
		return app.errf("%v", err)
	}

	if *generate != "" {
		labels, err := parseLabels(genLabels)
		if err != nil {
			return app.errf("%v", err)
		}
		res, err := inventory.Generate(rep, inventory.GenerateOptions{
			Host:       app.Host,
			Dir:        *generate,
			Categories: cats,
			Targets:    labels,
		})
		if err != nil {
			return app.errf("%v", err)
		}
		app.reportGeneration(res, *generate)
		return ExitOK
	}

	if *asJSON {
		items := rep.Items
		if !*all {
			items = rep.Unmanaged()
		}
		return app.writeJSON(map[string]any{
			"hostname": rep.Hostname,
			"counts":   rep.Counts(),
			"items":    items,
			"skipped":  rep.Skipped,
		})
	}
	app.printInventory(rep, *all)
	return ExitOK
}

func (app *App) printInventory(rep *inventory.Report, all bool) {
	items := rep.Items
	if !all {
		items = rep.Unmanaged()
	}
	c := rep.Counts()

	if len(items) == 0 {
		fmt.Fprintf(app.Stdout, "Nothing unmanaged found on %s.\n", rep.Hostname)
	} else {
		byCategory := map[inventory.Category][]inventory.Item{}
		for _, item := range items {
			byCategory[item.Category] = append(byCategory[item.Category], item)
		}
		names := make([]inventory.Category, 0, len(byCategory))
		for k := range byCategory {
			names = append(names, k)
		}
		sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })

		for _, category := range names {
			group := byCategory[category]
			fmt.Fprintf(app.Stdout, "\n%s (%d)\n", category, len(group))
			fmt.Fprintf(app.Stdout, "%s\n", strings.Repeat("─", 46))
			for _, item := range group {
				line := "  " + item.Claim
				if item.Package != "" {
					line += "  [" + item.Package + "]"
				}
				if item.ManagedBy != "" {
					line += "  → managed by " + item.ManagedBy
				}
				fmt.Fprintln(app.Stdout, line)
			}
		}
	}

	fmt.Fprintf(app.Stdout, "\n%s\n", strings.Repeat("─", 46))
	fmt.Fprintf(app.Stdout, "%d items · %d already managed · %d unmanaged\n", c.Total, c.Managed, c.Unmanaged)
	for _, note := range rep.Skipped {
		fmt.Fprintf(app.Stderr, "systemcd: could not scan %s\n", note)
	}
	if c.Unmanaged > 0 {
		fmt.Fprintf(app.Stdout, "\nNext: `systemcd inventory --generate <repo>` writes starter manifests,\n"+
			"then `systemcd adopt -C <repo>` takes ownership without changing anything.\n")
	}
}

func (app *App) reportGeneration(res *inventory.GenerateResult, dir string) {
	if res.Documents == 0 {
		fmt.Fprintf(app.Stdout, "Nothing to generate: no unmanaged items matched.\n")
	} else {
		fmt.Fprintf(app.Stdout, "Wrote %d documents to %s\n", res.Documents, res.ManifestPath)
		fmt.Fprintf(app.Stdout, "Extracted %d file payloads under %s/files\n", len(res.FilePaths), dir)
		fmt.Fprintf(app.Stdout, "\nReview it, delete what should not be managed, then:\n")
		fmt.Fprintf(app.Stdout, "  systemcd adopt -C %s --dry-run\n", dir)
	}
	for _, skip := range res.Skipped {
		fmt.Fprintf(app.Stderr, "systemcd: skipped %s\n", skip)
	}
}

// runAdopt takes ownership of resources that already exist, without changing
// them — the safe first move on a machine nobody wants rewritten today.
func runAdopt(app *App, args []string) int {
	fs := newFlagSet(app, "adopt", `Take ownership of resources that already exist on this host.

The host is not modified. Each resource's current state is recorded as the
baseline, so it becomes owned and immediately in sync; the next `+"`plan`"+`
then shows exactly what the repository would change.`)
	var f commonFlags
	f.register(fs, false)
	dryRun := fs.Bool("dry-run", false, "show what would be adopted without recording ownership")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}

	repo, node, err := app.loadRepo(&f)
	if err != nil {
		return app.errf("%v", err)
	}
	if !*dryRun {
		app.warnIfNotRoot()
	}

	ctx, cancel := signalContext()
	defer cancel()

	run := func() int {
		rep, err := engine.Adopt(ctx, engine.AdoptOptions{
			Repo:     repo,
			Host:     app.Host,
			Node:     node,
			Store:    app.storeFor(),
			Only:     f.only,
			DryRun:   *dryRun,
			Revision: app.revision(ctx, repo.Root),
		})
		if err != nil {
			return app.errf("%v", err)
		}
		return app.emit(rep, &f, *dryRun)
	}
	if *dryRun {
		return run()
	}
	return app.withLock(run)
}

// runRelease hands ownership back. A fleet tool without a migration-out path
// is a trap, and teams are right not to adopt one.
func runRelease(app *App, args []string) int {
	fs := newFlagSet(app, "release", `Hand ownership of a resource back and stop reconciling it.

  --preserve  leave the host exactly as it is (default)
  --restore   put the resource back the way it was before systemcd adopted it

Remove the resource from the repository first: releasing something the
repository still declares would be undone by the next reconcile.`)
	var f commonFlags
	f.register(fs, false)
	restore := fs.Bool("restore", false, "restore the pre-adoption state before releasing")
	preserve := fs.Bool("preserve", false, "leave the host as it is (the default)")
	all := fs.Bool("all", false, "release every resource systemcd owns on this host")
	force := fs.Bool("force", false, "release even though the repository still declares it")
	dryRun := fs.Bool("dry-run", false, "show what would happen without changing ownership")
	refs, err := parseWithPositional(fs, args)
	if err != nil {
		return ExitError
	}
	if *restore && *preserve {
		return app.errf("--restore and --preserve are mutually exclusive")
	}
	if len(refs) == 0 && !*all {
		fs.Usage()
		return app.errf("name at least one resource (for example File/motd), or pass --all")
	}

	mode := engine.ReleasePreserve
	if *restore {
		mode = engine.ReleaseRestore
	}

	// The repository is consulted so release can refuse to hand back
	// something git will immediately reclaim. A missing repository is fine —
	// releasing on a decommissioned host should still work.
	repo, node, loadErr := app.loadRepo(&f)
	if loadErr != nil {
		repo, node = nil, app.bestEffortNode(&f)
	}
	if !*dryRun {
		app.warnIfNotRoot()
	}

	ctx, cancel := signalContext()
	defer cancel()

	run := func() int {
		rep, err := engine.Release(ctx, engine.ReleaseOptions{
			Repo:   repo,
			Host:   app.Host,
			Node:   node,
			Store:  app.storeFor(),
			Refs:   refs,
			All:    *all,
			Mode:   mode,
			DryRun: *dryRun,
			Force:  *force,
		})
		if err != nil {
			return app.errf("%v", err)
		}
		return app.emit(rep, &f, *dryRun)
	}
	if *dryRun {
		return run()
	}
	return app.withLock(run)
}

// bestEffortNode resolves node identity when the repository cannot be loaded,
// so releasing still works on a host whose repository is gone.
func (app *App) bestEffortNode(f *commonFlags) manifest.Node {
	name := f.hostname
	if name == "" {
		name, _ = app.Host.Hostname()
	}
	labels, _ := parseLabels(f.labels)
	return manifest.Node{Hostname: name, Labels: labels}
}

func parseLabels(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, kv := range pairs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("label %q must be key=value", kv)
		}
		out[k] = v
	}
	return out, nil
}
