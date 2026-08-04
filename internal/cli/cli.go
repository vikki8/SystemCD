// Package cli implements the systemcd command line.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/vikki8/systemcd/internal/host"
	"github.com/vikki8/systemcd/internal/manifest"
	"github.com/vikki8/systemcd/internal/paths"
	"github.com/vikki8/systemcd/internal/resource"
	"github.com/vikki8/systemcd/internal/state"
	"gopkg.in/yaml.v3"
)

// Version is stamped at build time with -ldflags.
var Version = "dev"

// Exit codes. Code 2 mirrors `terraform plan -detailed-exitcode`: not an
// error, but "there is drift", which is what a monitoring check wants.
const (
	ExitOK        = 0
	ExitError     = 1
	ExitOutOfSync = 2
)

// App holds the process-wide dependencies a command needs.
type App struct {
	Host   host.Host
	Stdout io.Writer
	Stderr io.Writer
}

type command struct {
	name    string
	summary string
	run     func(app *App, args []string) int
}

func commands() []command {
	return []command{
		{"plan", "show what would change, without touching the host", runPlan},
		{"apply", "converge the host to the manifests", runApply},
		{"status", "report per-resource sync and health", runStatus},
		{"sync", "pull the desired-state repo, then apply", runSync},
		{"agent", "run the reconcile loop continuously", runAgent},
		{"rollback", "reapply the previously applied revision", runRollback},
		{"validate", "check manifests without contacting the host", runValidate},
		{"show", "show systemcd's ownership record for a resource", runShow},
		{"owns", "report which resource owns a path, unit, package or account", runOwns},
		{"capabilities", "report what systemcd can guarantee on this host", runCapabilities},
		{"install", "install systemcd's own systemd unit", runInstall},
		{"kinds", "list supported resource kinds", runKinds},
		{"version", "print the version", runVersion},
	}
}

// Main is the process entry point. It returns the exit code.
func Main(args []string) int {
	app := &App{Host: host.NewOS(), Stdout: os.Stdout, Stderr: os.Stderr}
	return app.Run(args)
}

// Run dispatches a command.
func (app *App) Run(args []string) int {
	if len(args) < 1 {
		app.usage()
		return ExitError
	}
	name := args[0]
	if name == "-h" || name == "--help" || name == "help" {
		app.usage()
		return ExitOK
	}
	if name == "-v" || name == "--version" {
		name = "version"
	}
	for _, c := range commands() {
		if c.name == name {
			return c.run(app, args[1:])
		}
	}
	fmt.Fprintf(app.Stderr, "systemcd: unknown command %q\n\n", name)
	app.usage()
	return ExitError
}

func (app *App) usage() {
	fmt.Fprintf(app.Stdout, `systemcd — GitOps for Linux hosts

Declare systemd units, config files, packages, users and kernel parameters in
git; systemcd reconciles the machine to match and reports drift.

Usage:
  systemcd <command> [flags]

Commands:
`)
	for _, c := range commands() {
		fmt.Fprintf(app.Stdout, "  %-13s %s\n", c.name, c.summary)
	}
	fmt.Fprintf(app.Stdout, `
Run "systemcd <command> -h" for the flags of a command.

Exit codes:
  0  success and in sync
  1  error
  2  drift detected (plan/status)
`)
}

func (app *App) errf(format string, args ...any) int {
	fmt.Fprintf(app.Stderr, "systemcd: "+format+"\n", args...)
	return ExitError
}

// commonFlags are shared by the reconciling commands.
type commonFlags struct {
	root     string
	only     stringList
	prune    bool
	noPrune  bool
	json     bool
	verbose  bool
	noColor  bool
	labels   stringList
	hostname string
	health   bool
	confirm  bool
}

func (f *commonFlags) register(fs *flag.FlagSet, withPrune bool) {
	fs.StringVar(&f.root, "C", ".", "directory holding the manifests")
	fs.Var(&f.only, "only", "restrict to matching resources (Kind, Kind/name, or a glob); repeatable")
	fs.BoolVar(&f.json, "json", false, "emit machine-readable JSON")
	fs.BoolVar(&f.verbose, "verbose", false, "also show resources already in sync")
	fs.BoolVar(&f.noColor, "no-color", false, "disable ANSI color")
	fs.Var(&f.labels, "label", "node label as key=value, for host targeting; repeatable")
	fs.StringVar(&f.hostname, "hostname", "", "override the detected hostname for targeting")
	fs.BoolVar(&f.health, "health", true, "run post-apply health checks")
	if withPrune {
		fs.BoolVar(&f.prune, "prune", false, "delete resources removed from the repository")
		fs.BoolVar(&f.noPrune, "no-prune", false, "never prune, overriding the repo config")
		fs.BoolVar(&f.confirm, "confirm", false, "allow pruning packages, accounts and directory trees, which systemcd cannot undo")
	}
}

