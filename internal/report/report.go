// Package report renders reconcile results for humans and for machines.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/vikki8/systemcd/internal/engine"
	"github.com/vikki8/systemcd/internal/resource"
	"github.com/vikki8/systemcd/internal/state"
)

// Options control rendering.
type Options struct {
	// Color enables ANSI styling.
	Color bool
	// Verbose prints in-sync resources too, not just drift.
	Verbose bool
	// ShowDiff prints per-field differences.
	ShowDiff bool
}

// DetectColor reports whether the writer looks like an interactive terminal
// that has not opted out via NO_COLOR.
func DetectColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

type palette struct {
	reset, bold, dim, red, green, yellow, blue, magenta, cyan string
}

func newPalette(color bool) palette {
	if !color {
		return palette{}
	}
	return palette{
		reset: "\033[0m", bold: "\033[1m", dim: "\033[2m",
		red: "\033[31m", green: "\033[32m", yellow: "\033[33m",
		blue: "\033[34m", magenta: "\033[35m", cyan: "\033[36m",
	}
}

// symbol and color for each action, borrowed from the diff vocabulary people
// already know from terraform and git.
func actionStyle(p palette, a engine.Action) (string, string) {
	switch a {
	case engine.ActionCreate:
		return "+", p.green
	case engine.ActionUpdate:
		return "~", p.yellow
	case engine.ActionDelete, engine.ActionPrune:
		return "-", p.red
	case engine.ActionRun:
		return ">", p.cyan
	case engine.ActionRefresh:
		return "↻", p.magenta
	case engine.ActionError:
		return "!", p.red
	case engine.ActionOrphan:
		return "?", p.yellow
	case engine.ActionSkip:
		return "·", p.dim
	default:
		return "=", p.dim
	}
}

// Text renders a report as a terminal-friendly summary.
func Text(w io.Writer, r *engine.Report, opts Options) error {
	p := newPalette(opts.Color)

	verb := "apply"
	if r.DryRun {
		verb = "plan"
	}
	header := fmt.Sprintf("systemcd %s", verb)
	if r.Revision != "" {
		header += fmt.Sprintf(" @ %s", shortRev(r.Revision))
	}
	fmt.Fprintf(w, "%s%s%s\n\n", p.bold, header, p.reset)

	shown := 0
	for _, res := range r.Results {
		if res.Action == engine.ActionNoop && !opts.Verbose {
			continue
		}
		shown++
		sym, color := actionStyle(p, res.Action)
		fmt.Fprintf(w, "%s%s %-28s%s %s%s%s\n", color, sym, res.ID.String(), p.reset, p.dim, res.Action, p.reset)

		if opts.ShowDiff {
			for _, f := range res.Diff {
				have, want := f.Have, f.Want
				if f.Sensitive {
					have, want = "(sensitive)", "(sensitive)"
				}
				fmt.Fprintf(w, "    %s%s%s: %s%s%s -> %s%s%s\n",
					p.dim, f.Field, p.reset,
					p.red, truncate(have), p.reset,
					p.green, truncate(want), p.reset)
			}
		}
		if res.Owner.Origin == state.OriginExternal {
			fmt.Fprintf(w, "    %schanged outside systemcd since %s%s\n",
				p.red, relative(res.Owner.LastAppliedAt), p.reset)
		}
		for _, m := range res.Messages {
			fmt.Fprintf(w, "    %s%s%s\n", p.dim, m, p.reset)
		}
		if res.Notified {
			fmt.Fprintf(w, "    %snotified by a dependency%s\n", p.dim, p.reset)
		}
		if res.Err != nil {
			fmt.Fprintf(w, "    %serror: %v%s\n", p.red, res.Err, p.reset)
		}
		if res.Health == resource.HealthDegraded {
			for _, line := range strings.Split(res.HealthDetail, "\n") {
				fmt.Fprintf(w, "    %sdegraded: %s%s\n", p.red, line, p.reset)
			}
		}
	}

	if shown == 0 {
		fmt.Fprintf(w, "%sEverything is in sync.%s\n", p.green, p.reset)
	}

	c := r.Counts()
	fmt.Fprintf(w, "\n%s%s%s\n", p.bold, strings.Repeat("─", 46), p.reset)
	status, statusColor := syncStatus(p, r)
	fmt.Fprintf(w, "%s%s%s  %d resources · %d in sync · %d %s · %d failed",
		statusColor, status, p.reset, c.Total, c.InSync, c.Changed, changeWord(r.DryRun), c.Failed)
	if c.Skipped > 0 {
		fmt.Fprintf(w, " · %d skipped", c.Skipped)
	}
	if c.Degraded > 0 {
		fmt.Fprintf(w, " · %s%d degraded%s", p.red, c.Degraded, p.reset)
	}
	if c.Orphaned > 0 {
		fmt.Fprintf(w, " · %s%d orphaned%s", p.yellow, c.Orphaned, p.reset)
	}
	fmt.Fprintf(w, "\n%stook %s%s\n", p.dim, r.Finished.Sub(r.Started).Round(time.Millisecond), p.reset)

	// External drift is the line worth waking someone for: the repository did
	// not ask for these to change, so something else edited the machine.
	if c.ExternalDrift > 0 {
		fmt.Fprintf(w, "\n%s%d resource(s) were changed outside systemcd since the last apply:%s\n",
			p.red, c.ExternalDrift, p.reset)
		for _, res := range r.ExternalDrift() {
			fmt.Fprintf(w, "  %s (last applied %s)\n", res.ID, relative(res.Owner.LastAppliedAt))
		}
	}
	if c.NeedsConfirm > 0 {
		fmt.Fprintf(w, "\n%s%d resource(s) were left in place because pruning them is irreversible.%s\n",
			p.yellow, c.NeedsConfirm, p.reset)
		fmt.Fprintf(w, "%sRe-run with --confirm to allow it.%s\n", p.dim, p.reset)
	}
	for _, note := range r.Notes {
		fmt.Fprintf(w, "\n%snote: %s%s\n", p.dim, note, p.reset)
	}
	return nil
}

