package resource

import (
	"context"
	"strings"
	"testing"

	"github.com/vikki8/systemcd/internal/host"
	"github.com/vikki8/systemcd/internal/manifest"
)

// buildFrom compiles a single YAML document into a resource.
func buildFrom(t *testing.T, src string) Resource {
	t.Helper()
	docs, err := manifest.Parse([]byte(src), "test.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d documents, want 1", len(docs))
	}
	r, err := Build(docs[0])
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return r
}

func buildErr(t *testing.T, src string) error {
	t.Helper()
	docs, err := manifest.Parse([]byte(src), "test.yaml")
	if err != nil {
		return err
	}
	_, err = Build(docs[0])
	return err
}

// converge observes, diffs and applies once, returning the diff that was
// applied. It is the single-resource equivalent of a reconcile.
func converge(t *testing.T, r Resource, c *Context) Diff {
	t.Helper()
	desired, err := r.Desired(c)
	if err != nil {
		t.Fatalf("desired: %v", err)
	}
	observed, err := r.Observe(c)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	var sensitive map[string]bool
	if s, ok := r.(Sensitiver); ok {
		sensitive = s.SensitiveFields()
	}
	d := Compare(desired, observed, sensitive)
	if d.Empty() {
		return d
	}
	if err := r.Apply(c, d); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return d
}

// assertConverged re-runs observe/diff and fails if anything still differs.
// Every resource must be idempotent: a second apply is always a no-op.
func assertConverged(t *testing.T, r Resource, c *Context) {
	t.Helper()
	desired, err := r.Desired(c)
	if err != nil {
		t.Fatalf("desired: %v", err)
	}
	observed, err := r.Observe(c)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if d := Compare(desired, observed, nil); !d.Empty() {
		t.Fatalf("resource is not idempotent, still drifting: %+v", d)
	}
}

func newContext(h host.Host) *Context {
	return &Context{Ctx: context.Background(), Host: h}
}

func TestFileCreatesWithModeAndOwner(t *testing.T) {
	h := host.NewMem()
	h.Users["www-data"] = 33
	h.Groups["www-data"] = 33
	c := newContext(h)

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: nginx-conf
spec:
  path: /etc/nginx/nginx.conf
  content: |
    worker_processes auto;
  mode: "0640"
  owner: www-data
  group: www-data
`)

	d := converge(t, r, c)
	if !d.Has("state") {
		t.Errorf("expected a state change on creation, got %+v", d)
	}

	got, err := h.ReadFile("/etc/nginx/nginx.conf")
	if err != nil {
		t.Fatalf("file was not written: %v", err)
	}
	if string(got) != "worker_processes auto;\n" {
		t.Errorf("content = %q", got)
	}
	info, err := h.Stat("/etc/nginx/nginx.conf")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode.Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", info.Mode.Perm())
	}
	if info.UID != 33 || info.GID != 33 {
		t.Errorf("ownership = %d:%d, want 33:33", info.UID, info.GID)
	}
	assertConverged(t, r, c)
}

func TestFileDetectsContentDriftAndBacksUp(t *testing.T) {
	h := host.NewMem()
	h.SetFile("/etc/motd", "hand edited by someone at 3am\n", 0o644, 0, 0)
	c := newContext(h)

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: motd
spec:
  path: /etc/motd
  content: "managed by systemcd\n"
`)

	d := converge(t, r, c)
	if !d.Has("checksum") {
		t.Errorf("expected checksum drift, got %+v", d)
	}
	got, _ := h.ReadFile("/etc/motd")
	if string(got) != "managed by systemcd\n" {
		t.Errorf("content = %q", got)
	}

	// The overwritten content must be recoverable.
	var backup string
	for _, p := range h.Paths() {
		if strings.HasPrefix(p, "/var/lib/systemcd/backups/etc_motd.") {
			backup = p
		}
	}
	if backup == "" {
		t.Fatalf("no backup was written; paths = %v", h.Paths())
	}
	data, _ := h.ReadFile(backup)
	if string(data) != "hand edited by someone at 3am\n" {
		t.Errorf("backup content = %q", data)
	}
	assertConverged(t, r, c)
}