// storeFor builds a state store that reports recoverable problems rather than
// swallowing them.
func (app *App) storeFor() *state.Store {
	store := state.New(app.Host)
	store.Warnf = func(format string, args ...any) {
		fmt.Fprintf(app.Stderr, "systemcd: "+format+"\n", args...)
	}
	return store
}

// withLock serializes host mutation. A hand-run `systemcd apply` and the
// agent's loop converging the same machine at the same time would interleave
// their writes and clobber each other's ownership records, so the second one
// is turned away with an explanation instead of racing.
func (app *App) withLock(fn func() int) int {
	unlock, err := app.storeFor().Lock()
	if errors.Is(err, host.ErrLocked) {
		return app.errf("another systemcd process is reconciling this host (lock: %s).\n"+
			"        Wait for it to finish, or check `systemctl status systemcd` if the agent is running.", paths.LockFile)
	}
	if err != nil {
		return app.errf("could not take the reconcile lock: %v", err)
	}
	defer func() {
		if err := unlock(); err != nil {
			fmt.Fprintf(app.Stderr, "systemcd: releasing the lock failed: %v\n", err)
		}
	}()
	return fn()
}

// stringList collects a repeatable flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// loadRepo reads manifests and resolves the node identity used for targeting.
func (app *App) loadRepo(f *commonFlags) (*manifest.Repository, manifest.Node, error) {
	repo, err := manifest.Load(f.root)
	if err != nil {
		return nil, manifest.Node{}, err
	}
	if err := repo.Validate(); err != nil {
		return nil, manifest.Node{}, err
	}
	node, err := app.node(repo, f)
	if err != nil {
		return nil, manifest.Node{}, err
	}
	return repo, node, nil
}

// node assembles the machine's identity from, in increasing precedence:
// the repo config, /etc/systemcd/node.yaml, and --label flags.
func (app *App) node(repo *manifest.Repository, f *commonFlags) (manifest.Node, error) {
	name := f.hostname
	if name == "" {
		h, err := app.Host.Hostname()
		if err != nil {
			return manifest.Node{}, err
		}
		name = h
	}
	labels := map[string]string{}
	for k, v := range repo.Config.Labels {
		labels[k] = v
	}
	for k, v := range app.nodeFileLabels() {
		labels[k] = v
	}
	for _, kv := range f.labels {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return manifest.Node{}, fmt.Errorf("label %q must be key=value", kv)
		}
		labels[k] = v
	}
	return manifest.Node{Hostname: name, Labels: labels}, nil
}

// nodeFileLabels reads optional node-local labels. A missing or malformed
// file is not fatal: targeting simply falls back to hostname matching.
func (app *App) nodeFileLabels() map[string]string {
	data, err := app.Host.ReadFile(paths.NodeConfigFile)
	if err != nil {
		return nil
	}
	var doc struct {
		Labels map[string]string `yaml:"labels"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		fmt.Fprintf(app.Stderr, "systemcd: ignoring %s: %v\n", paths.NodeConfigFile, err)
		return nil
	}
	return doc.Labels
}

// resolvePrune combines repo config with the command-line flags.
func resolvePrune(cfg manifest.Config, f *commonFlags) bool {
	if f.noPrune {
		return false
	}
	if f.prune {
		return true
	}
	return cfg.Prune
}

func runVersion(app *App, args []string) int {
	fmt.Fprintf(app.Stdout, "systemcd %s\n", Version)
	return ExitOK
}

func runKinds(app *App, args []string) int {
	kinds := resource.Kinds()
	sort.Strings(kinds)
	for _, k := range kinds {
		fmt.Fprintln(app.Stdout, k)
	}
	return ExitOK
}

// warnIfNotRoot tells the operator early that changes will fail, rather than
// letting them discover it one failed chown at a time.
func (app *App) warnIfNotRoot() {
	if os.Geteuid() != 0 {
		fmt.Fprintf(app.Stderr, "systemcd: warning: not running as root; most changes will fail\n")
	}
}

// signalContext cancels on SIGINT/SIGTERM so the agent stops between
// reconciles instead of being killed mid-apply.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// lockedRun runs fn under the reconcile lock, skipping the lock entirely for
// read-only passes. A busy lock is reported as an ordinary error so the agent
// can log it and try again next tick rather than dying.
func (app *App) lockedRun(readOnly bool, fn func() error) error {
	if readOnly {
		return fn()
	}
	unlock, err := app.storeFor().Lock()
	if errors.Is(err, host.ErrLocked) {
		return fmt.Errorf("another systemcd process holds the reconcile lock (%s); skipping this pass", paths.LockFile)
	}
	if err != nil {
		return fmt.Errorf("could not take the reconcile lock: %w", err)
	}
	defer func() { _ = unlock() }()
	return fn()
}
