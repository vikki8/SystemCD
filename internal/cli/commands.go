package cli

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/vikki8/systemcd/internal/engine"
	"github.com/vikki8/systemcd/internal/gitsync"
	"github.com/vikki8/systemcd/internal/manifest"
	"github.com/vikki8/systemcd/internal/paths"
	"github.com/vikki8/systemcd/internal/report"
	"github.com/vikki8/systemcd/internal/state"
)

func newFlagSet(app *App, name, usage string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(app.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(app.Stderr, "Usage: systemcd %s [flags]\n\n%s\n\nFlags:\n", name, usage)
		fs.PrintDefaults()
	}
	return fs
}

func runPlan(app *App, args []string) int {
	fs := newFlagSet(app, "plan", "Show the changes systemcd would make. The host is not modified.")
	var f commonFlags
	f.register(fs, true)
	if err := fs.Parse(args); err != nil {
		return ExitError
	}

	repo, node, err := app.loadRepo(&f)
	if err != nil {
		return app.errf("%v", err)
	}

	ctx, cancel := signalContext()
	defer cancel()

	rep, err := engine.Reconcile(ctx, engine.Options{
		Repo:     repo,
		Host:     app.Host,
		Node:     node,
		DryRun:   true,
		Prune:    resolvePrune(repo.Config, &f),
		Revision: app.revision(ctx, repo.Root),
		Only:     f.only,
		Store:    state.New(app.Host),
	})
	if err != nil {
		return app.errf("%v", err)
	}
	return app.emit(rep, &f, true)
}

func runApply(app *App, args []string) int {
	fs := newFlagSet(app, "apply", "Converge the host to the manifests in the given directory.")
	var f commonFlags
	f.register(fs, true)
	if err := fs.Parse(args); err != nil {
		return ExitError
	}

	repo, node, err := app.loadRepo(&f)
	if err != nil {
		return app.errf("%v", err)
	}
	app.warnIfNotRoot()

	ctx, cancel := signalContext()
	defer cancel()

	rep, err := app.apply(ctx, repo, node, &f, app.revision(ctx, repo.Root))
	if err != nil {
		return app.errf("%v", err)
	}
	return app.emit(rep, &f, false)
}

func runStatus(app *App, args []string) int {
	fs := newFlagSet(app, "status", "Report the sync and health of every managed resource.")
	var f commonFlags
	f.register(fs, false)
	if err := fs.Parse(args); err != nil {
		return ExitError
	}

	repo, node, err := app.loadRepo(&f)
	if err != nil {
		return app.errf("%v", err)
	}

	ctx, cancel := signalContext()
	defer cancel()

	rep, err := engine.Reconcile(ctx, engine.Options{
		Repo:   repo,
		Host:   app.Host,
		Node:   node,
		DryRun: true,
		Only:   f.only,
		Store:  state.New(app.Host),
	})
	if err != nil {
		return app.errf("%v", err)
	}

	if f.json {
		if err := report.JSON(app.Stdout, rep); err != nil {
			return app.errf("%v", err)
		}
	} else if err := report.Status(app.Stdout, rep, app.reportOptions(&f)); err != nil {
		return app.errf("%v", err)
	}
	if rep.Err() != nil {
		return ExitError
	}
	if rep.OutOfSync() {
		return ExitOutOfSync
	}
	return ExitOK
}

func runValidate(app *App, args []string) int {
	fs := newFlagSet(app, "validate", "Parse and type-check the manifests. Nothing on the host is read or changed.")
	var f commonFlags
	f.register(fs, false)
	if err := fs.Parse(args); err != nil {
		return ExitError
	}

	repo, err := manifest.Load(f.root)
	if err != nil {
		return app.errf("%v", err)
	}
	if err := repo.Validate(); err != nil {
		return app.errf("%v", err)
	}
	// Building every resource runs each kind's own spec validation, which is
	// where most real mistakes (bad mode strings, missing paths) surface.
	if err := engine.Check(repo); err != nil {
		return app.errf("%v", err)
	}
	fmt.Fprintf(app.Stdout, "ok: %d documents, %d resources\n", len(repo.Documents), len(repo.Documents))
	return ExitOK
}