func TestFileModeOnlyDriftDoesNotRewriteContent(t *testing.T) {
	h := host.NewMem()
	h.SetFile("/etc/secret", "token\n", 0o666, 0, 0)
	c := newContext(h)

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: secret
spec:
  path: /etc/secret
  content: "token\n"
  mode: "0600"
`)
	d := converge(t, r, c)
	if len(d) != 1 || d[0].Field != "mode" {
		t.Fatalf("expected only a mode diff, got %+v", d)
	}
	if len(h.Paths()) != 3 { // "/", "/etc", "/etc/secret" — no backup taken
		t.Errorf("a mode fix should not create a backup; paths = %v", h.Paths())
	}
	info, _ := h.Stat("/etc/secret")
	if info.Mode.Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode.Perm())
	}
	assertConverged(t, r, c)
}

func TestFileAbsentRemoves(t *testing.T) {
	h := host.NewMem()
	h.SetFile("/etc/nginx/conf.d/default.conf", "server {}\n", 0o644, 0, 0)
	c := newContext(h)

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: default-site
spec:
  path: /etc/nginx/conf.d/default.conf
  state: absent
`)
	converge(t, r, c)
	if _, err := h.Stat("/etc/nginx/conf.d/default.conf"); err == nil {
		t.Error("file should have been removed")
	}
	assertConverged(t, r, c)
}

func TestFileValidationErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"relative path", "spec:\n  path: etc/x\n", "must be absolute"},
		{"missing path", "spec: {}\n", "spec.path is required"},
		{"content and source", "spec:\n  path: /a\n  content: x\n  source: f\n", "mutually exclusive"},
		{"bad mode", "spec:\n  path: /a\n  mode: \"rwxr\"\n", "octal"},
		{"bad state", "spec:\n  path: /a\n  state: enabled\n", "state must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := buildErr(t, "apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: t\n"+tc.src)
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestServiceEnablesAndStarts(t *testing.T) {
	h := host.NewMem()
	enabled, active := "disabled", "inactive"
	h.AddStub(host.CommandStub{Match: "is-enabled", Stdout: enabled})
	h.AddStub(host.CommandStub{Match: "is-active", Stdout: active})
	c := newContext(h)

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: Service
metadata:
  name: nginx
spec:
  enabled: true
  state: started
`)
	d := converge(t, r, c)
	if !d.Has("enabled") || !d.Has("active") {
		t.Fatalf("expected enabled and active drift, got %+v", d)
	}
	if !h.Ran("systemctl enable nginx.service") {
		t.Errorf("did not enable the unit; commands = %v", h.Commands)
	}
	if !h.Ran("systemctl start nginx.service") {
		t.Errorf("did not start the unit; commands = %v", h.Commands)
	}
}

func TestServiceTreatsStaticUnitsAsEnabled(t *testing.T) {
	// A static unit cannot be enabled. Reporting it as disabled would make
	// every reconcile attempt (and fail) an enable, forever.
	h := host.NewMem()
	h.AddStub(host.CommandStub{Match: "is-enabled", Stdout: "static\n"})
	h.AddStub(host.CommandStub{Match: "is-active", Stdout: "active\n"})
	c := newContext(h)

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: Service
metadata:
  name: dbus
spec:
  enabled: true
`)
	assertConverged(t, r, c)
}

func TestServiceTransientStatesSettle(t *testing.T) {
	h := host.NewMem()
	h.AddStub(host.CommandStub{Match: "is-enabled", Stdout: "enabled"})
	h.AddStub(host.CommandStub{Match: "is-active", Stdout: "activating"})
	c := newContext(h)

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: Service
metadata:
  name: slow-starter
spec:
  enabled: true
  state: started
`)
	// "activating" is heading for "active"; restarting it mid-transition
	// would be counterproductive.
	assertConverged(t, r, c)
}

func TestServiceRefreshUsesReloadWhenAsked(t *testing.T) {
	h := host.NewMem()
	c := newContext(h)
	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: Service
metadata:
  name: nginx
spec:
  reload: true
`)
	if err := r.(Refreshable).Refresh(c); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !h.Ran("systemctl reload-or-restart nginx.service") {
		t.Errorf("commands = %v", h.Commands)
	}
}

func TestServiceHealthReportsDegraded(t *testing.T) {
	h := host.NewMem()
	h.AddStub(host.CommandStub{Match: "is-active", Stdout: "failed"})
	h.AddStub(host.CommandStub{Match: "status", Stdout: "nginx.service: Failed with result 'exit-code'."})
	c := newContext(h)

	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Service\nmetadata:\n  name: nginx\n")
	status, detail, err := r.(Checker).Health(c)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if status != HealthDegraded {
		t.Errorf("status = %v, want Degraded", status)
	}
	if !strings.Contains(detail, "exit-code") {
		t.Errorf("detail should include the journal tail, got %q", detail)
	}
}

