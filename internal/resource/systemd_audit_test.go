package resource

import (
	"strings"
	"testing"

	"github.com/vikki8/systemcd/internal/host"
)

func serviceHost(enabled, active string) *host.MemHost {
	h := host.NewMem()
	h.AddStub(host.CommandStub{Match: "is-enabled", Stdout: enabled + "\n"})
	h.AddStub(host.CommandStub{Match: "is-active", Stdout: active + "\n"})
	return h
}

func TestServiceDisabledOnUnenableableUnitConverges(t *testing.T) {
	// `systemctl disable` on a static unit exits 0 and changes nothing (on
	// an indirect one it disables the units its Also= names). is-enabled
	// answers the same afterwards, so reporting "true" against a desired
	// "false" ran a disable on every reconcile and never converged.
	for _, state := range []string{"static", "indirect", "generated", "transient"} {
		for _, want := range []string{"true", "false"} {
			h := serviceHost(state, "active")
			r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Service\nmetadata:\n  name: helper\nspec:\n  enabled: "+want+"\n")
			assertConverged(t, r, newContext(h))
		}
	}
	// Units that can be disabled still report as enabled.
	h := serviceHost("enabled", "active")
	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Service\nmetadata:\n  name: nginx\nspec:\n  enabled: false\n")
	d := converge(t, r, newContext(h))
	if !d.Has("enabled") || !h.Ran("systemctl disable nginx.service") {
		t.Errorf("an enabled unit should be disabled; diff = %+v, commands = %v", d, h.Commands)
	}
}

func TestServiceStoppedFailedUnitConverges(t *testing.T) {
	// `systemctl stop` leaves a failed unit failed, so treating "failed" as
	// drift from "inactive" issued a stop on every reconcile.
	h := serviceHost("disabled", "failed")
	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Service\nmetadata:\n  name: batch\nspec:\n  state: stopped\n")
	assertConverged(t, r, newContext(h))

	// A failed unit that should be running is still drift.
	h2 := serviceHost("enabled", "failed")
	r2 := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Service\nmetadata:\n  name: batch\nspec:\n  state: started\n")
	if d := converge(t, r2, newContext(h2)); !d.Has("active") {
		t.Errorf("a failed unit meant to run should be started; diff = %+v", d)
	}
}

func TestServiceReloadingCountsAsRunning(t *testing.T) {
	for _, state := range []string{"reloading", "refreshing"} {
		h := serviceHost("enabled", state)
		c := newContext(h)
		r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Service\nmetadata:\n  name: nginx\n")
		assertConverged(t, r, c)
		status, _, err := r.(Checker).Health(c)
		if err != nil || status != HealthHealthy {
			t.Errorf("%s: health = %v, %v; want Healthy", state, status, err)
		}
	}
}

func TestServiceUnmanagedRefreshDoesNotStartIt(t *testing.T) {
	// restart and reload-or-restart both start a stopped unit. With
	// state: unmanaged, whether it runs is not systemcd's decision.
	for reload, verb := range map[string]string{"false": "try-restart", "true": "try-reload-or-restart"} {
		h := host.NewMem()
		r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Service\nmetadata:\n  name: cron\nspec:\n  state: unmanaged\n  reload: "+reload+"\n")
		if err := r.(Refreshable).Refresh(newContext(h)); err != nil {
			t.Fatal(err)
		}
		if !h.Ran("systemctl "+verb+" cron.service") || h.Ran("systemctl restart") || h.Ran("systemctl reload-or-restart") {
			t.Errorf("reload=%s: commands = %v, want %s", reload, h.Commands, verb)
		}
	}
	// A started service still gets the real restart.
	h := host.NewMem()
	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Service\nmetadata:\n  name: cron\n")
	if err := r.(Refreshable).Refresh(newContext(h)); err != nil {
		t.Fatal(err)
	}
	if !h.Ran("systemctl restart cron.service") {
		t.Errorf("commands = %v", h.Commands)
	}
}

func TestServiceHealthDetectsCrashLoop(t *testing.T) {
	// With Restart=on-failure and RestartSec=5s (the billing example) a unit
	// that dies at startup sits in "activating (auto-restart)" and never
	// reaches "failed", so it was reported Healthy forever.
	h := serviceHost("enabled", "activating")
	h.AddStub(host.CommandStub{Match: "show --property=SubState", Stdout: "SubState=auto-restart\n"})
	h.AddStub(host.CommandStub{Match: "status", Stdout: "billing.service: Main process exited, code=exited, status=1/FAILURE"})
	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Service\nmetadata:\n  name: billing\n")
	status, detail, err := r.(Checker).Health(newContext(h))
	if err != nil || status != HealthDegraded {
		t.Fatalf("health = %v, %v; want Degraded", status, err)
	}
	if !strings.Contains(detail, "auto-restart") || !strings.Contains(detail, "status=1/FAILURE") {
		t.Errorf("detail should explain the restart loop and include the status tail, got %q", detail)
	}

	// A unit genuinely on its way up is fine.
	h2 := serviceHost("enabled", "activating")
	h2.AddStub(host.CommandStub{Match: "show --property=SubState", Stdout: "SubState=start\n"})
	if status, _, _ := r.(Checker).Health(newContext(h2)); status != HealthHealthy {
		t.Errorf("an activating unit that is starting should be Healthy, got %v", status)
	}
}