// relative renders a timestamp the way an operator reads it mid-incident.
func relative(t time.Time) string {
	if t.IsZero() {
		return "an unknown time"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "less than a minute ago"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago (%s)", int(d.Hours()/24), t.Local().Format("2006-01-02 15:04"))
	}
}

func changeWord(dryRun bool) string {
	if dryRun {
		return "to change"
	}
	return "changed"
}

func syncStatus(p palette, r *engine.Report) (string, string) {
	c := r.Counts()
	switch {
	case c.Failed > 0:
		return "Failed", p.red
	case c.Degraded > 0:
		return "Degraded", p.red
	case r.OutOfSync() && r.DryRun:
		return "OutOfSync", p.yellow
	case r.OutOfSync():
		return "Synced", p.green
	default:
		return "Synced", p.green
	}
}

// Status renders the Argo-style per-resource sync table.
func Status(w io.Writer, r *engine.Report, opts Options) error {
	p := newPalette(opts.Color)
	fmt.Fprintf(w, "%s%-28s %-11s %-9s %-9s %-14s %s%s\n",
		p.bold, "RESOURCE", "SYNC", "HEALTH", "OWNED", "LAST CHANGED", "DETAIL", p.reset)

	results := append([]engine.Result(nil), r.Results...)
	sort.SliceStable(results, func(i, j int) bool { return results[i].ID.String() < results[j].ID.String() })

	for _, res := range results {
		sync, syncColor := "Synced", p.green
		switch {
		case res.Err != nil:
			sync, syncColor = "Error", p.red
		case res.Action == engine.ActionOrphan:
			sync, syncColor = "Orphaned", p.yellow
		case res.OutOfSync():
			sync, syncColor = "OutOfSync", p.yellow
		}

		health, healthColor := "-", p.dim
		switch res.Health {
		case resource.HealthHealthy:
			health, healthColor = "Healthy", p.green
		case resource.HealthDegraded:
			health, healthColor = "Degraded", p.red
		case resource.HealthUnknown:
			health, healthColor = "Unknown", p.yellow
		}

		owned, ownedColor := "no", p.dim
		if res.Owner.Owned {
			owned, ownedColor = "yes", p.green
			if res.Owner.Adopted {
				owned = "adopted"
			}
		}

		detail := ""
		switch {
		case res.Err != nil:
			detail = res.Err.Error()
		case res.Owner.Origin == state.OriginExternal:
			// Naming the cause beats listing the fields: an external edit is a
			// different problem from the repository moving ahead.
			detail = "changed outside systemcd: " + strings.Join(diffFields(res.Diff), ", ")
		case len(res.Diff) > 0:
			detail = strings.Join(diffFields(res.Diff), ", ")
		case len(res.Messages) > 0:
			detail = res.Messages[0]
		}

		fmt.Fprintf(w, "%s%-28s%s %s%-11s%s %s%-9s%s %s%-9s%s %-14s %s%s%s\n",
			p.reset, truncateTo(res.ID.String(), 28), p.reset,
			syncColor, sync, p.reset,
			healthColor, health, p.reset,
			ownedColor, owned, p.reset,
			lastChanged(res),
			p.dim, truncate(detail), p.reset)
	}
	return nil
}

