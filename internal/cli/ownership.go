package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vikki8/systemcd/internal/resource"
	"github.com/vikki8/systemcd/internal/state"
)

// runShow answers "what does systemcd know about this resource?" — ownership,
// when it was last applied, when it last actually changed, and at which
// revision. Answering that without reading JSON by hand is most of what an
// operator wants mid-incident.
func runShow(app *App, args []string) int {
	fs := newFlagSet(app, "show", "Show systemcd's ownership record for a resource (Kind/name).")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return app.errf("expected exactly one resource reference, for example Service/nginx")
	}
	ref := fs.Arg(0)
	if _, err := resource.ParseID(ref); err != nil {
		return app.errf("%v", err)
	}

	snap, err := app.snapshot()
	if err != nil {
		return app.errf("%v", err)
	}
	rec, ok := snap.Resources[ref]
	if !ok {
		if *asJSON {
			return app.writeJSON(map[string]any{"resource": ref, "owned": false})
		}
		fmt.Fprintf(app.Stdout, "%s\n  owned by systemcd: no\n", ref)
		fmt.Fprintf(app.Stdout, "\nsystemcd has no record of applying this resource on this host.\n")
		return ExitOK
	}

	if *asJSON {
		return app.writeJSON(map[string]any{
			"resource":       ref,
			"owned":          true,
			"adopted":        rec.Adopted,
			"claims":         rec.Claims,
			"firstAppliedAt": rec.FirstAppliedAt,
			"lastAppliedAt":  rec.LastAppliedAt,
			"lastChangedAt":  rec.LastChangedAt,
			"revision":       rec.Revision,
			"applied":        rec.Applied,
			"priorState":     rec.PriorState,
		})
	}

	w := app.Stdout
	fmt.Fprintf(w, "%s\n", ref)
	fmt.Fprintf(w, "  owned by systemcd: yes\n")
	fmt.Fprintf(w, "  ownership:         %s\n", ownershipWord(rec.Adopted))
	fmt.Fprintf(w, "  first applied:     %s\n", stamp(rec.FirstAppliedAt))
	fmt.Fprintf(w, "  last applied:      %s\n", stamp(rec.LastAppliedAt))
	fmt.Fprintf(w, "  last changed:      %s\n", stampOrNever(rec.LastChangedAt))
	if rec.Revision != "" {
		fmt.Fprintf(w, "  revision:          %s\n", rec.Revision)
	}
	if len(rec.Claims) > 0 {
		fmt.Fprintf(w, "  claims:\n")
		for _, c := range rec.Claims {
			fmt.Fprintf(w, "    %s\n", c)
		}
	}
	if len(rec.Applied) > 0 {
		fmt.Fprintf(w, "  last applied state:\n")
		for _, k := range sortedKeys(rec.Applied) {
			fmt.Fprintf(w, "    %-12s %s\n", k+":", rec.Applied[k])
		}
	}
	if rec.Adopted && len(rec.PriorState) > 0 {
		fmt.Fprintf(w, "  state before systemcd took it over:\n")
		for _, k := range sortedKeys(rec.PriorState) {
			fmt.Fprintf(w, "    %-12s %s\n", k+":", rec.PriorState[k])
		}
	}
	return ExitOK
}