func runSync(app *App, args []string) int {
	fs := newFlagSet(app, "sync", "Fetch the desired-state repository, then apply it.")
	var f commonFlags
	f.register(fs, true)
	repoURL := fs.String("repo", "", "git URL of the desired-state repository")
	branch := fs.String("branch", "", "branch, tag or commit to check out")
	dir := fs.String("dir", paths.RepoDir, "local checkout directory")
	depth := fs.Int("depth", 1, "clone depth; 0 for full history")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	app.warnIfNotRoot()

	ctx, cancel := signalContext()
	defer cancel()

	rev, root, err := app.fetch(ctx, *repoURL, *branch, *dir, *depth, &f)
	if err != nil {
		return app.errf("%v", err)
	}
	f.root = root

	repo, node, err := app.loadRepo(&f)
	if err != nil {
		return app.errf("%v", err)
	}
	rep, err := app.apply(ctx, repo, node, &f, rev)
	if err != nil {
		return app.errf("%v", err)
	}
	return app.emit(rep, &f, false)
}

func runAgent(app *App, args []string) int {
	fs := newFlagSet(app, "agent", "Reconcile continuously: pull the repo, apply, and heal drift.")
	var f commonFlags
	f.register(fs, true)
	repoURL := fs.String("repo", "", "git URL of the desired-state repository")
	branch := fs.String("branch", "", "branch, tag or commit to track")
	dir := fs.String("dir", paths.RepoDir, "local checkout directory")
	interval := fs.String("interval", "", "reconcile period, e.g. 5m (overrides the repo config)")
	once := fs.Bool("once", false, "reconcile once and exit")
	selfHeal := fs.Bool("self-heal", true, "reapply on drift; when false, drift is only reported")
	depth := fs.Int("depth", 1, "clone depth; 0 for full history")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	app.warnIfNotRoot()

	ctx, cancel := signalContext()
	defer cancel()

	// The very first iteration establishes the interval, since it may come
	// from the repo's own Config document.
	period := 5 * time.Minute
	for iteration := 0; ; iteration++ {
		rev, root, err := app.fetch(ctx, *repoURL, *branch, *dir, *depth, &f)
		if err != nil {
			fmt.Fprintf(app.Stderr, "systemcd: sync failed: %v\n", err)
			if *once {
				return ExitError
			}
			if !sleepCtx(ctx, period) {
				return ExitOK
			}
			continue
		}
		f.root = root

		repo, node, err := app.loadRepo(&f)
		if err != nil {
			fmt.Fprintf(app.Stderr, "systemcd: %v\n", err)
			if *once {
				return ExitError
			}
			if !sleepCtx(ctx, period) {
				return ExitOK
			}
			continue
		}

		if d, err := time.ParseDuration(firstNonEmpty(*interval, repo.Config.Interval)); err == nil && d > 0 {
			period = d
		}

		dryRun := !*selfHeal && !repo.Config.SelfHeal
		rep, err := engine.Reconcile(ctx, engine.Options{
			Repo:         repo,
			Host:         app.Host,
			Node:         node,
			DryRun:       dryRun,
			Prune:        resolvePrune(repo.Config, &f),
			Revision:     rev,
			Only:         f.only,
			Store:        state.New(app.Host),
			HealthChecks: f.health,
		})
		if err != nil {
			fmt.Fprintf(app.Stderr, "systemcd: reconcile failed: %v\n", err)
		} else {
			app.logIteration(rep, &f)
		}

		if *once {
			if err != nil {
				return ExitError
			}
			return app.exitCode(rep, dryRun)
		}
		if !sleepCtx(ctx, period) {
			fmt.Fprintln(app.Stderr, "systemcd: shutting down")
			return ExitOK
		}
	}
}