// lastChanged renders when systemcd last modified the resource, which is not
// the same question as when it last confirmed it.
func lastChanged(res engine.Result) string {
	if !res.Owner.Owned {
		return "-"
	}
	if res.Owner.LastChangedAt.IsZero() {
		return "never"
	}
	return relative(res.Owner.LastChangedAt)
}

func diffFields(d resource.Diff) []string {
	out := make([]string, 0, len(d))
	for _, f := range d {
		out = append(out, f.Field)
	}
	return out
}

// jsonResult is the stable machine-readable shape.
type jsonResult struct {
	Resource string          `json:"resource"`
	Kind     string          `json:"kind"`
	Name     string          `json:"name"`
	Action   string          `json:"action"`
	Sync     string          `json:"sync"`
	Health   string          `json:"health,omitempty"`
	Detail   string          `json:"healthDetail,omitempty"`
	Diff     []jsonFieldDiff `json:"diff,omitempty"`
	Messages []string        `json:"messages,omitempty"`
	Error    string          `json:"error,omitempty"`
	Source   string          `json:"source,omitempty"`
	Owner    jsonOwnership   `json:"ownership"`
}

// jsonOwnership is what a monitoring system needs to distinguish "the repo
// changed" from "someone edited the box".
type jsonOwnership struct {
	Owned          bool       `json:"owned"`
	Adopted        bool       `json:"adopted,omitempty"`
	Claims         []string   `json:"claims,omitempty"`
	DriftOrigin    string     `json:"driftOrigin,omitempty"`
	FirstAppliedAt *time.Time `json:"firstAppliedAt,omitempty"`
	LastAppliedAt  *time.Time `json:"lastAppliedAt,omitempty"`
	LastChangedAt  *time.Time `json:"lastChangedAt,omitempty"`
	Revision       string     `json:"revision,omitempty"`
}

type jsonFieldDiff struct {
	Field string `json:"field"`
	Have  string `json:"have"`
	Want  string `json:"want"`
}

type jsonReport struct {
	Revision   string        `json:"revision,omitempty"`
	DryRun     bool          `json:"dryRun"`
	Status     string        `json:"status"`
	OutOfSync  bool          `json:"outOfSync"`
	Counts     engine.Counts `json:"counts"`
	DurationMS int64         `json:"durationMs"`
	Notes      []string      `json:"notes,omitempty"`
	Results    []jsonResult  `json:"results"`
}

// nonZero omits zero timestamps from JSON rather than emitting year 1.
func nonZero(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// JSON renders a report as machine-readable output, for pipelines and for
// scraping into monitoring.
func JSON(w io.Writer, r *engine.Report) error {
	out := jsonReport{
		Revision:   r.Revision,
		DryRun:     r.DryRun,
		OutOfSync:  r.OutOfSync(),
		Counts:     r.Counts(),
		DurationMS: r.Finished.Sub(r.Started).Milliseconds(),
		Notes:      r.Notes,
	}
	status, _ := syncStatus(palette{}, r)
	out.Status = status

	for _, res := range r.Results {
		jr := jsonResult{
			Resource: res.ID.String(),
			Kind:     res.ID.Kind,
			Name:     res.ID.Name,
			Action:   string(res.Action),
			Sync:     "Synced",
			Health:   string(res.Health),
			Detail:   res.HealthDetail,
			Messages: res.Messages,
			Source:   res.Source,
		}
		if res.OutOfSync() {
			jr.Sync = "OutOfSync"
		}
		if res.Action == engine.ActionOrphan {
			jr.Sync = "Orphaned"
		}
		jr.Owner = jsonOwnership{
			Owned:          res.Owner.Owned,
			Adopted:        res.Owner.Adopted,
			Claims:         res.Owner.Claims,
			DriftOrigin:    string(res.Owner.Origin),
			FirstAppliedAt: nonZero(res.Owner.FirstAppliedAt),
			LastAppliedAt:  nonZero(res.Owner.LastAppliedAt),
			LastChangedAt:  nonZero(res.Owner.LastChangedAt),
			Revision:       res.Owner.Revision,
		}
		if res.Err != nil {
			jr.Sync = "Error"
			jr.Error = res.Err.Error()
		}
		for _, f := range res.Diff {
			fd := jsonFieldDiff{Field: f.Field, Have: f.Have, Want: f.Want}
			if f.Sensitive {
				fd.Have, fd.Want = "(sensitive)", "(sensitive)"
			}
			jr.Diff = append(jr.Diff, fd)
		}
		out.Results = append(out.Results, jr)
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func truncate(s string) string { return truncateTo(s, 72) }

func truncateTo(s string, max int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func shortRev(rev string) string {
	if len(rev) > 12 && !strings.Contains(rev, " ") {
		return rev[:12]
	}
	return rev
}