func TestServiceObserveFailsWhenSystemctlCannotAnswer(t *testing.T) {
	// Without a reachable manager systemctl prints nothing on stdout and
	// fails. That was reported as "active: unknown" / "enabled: false", so
	// a plan showed a start and an enable that could never happen.
	h := host.NewMem()
	h.AddStub(host.CommandStub{Match: "is-enabled", Stdout: "enabled\n"})
	h.AddStub(host.CommandStub{Match: "is-active", ExitCode: 1,
		Stderr: "System has not been booted with systemd as init system (PID 1). Can't operate.\nFailed to connect to bus: Host is down\n"})
	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Service\nmetadata:\n  name: nginx\n")
	if _, err := r.Observe(newContext(h)); err == nil || !strings.Contains(err.Error(), "has not been booted with systemd") {
		t.Errorf("error = %v, want the systemctl failure", err)
	}

	h2 := host.NewMem()
	h2.AddStub(host.CommandStub{Match: "is-enabled", ExitCode: 1, Stderr: "Failed to connect to bus: No medium found\n"})
	h2.AddStub(host.CommandStub{Match: "is-active", Stdout: "active\n"})
	if _, err := r.Observe(newContext(h2)); err == nil || !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Errorf("error = %v, want the systemctl failure", err)
	}

	// systemd before v237 reported a missing unit this way; that is an
	// answer ("not installed yet"), not a failure.
	h3 := host.NewMem()
	h3.AddStub(host.CommandStub{Match: "is-enabled", ExitCode: 1, Stderr: "Failed to get unit file state for nginx.service: No such file or directory\n"})
	h3.AddStub(host.CommandStub{Match: "is-active", ExitCode: 3, Stdout: "unknown\n"})
	observed, err := r.Observe(newContext(h3))
	if err != nil {
		t.Fatalf("a missing unit is not an error: %v", err)
	}
	if observed["enabled"] != "false" {
		t.Errorf("observed = %v", observed)
	}
}

func TestUnitNamesAreValidated(t *testing.T) {
	for _, bad := range []string{
		"-H",                    // systemctl reads it as an option
		"--global",              //
		"../../../etc/cron.d/x", // escapes the unit directory
		"a/b",
		"foo bar",
		"x;reboot",
	} {
		for _, kind := range []string{"Service", "SystemdUnit"} {
			src := "apiVersion: systemcd.dev/v1\nkind: " + kind + "\nmetadata:\n  name: n\nspec:\n  unit: \"" + bad + "\"\n"
			if kind == "SystemdUnit" {
				src += "  content: \"[Unit]\\n\"\n"
			}
			if err := buildErr(t, src); err == nil || !strings.Contains(err.Error(), "unit name") {
				t.Errorf("%s unit %q: error = %v, want a unit name validation failure", kind, bad, err)
			}
		}
	}
	for _, good := range []string{"nginx", "getty@tty1", "dev-disk-by\\\\x2duuid-1.device", "foo.socket", "user@1000.service"} {
		if err := buildErr(t, "apiVersion: systemcd.dev/v1\nkind: Service\nmetadata:\n  name: n\nspec:\n  unit: \""+good+"\"\n"); err != nil {
			t.Errorf("unit %q should be accepted: %v", good, err)
		}
	}
}

func TestUnitNamesWithoutAUnitTypeBecomeServices(t *testing.T) {
	// A SystemdUnit named "app-v1.2" was written to
	// /etc/systemd/system/app-v1.2, which systemd never loads.
	cases := []struct{ unit, path string }{
		{"app-v1.2", "/etc/systemd/system/app-v1.2.service"},
		{"billing", "/etc/systemd/system/billing.service"},
		{"backup.timer", "/etc/systemd/system/backup.timer"},
		{"app.socket", "/etc/systemd/system/app.socket"},
		{"worker@.service", "/etc/systemd/system/worker@.service"},
	}
	for _, tc := range cases {
		r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: SystemdUnit\nmetadata:\n  name: u\nspec:\n  unit: \""+tc.unit+"\"\n  content: \"[Unit]\\n\"\n")
		if got := ClaimStrings(ClaimsOf(r)); len(got) != 1 || got[0] != "path:"+tc.path {
			t.Errorf("unit %q claims %v, want path:%s", tc.unit, got, tc.path)
		}
	}
	// A Service named the same way claims the same unit systemctl acts on,
	// so two resources for php8.2-fpm cannot slip past conflict detection.
	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Service\nmetadata:\n  name: php8.2-fpm\n")
	if got := ClaimStrings(ClaimsOf(r)); len(got) != 1 || got[0] != "unit:php8.2-fpm.service" {
		t.Errorf("claims = %v, want unit:php8.2-fpm.service", got)
	}
}