func TestSystemdUnitWritesFileAndReloads(t *testing.T) {
	h := host.NewMem()
	c := newContext(h)

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: SystemdUnit
metadata:
  name: billing-api
spec:
  content: |
    [Unit]
    Description=Billing API
    [Service]
    ExecStart=/usr/local/bin/billing
`)
	converge(t, r, c)

	data, err := h.ReadFile("/etc/systemd/system/billing-api.service")
	if err != nil {
		t.Fatalf("unit file not written: %v", err)
	}
	if !strings.Contains(string(data), "ExecStart=/usr/local/bin/billing") {
		t.Errorf("unit content = %q", data)
	}
	if !h.Ran("systemctl daemon-reload") {
		t.Errorf("daemon-reload was not run; commands = %v", h.Commands)
	}
	assertConverged(t, r, c)
}

func TestPackageInstallsViaDetectedManager(t *testing.T) {
	h := host.NewMem()
	h.Binaries["apt-get"] = true
	installed := false
	h.AddStub(host.CommandStub{Match: "dpkg-query", ExitCode: 1, Do: func(m *host.MemHost, args []string) {}})
	h.AddStub(host.CommandStub{Match: "apt-get install", Do: func(m *host.MemHost, args []string) { installed = true }})
	c := newContext(h)

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: Package
metadata:
  name: web-stack
spec:
  names: [nginx, curl]
`)
	d := converge(t, r, c)
	if len(d) != 2 {
		t.Fatalf("expected both packages to be missing, got %+v", d)
	}
	if !installed {
		t.Errorf("apt-get install was not run; commands = %v", h.Commands)
	}
	// Both packages go out in a single invocation; the order is the diff's
	// sorted order, which keeps runs reproducible.
	if !h.Ran("apt-get install -y --no-install-recommends 'curl' 'nginx'") {
		t.Errorf("both packages should be installed in one call; commands = %v", h.Commands)
	}
}

func TestPackageNoManagerIsAClearError(t *testing.T) {
	h := host.NewMem() // no binaries registered
	c := newContext(h)
	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Package\nmetadata:\n  name: nginx\n")
	if _, err := r.Observe(c); err == nil || !strings.Contains(err.Error(), "no supported package manager") {
		t.Fatalf("error = %v, want a clear 'no supported package manager' message", err)
	}
}

func TestPackageReportsInstalledVersion(t *testing.T) {
	h := host.NewMem()
	h.Binaries["apt-get"] = true
	h.AddStub(host.CommandStub{Match: "dpkg-query", Stdout: "installed 1.24.0-2ubuntu7"})
	c := newContext(h)

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: Package
metadata:
  name: nginx
spec:
  version: "1.24.0-2ubuntu7"
`)
	assertConverged(t, r, c)

	// A different installed version must read as drift.
	h2 := host.NewMem()
	h2.Binaries["apt-get"] = true
	h2.AddStub(host.CommandStub{Match: "dpkg-query", Stdout: "installed 1.18.0-0ubuntu1"})
	c2 := newContext(h2)
	desired, _ := r.Desired(c2)
	observed, _ := r.Observe(c2)
	if d := Compare(desired, observed, nil); d.Empty() {
		t.Error("a version mismatch should be reported as drift")
	}
}

func TestExecGuards(t *testing.T) {
	t.Run("creates suppresses the command", func(t *testing.T) {
		h := host.NewMem()
		h.SetFile("/var/lib/app/.initialized", "", 0o644, 0, 0)
		c := newContext(h)
		r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: Exec
metadata:
  name: init-db
spec:
  command: /usr/local/bin/init-db
  creates: /var/lib/app/.initialized
`)
		assertConverged(t, r, c)
		if len(h.Commands) != 0 {
			t.Errorf("guarded command should not run; commands = %v", h.Commands)
		}
	})

	t.Run("missing creates path runs the command", func(t *testing.T) {
		h := host.NewMem()
		c := newContext(h)
		r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: Exec
metadata:
  name: init-db
spec:
  command: /usr/local/bin/init-db
  creates: /var/lib/app/.initialized
`)
		converge(t, r, c)
		if !h.Ran("/usr/local/bin/init-db") {
			t.Errorf("commands = %v", h.Commands)
		}
	})

	t.Run("onlyIf that fails makes the resource satisfied", func(t *testing.T) {
		h := host.NewMem()
		h.AddStub(host.CommandStub{Match: "test -d /opt/app", ExitCode: 1})
		c := newContext(h)
		r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: Exec
metadata:
  name: reindex
spec:
  command: /opt/app/bin/reindex
  onlyIf: test -d /opt/app
`)
		assertConverged(t, r, c)
		if h.Ran("/opt/app/bin/reindex") {
			t.Errorf("command should not run when onlyIf fails; commands = %v", h.Commands)
		}
	})

	t.Run("an unguarded Exec is rejected", func(t *testing.T) {
		err := buildErr(t, `
apiVersion: systemcd.dev/v1
kind: Exec
metadata:
  name: dangerous
spec:
  command: rm -rf /var/cache/app
`)
		if err == nil || !strings.Contains(err.Error(), "needs a guard") {
			t.Fatalf("error = %v, want a guard requirement", err)
		}
	})
}