func runRollback(app *App, args []string) int {
	fs := newFlagSet(app, "rollback", "Check out the previously applied revision and apply it.")
	var f commonFlags
	f.register(fs, true)
	to := fs.String("to", "", "revision to roll back to; defaults to the previous applied revision")
	dir := fs.String("dir", paths.RepoDir, "local checkout directory")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	app.warnIfNotRoot()

	ctx, cancel := signalContext()
	defer cancel()

	store := state.New(app.Host)
	snap, err := store.Load()
	if err != nil {
		return app.errf("%v", err)
	}

	target := *to
	if target == "" {
		prev, ok := snap.PreviousRevision()
		if !ok {
			return app.errf("no previous revision recorded; pass --to <revision>")
		}
		target = prev
	}

	syncer := gitsync.New(app.Host)
	syncer.Logf = func(format string, args ...any) { fmt.Fprintf(app.Stderr, "systemcd: "+format+"\n", args...) }
	if err := syncer.Checkout(ctx, *dir, target); err != nil {
		return app.errf("%v", err)
	}
	fmt.Fprintf(app.Stderr, "systemcd: rolled back to %s\n", target)

	f.root = *dir
	repo, node, err := app.loadRepo(&f)
	if err != nil {
		return app.errf("%v", err)
	}
	rep, err := app.apply(ctx, repo, node, &f, target)
	if err != nil {
		return app.errf("%v", err)
	}
	return app.emit(rep, &f, false)
}

// apply runs a real reconcile with health checks enabled.
func (app *App) apply(ctx context.Context, repo *manifest.Repository, node manifest.Node, f *commonFlags, revision string) (*engine.Report, error) {
	logf := func(format string, args ...any) {}
	if f.verbose {
		logf = func(format string, args ...any) { fmt.Fprintf(app.Stderr, format+"\n", args...) }
	}
	return engine.Reconcile(ctx, engine.Options{
		Repo:         repo,
		Host:         app.Host,
		Node:         node,
		DryRun:       false,
		Prune:        resolvePrune(repo.Config, f),
		Revision:     revision,
		Only:         f.only,
		Store:        state.New(app.Host),
		Logf:         logf,
		HealthChecks: f.health,
	})
}

// fetch resolves the manifest root, pulling from git when a repo is
// configured. It returns the revision and the directory to load manifests
// from.
func (app *App) fetch(ctx context.Context, repoURL, branch, dir string, depth int, f *commonFlags) (string, string, error) {
	syncer := gitsync.New(app.Host)
	syncer.Logf = func(format string, args ...any) { fmt.Fprintf(app.Stderr, "systemcd: "+format+"\n", args...) }

	// Without a remote, `sync` still works: it reconciles whatever is in the
	// working directory, which is the normal way to try systemcd out.
	if repoURL == "" {
		root := f.root
		if root == "" || root == "." {
			root = dir
			if _, err := app.Host.Stat(root); err != nil {
				root = f.root
			}
		}
		rev, _ := syncer.Revision(ctx, root)
		return rev, root, nil
	}

	rev, err := syncer.Sync(ctx, gitsync.Source{URL: repoURL, Ref: branch, Dir: dir, Depth: depth})
	if err != nil {
		return "", "", err
	}
	return rev, dir, nil
}

func (app *App) revision(ctx context.Context, root string) string {
	rev, _ := gitsync.New(app.Host).Revision(ctx, root)
	return rev
}

func (app *App) reportOptions(f *commonFlags) report.Options {
	return report.Options{
		Color:    !f.noColor && report.DetectColor(app.Stdout),
		Verbose:  f.verbose,
		ShowDiff: true,
	}
}

// emit renders a report and returns the process exit code.
func (app *App) emit(rep *engine.Report, f *commonFlags, dryRun bool) int {
	if f.json {
		if err := report.JSON(app.Stdout, rep); err != nil {
			return app.errf("%v", err)
		}
	} else if err := report.Text(app.Stdout, rep, app.reportOptions(f)); err != nil {
		return app.errf("%v", err)
	}
	return app.exitCode(rep, dryRun)
}

func (app *App) exitCode(rep *engine.Report, dryRun bool) int {
	if rep.Err() != nil {
		return ExitError
	}
	if len(rep.Degraded()) > 0 {
		return ExitError
	}
	if dryRun && rep.OutOfSync() {
		return ExitOutOfSync
	}
	return ExitOK
}

// logIteration prints a one-line summary per agent pass, which is what shows
// up in `journalctl -u systemcd`.
func (app *App) logIteration(rep *engine.Report, f *commonFlags) {
	c := rep.Counts()
	if c.Changed == 0 && c.Failed == 0 && !f.verbose {
		return
	}
	if f.json {
		_ = report.JSON(app.Stdout, rep)
		return
	}
	_ = report.Text(app.Stdout, rep, app.reportOptions(f))
}

// sleepCtx waits for d, returning false if the context was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
