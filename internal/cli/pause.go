package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/vikki8/systemcd/internal/host"
	"github.com/vikki8/systemcd/internal/paths"
)

// PauseRecord is what the pause file holds, so `status` and the agent's logs
// can say who stopped reconciliation and why.
type PauseRecord struct {
	Reason string    `json:"reason"`
	By     string    `json:"by,omitempty"`
	Since  time.Time `json:"since"`
	// Until, when set, is when the pause expires on its own. A pause that
	// outlives the incident it was declared for is how a fleet quietly stops
	// being managed, so an expiry is offered and encouraged.
	Until *time.Time `json:"until,omitempty"`
}

// Expired reports whether a timed pause has run out.
func (p PauseRecord) Expired(now time.Time) bool {
	return p.Until != nil && now.After(*p.Until)
}

// readPause returns the current pause, if reconciliation is paused.
func readPause(h host.Host) (PauseRecord, bool) {
	data, err := h.ReadFile(paths.PauseFile)
	if err != nil {
		return PauseRecord{}, false
	}
	var rec PauseRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		// An unparsable pause file still means somebody wanted this host left
		// alone. Honour the intent rather than the format.
		return PauseRecord{Reason: "unreadable pause file; treating the host as paused"}, true
	}
	if rec.Expired(time.Now()) {
		return rec, false
	}
	return rec, true
}

// runPause stops the agent from converging this host.
//
// A self-healing agent reverting an operator's emergency change in the middle
// of an incident is the failure mode that makes teams turn continuous
// reconciliation off permanently. A supported pause is how it stays on.
func runPause(app *App, args []string) int {
	fs := newFlagSet(app, "pause", `Stop the agent from converging this host.

Drift is still detected and reported; nothing is changed until you resume.
Use this during an incident instead of stopping the agent, so the pause is
visible to everyone looking at the machine.`)
	reason := fs.String("reason", "", "why the host is paused (required, and it will be read by whoever finds it)")
	duration := fs.String("for", "", "expire the pause automatically after this long, e.g. 2h")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if *reason == "" {
		fs.Usage()
		return app.errf("--reason is required: a pause nobody can explain is a pause nobody dares lift")
	}

	rec := PauseRecord{Reason: *reason, Since: time.Now().UTC(), By: currentUser()}
	if *duration != "" {
		d, err := time.ParseDuration(*duration)
		if err != nil {
			return app.errf("--for %q is not a duration: %v", *duration, err)
		}
		until := rec.Since.Add(d)
		rec.Until = &until
	}

	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return app.errf("%v", err)
	}
	if err := app.Host.MkdirAll(paths.DataDir, 0o700); err != nil {
		return app.errf("%v", err)
	}
	if err := app.Host.WriteFile(paths.PauseFile, append(data, '\n'), 0o644); err != nil {
		return app.errf("write %s: %v", paths.PauseFile, err)
	}

	fmt.Fprintf(app.Stdout, "Reconciliation paused: %s\n", rec.Reason)
	if rec.Until != nil {
		fmt.Fprintf(app.Stdout, "Expires automatically at %s\n", rec.Until.Local().Format("2006-01-02 15:04:05 MST"))
	} else {
		fmt.Fprintf(app.Stdout, "No expiry set. Consider `--for 4h` so this cannot be forgotten.\n")
	}
	fmt.Fprintf(app.Stdout, "Drift is still reported. Resume with: systemcd resume\n")
	return ExitOK
}

// runResume lets the agent converge again.
func runResume(app *App, args []string) int {
	fs := newFlagSet(app, "resume", "Allow the agent to converge this host again.")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}

	rec, paused := readPause(app.Host)
	if err := app.Host.Remove(paths.PauseFile); err != nil && !errors.Is(err, host.ErrNotExist) {
		return app.errf("remove %s: %v", paths.PauseFile, err)
	}
	if !paused {
		fmt.Fprintf(app.Stdout, "This host was not paused.\n")
		return ExitOK
	}
	fmt.Fprintf(app.Stdout, "Reconciliation resumed (was paused %s: %s).\n",
		relativeSince(rec.Since), rec.Reason)
	return ExitOK
}

func relativeSince(t time.Time) string {
	if t.IsZero() {
		return "for an unknown time"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return fmt.Sprintf("for %dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("for %dh", int(d.Hours()))
	default:
		return fmt.Sprintf("for %dd", int(d.Hours()/24))
	}
}

func currentUser() string {
	for _, key := range []string{"SUDO_USER", "USER", "LOGNAME"} {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}
	return ""
}
