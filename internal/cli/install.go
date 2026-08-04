package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/vikki8/systemcd/internal/paths"
)

// unitTemplate is the systemd unit systemcd installs for itself. The agent is
// a plain long-running process: systemd owns restarts, logging goes to the
// journal, and the tool that manages services is itself a managed service.
const unitTemplate = `[Unit]
Description=systemcd — GitOps reconciler for this host
Documentation=https://github.com/vikki8/systemcd
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s
Restart=always
RestartSec=15s
# Reconciling the host needs root; the agent is not sandboxed beyond this.
User=root
StateDirectory=systemcd
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`

func runInstall(app *App, args []string) int {
	fs := newFlagSet(app, "install", "Write and enable a systemd unit that runs the systemcd agent.")
	repoURL := fs.String("repo", "", "git URL of the desired-state repository (required)")
	branch := fs.String("branch", "main", "branch to track")
	interval := fs.String("interval", "5m", "reconcile period")
	dir := fs.String("dir", paths.RepoDir, "local checkout directory")
	binary := fs.String("binary", "", "path to the systemcd binary (defaults to this executable)")
	prune := fs.Bool("prune", false, "let the agent delete resources removed from the repository")
	enable := fs.Bool("enable", true, "enable and start the unit after writing it")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if *repoURL == "" {
		fs.Usage()
		return app.errf("--repo is required")
	}
	app.warnIfNotRoot()

	exe := *binary
	if exe == "" {
		resolved, err := os.Executable()
		if err != nil {
			return app.errf("cannot determine the path of this binary: %v", err)
		}
		exe = resolved
	}

	cmd := []string{exe, "agent",
		"--repo", *repoURL,
		"--branch", *branch,
		"--interval", *interval,
		"--dir", *dir,
	}
	if *prune {
		cmd = append(cmd, "--prune")
	}
	unit := fmt.Sprintf(unitTemplate, strings.Join(cmd, " "))

	if err := app.Host.WriteFile(paths.UnitFile, []byte(unit), 0o644); err != nil {
		return app.errf("write %s: %v", paths.UnitFile, err)
	}
	fmt.Fprintf(app.Stdout, "wrote %s\n", paths.UnitFile)

	ctx, cancel := signalContext()
	defer cancel()

	if res, err := app.Host.Run(ctx, "systemctl", "daemon-reload"); err != nil || !res.OK() {
		return app.errf("systemctl daemon-reload failed: %v %s", err, res.Stderr)
	}
	if !*enable {
		fmt.Fprintf(app.Stdout, "run `systemctl enable --now systemcd` when ready\n")
		return ExitOK
	}
	if res, err := app.Host.Run(ctx, "systemctl", "enable", "--now", "systemcd"); err != nil || !res.OK() {
		return app.errf("systemctl enable --now systemcd failed: %v %s", err, res.Stderr)
	}
	fmt.Fprintf(app.Stdout, "systemcd agent enabled and started\nfollow it with: journalctl -u systemcd -f\n")
	return ExitOK
}