func TestSysctlAppliesAndPersists(t *testing.T) {
	h := host.NewMem()
	h.AddStub(host.CommandStub{Match: "sysctl -n net.ipv4.ip_forward", Stdout: "0\n"})
	h.AddStub(host.CommandStub{Match: "sysctl -w", Do: func(m *host.MemHost, args []string) {}})
	c := newContext(h)

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: Sysctl
metadata:
  name: routing
spec:
  key: net.ipv4.ip_forward
  value: "1"
`)
	d := converge(t, r, c)
	if !d.Has("sysctl:net.ipv4.ip_forward") {
		t.Fatalf("expected sysctl drift, got %+v", d)
	}
	if !h.Ran("sysctl -w net.ipv4.ip_forward=1") {
		t.Errorf("commands = %v", h.Commands)
	}
	data, err := h.ReadFile("/etc/sysctl.d/60-systemcd-routing.conf")
	if err != nil {
		t.Fatalf("drop-in not written: %v", err)
	}
	if !strings.Contains(string(data), "net.ipv4.ip_forward = 1") {
		t.Errorf("drop-in = %q", data)
	}
}

func TestSysctlNormalizesMultiValueParameters(t *testing.T) {
	// The kernel separates these with tabs; a manifest would use spaces.
	h := host.NewMem()
	h.AddStub(host.CommandStub{Match: "sysctl -n net.ipv4.tcp_rmem", Stdout: "4096\t87380\t6291456\n"})
	c := newContext(h)

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: Sysctl
metadata:
  name: tcp
spec:
  key: net.ipv4.tcp_rmem
  value: "4096 87380 6291456"
  persist: false
`)
	desired, _ := r.Desired(c)
	observed, _ := r.Observe(c)
	if d := Compare(desired, observed, nil); !d.Empty() {
		t.Errorf("tab and space separated values should compare equal, got %+v", d)
	}
}

func TestUserCreatesWithSuppliedAttributes(t *testing.T) {
	h := host.NewMem()
	h.AddStub(host.CommandStub{Match: "getent passwd deploy", ExitCode: 2})
	c := newContext(h)

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: User
metadata:
  name: deploy
spec:
  system: true
  shell: /usr/sbin/nologin
  home: /var/lib/deploy
`)
	converge(t, r, c)
	if !h.Ran("useradd --system --home-dir /var/lib/deploy --shell /usr/sbin/nologin --no-create-home deploy") {
		t.Errorf("commands = %v", h.Commands)
	}
}

func TestUserOnlyReportsRequestedGroupMembership(t *testing.T) {
	// The account belongs to extra groups systemcd was never told about.
	// Those must not read as drift, or every run would fight the machine.
	h := host.NewMem()
	h.AddStub(host.CommandStub{Match: "getent passwd deploy", Stdout: "deploy:x:997:997::/var/lib/deploy:/usr/sbin/nologin"})
	h.AddStub(host.CommandStub{Match: "id -nG deploy", Stdout: "deploy docker sudo systemd-journal"})
	c := newContext(h)

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: User
metadata:
  name: deploy
spec:
  shell: /usr/sbin/nologin
  home: /var/lib/deploy
  groups: [docker]
`)
	assertConverged(t, r, c)
}

func TestCompareIgnoresInformationalKeys(t *testing.T) {
	d := Compare(
		State{"mode": "0644", "_bytes": "10"},
		State{"mode": "0644", "_bytes": "999"},
		nil,
	)
	if !d.Empty() {
		t.Errorf("underscore-prefixed keys must not produce drift, got %+v", d)
	}
}

func TestCompareMarksSensitiveFields(t *testing.T) {
	d := Compare(State{"checksum": "sha256:new"}, State{"checksum": "sha256:old"}, map[string]bool{"checksum": true})
	if len(d) != 1 || !d[0].Sensitive {
		t.Fatalf("diff = %+v, want one sensitive field", d)
	}
}