func TestSystemdUnitDropIns(t *testing.T) {
	src := "apiVersion: systemcd.dev/v1\nkind: SystemdUnit\nmetadata:\n  name: nginx-limits\nspec:\n  directory: /etc/systemd/system/nginx.service.d\n  unit: %s\n  content: \"[Service]\\nLimitNOFILE=65536\\n\"\n"
	r := buildFrom(t, strings.Replace(src, "%s", "limits.conf", 1))
	if got := ClaimStrings(ClaimsOf(r)); got[0] != "path:/etc/systemd/system/nginx.service.d/limits.conf" {
		t.Errorf("claims = %v", got)
	}
	// The unit the drop-in extends is the one whose reload state matters.
	h := host.NewMem()
	h.AddStub(host.CommandStub{Match: "show --property=NeedDaemonReload nginx.service", Stdout: "NeedDaemonReload=no\n"})
	converge(t, r, newContext(h))
	assertConverged(t, r, newContext(h))
	if !h.Ran("systemctl show --property=NeedDaemonReload nginx.service") {
		t.Errorf("commands = %v", h.Commands)
	}

	// systemd ignores anything but *.conf in a drop-in directory.
	if err := buildErr(t, strings.Replace(src, "%s", "limits", 1)); err == nil || !strings.Contains(err.Error(), "*.conf") {
		t.Errorf("error = %v, want a drop-in naming failure", err)
	}
	// And a *.conf outside one is a drop-in that lost its directory.
	if err := buildErr(t, "apiVersion: systemcd.dev/v1\nkind: SystemdUnit\nmetadata:\n  name: x\nspec:\n  unit: override.conf\n  content: \"x\"\n"); err == nil || !strings.Contains(err.Error(), "drop-in") {
		t.Errorf("error = %v, want a drop-in placement failure", err)
	}
}

func TestSystemdUnitDirectoryAndScopeAreValidated(t *testing.T) {
	for _, spec := range []string{
		"  directory: /etc/systemd/system/../../cron.d\n",
		"  directory: etc/systemd/system\n",
		"  directory: /etc/systemd//system\n",
		"  scope: usr\n",
	} {
		err := buildErr(t, "apiVersion: systemcd.dev/v1\nkind: SystemdUnit\nmetadata:\n  name: app\nspec:\n  content: \"x\"\n"+spec)
		if err == nil {
			t.Errorf("spec %q should be rejected", strings.TrimSpace(spec))
		}
	}
	// A trailing slash is harmless.
	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: SystemdUnit\nmetadata:\n  name: app\nspec:\n  content: \"x\"\n  directory: /run/systemd/system/\n")
	if got := ClaimStrings(ClaimsOf(r)); got[0] != "path:/run/systemd/system/app.service" {
		t.Errorf("claims = %v", got)
	}
}

func TestSystemdUnitRetriesAFailedDaemonReload(t *testing.T) {
	// The file is written, then daemon-reload fails. On the next run the
	// file is in sync, so without asking systemd whether its loaded copy is
	// stale the reload was never retried and the old unit kept running.
	h := &scriptedHost{MemHost: host.NewMem()}
	stale := false
	h.respond = func(name string, args []string) (host.Result, bool) {
		if name != "systemctl" {
			return host.Result{}, false
		}
		switch strings.Join(args, " ") {
		case "show --property=NeedDaemonReload app.service":
			if stale {
				return host.Result{Stdout: "NeedDaemonReload=yes\n"}, true
			}
			return host.Result{Stdout: "NeedDaemonReload=no\n"}, true
		}
		return host.Result{}, false
	}
	h.AddStub(host.CommandStub{Match: "daemon-reload", ExitCode: 1, Stderr: "Failed to reload daemon: Connection timed out\n"})
	c := newContext(h)
	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: SystemdUnit\nmetadata:\n  name: app\nspec:\n  content: \"[Service]\\nExecStart=/bin/app\\n\"\n")

	desired, _ := r.Desired(c)
	observed, _ := r.Observe(c)
	if err := r.Apply(c, Compare(desired, observed, nil)); err == nil {
		t.Fatal("the daemon-reload failure should surface")
	}
	stale = true // systemd still has the old (here: no) definition loaded

	h.Stubs = nil
	h.AddStub(host.CommandStub{Match: "daemon-reload", Do: func(*host.MemHost, []string) { stale = false }})
	d := converge(t, r, c)
	if !d.Has("loaded") || !h.Ran("systemctl daemon-reload") {
		t.Fatalf("a stale unit should be reported and reloaded; diff = %+v", d)
	}
	assertConverged(t, r, c)
}

func TestSystemdUnitWithoutARunningManagerIsNotStale(t *testing.T) {
	// In a chroot or image build `systemctl show` cannot reach a manager;
	// there is nothing loaded to be out of date.
	h := host.NewMem()
	h.AddStub(host.CommandStub{Match: "systemctl show", ExitCode: 1, Stderr: "System has not been booted with systemd as init system (PID 1). Can't operate.\n"})
	c := newContext(h)
	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: SystemdUnit\nmetadata:\n  name: app\nspec:\n  content: \"x\"\n")
	converge(t, r, c)
	assertConverged(t, r, c)
}
