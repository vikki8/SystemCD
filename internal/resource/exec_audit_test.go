package resource

import (
	"strings"
	"testing"

	"github.com/vikki8/systemcd/internal/host"
)

// countRan counts executed command lines containing sub.
func countRan(h *host.MemHost, sub string) int {
	n := 0
	for _, c := range h.Commands {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

// applyThenNotify mirrors the engine for a notified resource: Apply when the
// diff is non-empty, then Refresh.
func applyThenNotify(t *testing.T, r Resource, c *Context) {
	t.Helper()
	converge(t, r, c)
	if err := r.(Refreshable).Refresh(c); err != nil {
		t.Fatalf("refresh: %v", err)
	}
}

func TestExecNotifiedWhilePendingRunsOnce(t *testing.T) {
	// A pending Exec that is also notified was run by Apply and then again
	// by Refresh in the same reconcile: a non-idempotent migration ran twice.
	for name, guard := range map[string]string{
		"unless": "  unless: test -f /var/lib/app/.migrated\n",
		"always": "  always: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := host.NewMem()
			h.AddStub(host.CommandStub{Match: "test -f", ExitCode: 1})
			c := newContext(h)
			r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Exec\nmetadata:\n  name: migrate\nspec:\n  command: /usr/local/bin/migrate\n"+guard)
			applyThenNotify(t, r, c)
			if n := countRan(h, "/usr/local/bin/migrate"); n != 1 {
				t.Errorf("command ran %d times, want once; commands = %v", n, h.Commands)
			}
		})
	}
}

func TestExecRefreshRespectsGuards(t *testing.T) {
	// A notify says "something changed", not "ignore the guards".
	cases := map[string]struct {
		guard string
		setup func(h *host.MemHost)
		runs  bool
	}{
		"refreshOnly with creates present": {
			guard: "  refreshOnly: true\n  creates: /etc/app/.done\n",
			setup: func(h *host.MemHost) { h.SetFile("/etc/app/.done", "", 0o644, 0, 0) },
		},
		"refreshOnly with onlyIf failing": {
			guard: "  refreshOnly: true\n  onlyIf: test -d /opt/app\n",
			setup: func(h *host.MemHost) { h.AddStub(host.CommandStub{Match: "test -d /opt/app", ExitCode: 1}) },
		},
		"refreshOnly with unless succeeding": {
			guard: "  refreshOnly: true\n  unless: /opt/app/bin/check\n",
			setup: func(h *host.MemHost) {},
		},
		"refreshOnly whose guards allow it": {
			guard: "  refreshOnly: true\n  creates: /etc/app/.done\n",
			setup: func(h *host.MemHost) {},
			runs:  true,
		},
		"refreshOnly without guards": {
			guard: "  refreshOnly: true\n",
			setup: func(h *host.MemHost) {},
			runs:  true,
		},
		"notified Exec already satisfied": {
			guard: "  creates: /etc/app/.done\n",
			setup: func(h *host.MemHost) { h.SetFile("/etc/app/.done", "", 0o644, 0, 0) },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := host.NewMem()
			tc.setup(h)
			c := newContext(h)
			r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Exec\nmetadata:\n  name: reindex\nspec:\n  command: /opt/app/bin/reindex\n"+tc.guard)
			applyThenNotify(t, r, c)
			if got := countRan(h, "/opt/app/bin/reindex"); (got == 1) != tc.runs || got > 1 {
				t.Errorf("ran %d times, want runs=%v; commands = %v", got, tc.runs, h.Commands)
			}
		})
	}
}

func TestExecDryRunNeverRunsTheCommand(t *testing.T) {
	// Only the guards may run during a plan (they are how a plan knows
	// whether the Exec is pending); the command itself never does.
	h := host.NewMem()
	h.AddStub(host.CommandStub{Match: "test -d /opt/app", ExitCode: 0})
	h.AddStub(host.CommandStub{Match: "test -f", ExitCode: 1})
	c := newContext(h)
	c.DryRun = true
	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Exec\nmetadata:\n  name: reindex\nspec:\n  command: /opt/app/bin/reindex\n  onlyIf: test -d /opt/app\n  unless: test -f /opt/app/.indexed\n")

	observed, err := r.Observe(c)
	if err != nil {
		t.Fatal(err)
	}
	if observed["run"] != execPending {
		t.Fatalf("observed = %v, want pending", observed)
	}
	if err := r.Apply(c, Diff{{Field: "run", Have: execPending, Want: execSatisfied}}); err != nil {
		t.Fatal(err)
	}
	if err := r.(Refreshable).Refresh(c); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range h.Commands {
		if strings.Contains(cmd, "/opt/app/bin/reindex") {
			t.Errorf("a dry run executed the command: %v", h.Commands)
		}
	}
}

func TestExecValidation(t *testing.T) {
	for name, spec := range map[string]string{
		"relative creates": "  command: /bin/true\n  creates: var/lib/app/.done\n",
		"empty argv[0]":    "  argv: [\"\", \"-x\"]\n  always: true\n",
	} {
		if err := buildErr(t, "apiVersion: systemcd.dev/v1\nkind: Exec\nmetadata:\n  name: x\nspec:\n"+spec); err == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
}