// runOwns is the reverse lookup: given a path, unit, package or account, which
// resource — if any — is responsible for it?
func runOwns(app *App, args []string) int {
	fs := newFlagSet(app, "owns", `Report which resource owns something on this host.

Accepts a bare path (/etc/nginx/nginx.conf) or an explicit claim
(unit:nginx.service, package:nginx, user:deploy, sysctl:net.ipv4.ip_forward).`)
	asJSON := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return app.errf("expected exactly one path or claim")
	}

	target := fs.Arg(0)
	claim := target
	if strings.HasPrefix(target, "/") {
		claim = string(resource.ClaimPath) + ":" + target
	}

	snap, err := app.snapshot()
	if err != nil {
		return app.errf("%v", err)
	}
	rec, found := snap.Owner(claim)

	if *asJSON {
		out := map[string]any{"claim": claim, "owned": found}
		if found {
			out["resource"] = rec.Ref()
			out["adopted"] = rec.Adopted
			out["lastAppliedAt"] = rec.LastAppliedAt
			out["lastChangedAt"] = rec.LastChangedAt
			out["revision"] = rec.Revision
		}
		return app.writeJSON(out)
	}

	if !found {
		fmt.Fprintf(app.Stdout, "%s is not managed by systemcd on this host.\n", claim)
		// Not an error: "nothing owns this" is a legitimate answer, and
		// scripts should not have to distinguish it from a failure.
		return ExitOK
	}
	fmt.Fprintf(app.Stdout, "%s\n", claim)
	fmt.Fprintf(app.Stdout, "  owned by:      %s\n", rec.Ref())
	fmt.Fprintf(app.Stdout, "  ownership:     %s\n", ownershipWord(rec.Adopted))
	fmt.Fprintf(app.Stdout, "  last applied:  %s\n", stamp(rec.LastAppliedAt))
	fmt.Fprintf(app.Stdout, "  last changed:  %s\n", stampOrNever(rec.LastChangedAt))
	if rec.Revision != "" {
		fmt.Fprintf(app.Stdout, "  revision:      %s\n", rec.Revision)
	}
	return ExitOK
}

// runCapabilities reports what this host's package manager can actually
// guarantee, so a manifest author can find out before writing a field that
// would be rejected.
func runCapabilities(app *App, args []string) int {
	fs := newFlagSet(app, "capabilities", "Report what systemcd can guarantee on this host.")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}

	ctx, cancel := signalContext()
	defer cancel()

	caps, err := resource.DetectCapabilities(&resource.Context{Ctx: ctx, Host: app.Host})
	if err != nil {
		if *asJSON {
			return app.writeJSON(map[string]any{"packageManager": nil, "error": err.Error()})
		}
		fmt.Fprintf(app.Stdout, "package manager:  none detected (%v)\n", err)
		return ExitOK
	}

	if *asJSON {
		return app.writeJSON(map[string]any{
			"packageManager": caps.Manager,
			"versionPinning": map[string]any{
				"supported": caps.VersionPinning,
				"note":      caps.Note,
			},
			"kinds": resource.Kinds(),
		})
	}

	fmt.Fprintf(app.Stdout, "package manager:  %s\n", caps.Manager)
	fmt.Fprintf(app.Stdout, "version pinning:  %s\n", supportedWord(caps.VersionPinning))
	if !caps.VersionPinning && caps.Note != "" {
		for _, line := range wrap(caps.Note, 66) {
			fmt.Fprintf(app.Stdout, "                  %s\n", line)
		}
	}
	fmt.Fprintf(app.Stdout, "resource kinds:   %s\n", strings.Join(resource.Kinds(), ", "))
	return ExitOK
}

func (app *App) snapshot() (*state.Snapshot, error) {
	store := state.New(app.Host)
	store.Warnf = func(format string, args ...any) {
		fmt.Fprintf(app.Stderr, "systemcd: "+format+"\n", args...)
	}
	return store.Load()
}

func (app *App) writeJSON(v any) int {
	enc := json.NewEncoder(app.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return app.errf("%v", err)
	}
	return ExitOK
}

func ownershipWord(adopted bool) string {
	if adopted {
		return "adopted (it already existed when systemcd first managed it)"
	}
	return "created by systemcd"
}

func supportedWord(ok bool) string {
	if ok {
		return "supported"
	}
	return "not supported"
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Local().Format("2006-01-02 15:04:05 MST")
}

func stampOrNever(t time.Time) string {
	if t.IsZero() {
		return "never (systemcd has only ever confirmed it was already correct)"
	}
	return stamp(t)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if !resource.Informational(k) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// wrap breaks text into lines no longer than width, on word boundaries.
func wrap(s string, width int) []string {
	var lines []string
	var current string
	for _, word := range strings.Fields(s) {
		switch {
		case current == "":
			current = word
		case len(current)+1+len(word) <= width:
			current += " " + word
		default:
			lines = append(lines, current)
			current = word
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}
