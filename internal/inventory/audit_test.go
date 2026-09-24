package inventory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vikki8/systemcd/internal/engine"
	"github.com/vikki8/systemcd/internal/host"
	"github.com/vikki8/systemcd/internal/manifest"
	"github.com/vikki8/systemcd/internal/resource"
)

func skippedText(rep *Report) string { return strings.Join(rep.Skipped, "\n") }

func TestDpkgVerifyKeepsPathsWithSpacesAndSkipsSecrets(t *testing.T) {
	h := host.NewMem()
	h.Binaries["dpkg"] = true
	// The real format, as printed by dpkg 1.22: flags, space, attribute, space, path.
	h.AddStub(host.CommandStub{Match: "dpkg -V", Stdout: "??5?????? c /etc/app/my file.conf\n??5?????? c /etc/sudoers\nmissing     /usr/share/doc/x\n"})
	rep, err := Scan(context.Background(), Options{Host: h, Categories: []Category{CategoryModifiedConfig}})
	if err != nil {
		t.Fatal(err)
	}
	got := claims(rep, CategoryModifiedConfig)
	if !has(got, "path:/etc/app/my file.conf") {
		t.Errorf("modified configs = %q, want the full path with its space", got)
	}
	if has(got, "path:/etc/sudoers") {
		t.Errorf("modified configs = %q, secrets must not be offered", got)
	}
}

func TestVerifyFailuresAreReportedNotReadAsClean(t *testing.T) {
	cases := []struct {
		name   string
		binary string
		stubs  []host.CommandStub
		want   string
	}{
		{"dpkg", "dpkg", []host.CommandStub{{Match: "dpkg -V", ExitCode: 2, Stderr: "dpkg: error: cannot access archive"}}, "cannot access archive"},
		{"rpm", "rpm", []host.CommandStub{{Match: "rpm -Va", ExitCode: 1, Stderr: "error: cannot open Packages database"}}, "cannot open Packages database"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := host.NewMem()
			h.Binaries[tc.binary] = true
			for _, s := range tc.stubs {
				h.AddStub(s)
			}
			rep, err := Scan(context.Background(), Options{Host: h, Categories: []Category{CategoryModifiedConfig}})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(skippedText(rep), tc.want) {
				t.Errorf("skipped = %v, want the failure reported", rep.Skipped)
			}
		})
	}
}

func TestRpmVerifyChecksFiles(t *testing.T) {
	// `--nofiles` turns off exactly the check this scan exists for; with it,
	// rpm verifies dependencies only and every RPM host reads as clean.
	h := host.NewMem()
	h.Binaries["rpm"] = true
	h.AddStub(host.CommandStub{Match: "--nofiles"})
	h.AddStub(host.CommandStub{Match: "rpm -Va", ExitCode: 1, Stdout: "S.5....T.  c /etc/ssh/sshd_config\n.M.......    /usr/bin/x\n"})
	rep, err := Scan(context.Background(), Options{Host: h, Categories: []Category{CategoryModifiedConfig}})
	if err != nil {
		t.Fatal(err)
	}
	if got := claims(rep, CategoryModifiedConfig); !has(got, "path:/etc/ssh/sshd_config") {
		t.Errorf("modified configs = %v (commands %v), want sshd_config", got, h.Commands)
	}
	if len(rep.Skipped) != 0 {
		t.Errorf("skipped = %v; exit 1 with a report is rpm's normal \"something differs\"", rep.Skipped)
	}
}

func TestManualPackageFailureIsReported(t *testing.T) {
	h := host.NewMem()
	h.Binaries["apt-mark"] = true
	h.AddStub(host.CommandStub{Match: "apt-mark showmanual", ExitCode: 100, Stderr: "E: could not open lock file"})
	rep, err := Scan(context.Background(), Options{Host: h, Categories: []Category{CategoryManualPackage}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(skippedText(rep), "could not open lock file") {
		t.Errorf("skipped = %v, want the apt-mark failure", rep.Skipped)
	}
}

func TestOwnershipQueryFailureIsReported(t *testing.T) {
	// dpkg-query exit 2 is dpkg-query failing, not "unowned"; reading it as
	// unowned listed every file under /etc.
	h := host.NewMem()
	h.Binaries["dpkg-query"] = true
	h.SetFile("/etc/hosts", "127.0.0.1 localhost\n", 0o644, 0, 0)
	h.AddStub(host.CommandStub{Match: "dpkg-query -S", ExitCode: 2, Stderr: "dpkg-query: error: database is locked"})
	rep, err := Scan(context.Background(), Options{Host: h, Categories: []Category{CategoryUnpackagedConfig}})
	if err != nil {
		t.Fatal(err)
	}
	if got := claims(rep, CategoryUnpackagedConfig); len(got) != 0 {
		t.Errorf("unpackaged = %v, want nothing claimed from a failed query", got)
	}
	if !strings.Contains(skippedText(rep), "database is locked") {
		t.Errorf("skipped = %v, want the failure reported", rep.Skipped)
	}
}

func TestOwnershipQueryIsLiteral(t *testing.T) {
	// dpkg-query -S treats its argument as a glob.
	h := host.NewMem()
	h.Binaries["dpkg-query"] = true
	h.SetFile("/etc/app/foo[1].conf", "x\n", 0o644, 0, 0)
	h.AddStub(host.CommandStub{Match: `dpkg-query -S /etc/app/foo\[1\].conf`, ExitCode: 1})
	h.AddStub(host.CommandStub{Match: "dpkg-query -S", Stdout: "somepkg: /etc/app/foo1.conf"})
	rep, err := Scan(context.Background(), Options{Host: h, Categories: []Category{CategoryUnpackagedConfig}})
	if err != nil {
		t.Fatal(err)
	}
	if got := claims(rep, CategoryUnpackagedConfig); !has(got, "path:/etc/app/foo[1].conf") {
		t.Errorf("unpackaged = %v (commands %v), want the file looked up literally", got, h.Commands)
	}
}

func TestScanSkipsSymlinksAndUnitLinkFarms(t *testing.T) {
	h := host.NewMem()
	h.Binaries["dpkg-query"] = true
	h.AddStub(host.CommandStub{Match: "dpkg-query -S", ExitCode: 1})
	h.SetFile("/etc/systemd/system/billing.service", "[Service]\n", 0o644, 0, 0)
	h.SetFile("/etc/sysctl.d/99-tuning.conf", "vm.swappiness = 10\n", 0o644, 0, 0)
	for link, target := range map[string]string{
		"/etc/resolv.conf":                                    "../run/systemd/resolve/stub-resolv.conf",
		"/etc/systemd/system/redis.service":                   "/usr/lib/systemd/system/redis-server.service",
		"/etc/systemd/system/masked.service":                  "/dev/null",
		"/etc/systemd/system/timers.target.wants/apt.timer":   "/usr/lib/systemd/system/apt.timer",
		"/etc/sysctl.d/99-sysctl.conf":                        "../sysctl.conf",
		"/etc/systemd/system/network.target.requires/x.mount": "/etc/systemd/system/x.mount",
	} {
		if err := h.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.MkdirAll("/etc/systemd/system/multi.target.requires", 0o755); err != nil {
		t.Fatal(err)
	}
	rep, err := Scan(context.Background(), Options{Host: h, Categories: []Category{
		CategoryUnpackagedConfig, CategoryLocalUnit, CategorySysctlDropIn,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range rep.Items {
		for _, bad := range []string{"resolv.conf", "redis", "masked", "apt.timer", "99-sysctl", "requires"} {
			if strings.Contains(item.Claim, bad) {
				t.Errorf("%s reported %s, which is a link or a directory, not a file somebody wrote", item.Category, item.Claim)
			}
		}
	}
	if got := claims(rep, CategoryLocalUnit); !has(got, "path:/etc/systemd/system/billing.service") {
		t.Errorf("local units = %v, want the real unit file", got)
	}
}

func TestScanSaysWhenItTruncates(t *testing.T) {
	h := host.NewMem()
	h.Binaries["dpkg-query"] = true
	h.AddStub(host.CommandStub{Match: "dpkg-query -S", ExitCode: 1})
	h.SetFile("/etc/a.conf", "a\n", 0o644, 0, 0)
	h.SetFile("/etc/b.conf", "b\n", 0o644, 0, 0)
	rep, err := Scan(context.Background(), Options{Host: h, Limit: 1, Categories: []Category{CategoryUnpackagedConfig}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Items) != 1 || !strings.Contains(skippedText(rep), "stopped after 1") {
		t.Errorf("items = %v, skipped = %v, want one item and a truncation note", rep.Items, rep.Skipped)
	}
}

func TestScanRejectsUnknownCategoriesAndMissingPaths(t *testing.T) {
	h := host.NewMem()
	if _, err := Scan(context.Background(), Options{Host: h, Categories: []Category{"bogus"}}); err == nil || !strings.Contains(err.Error(), "unknown category") {
		t.Errorf("unknown category: err = %v", err)
	}
	if _, err := Scan(context.Background(), Options{Host: h, Paths: []string{"/nonexistent"}}); err == nil || !strings.Contains(err.Error(), "/nonexistent") {
		t.Errorf("missing path: err = %v", err)
	}
}

// fleetHost is an inherited box with the awkward parts real ones have: one
// path found by several scans, a drop-in, symlinks the package manager and
// systemctl leave behind, secrets, and an enabled oneshot that is not running.
func fleetHost(t *testing.T) *host.MemHost {
	t.Helper()
	h := host.NewMem()
	h.Users["www-data"] = 33
	h.Groups["www-data"] = 33
	for _, b := range []string{"dpkg", "dpkg-query", "apt-mark"} {
		h.Binaries[b] = true
	}
	h.AddStub(host.CommandStub{Match: "dpkg -V", Stdout: "??5?????? c /etc/nginx/nginx.conf\n??5?????? c /etc/app/my file.conf\n"})
	for _, p := range []string{
		"/etc/local-thing.conf", "/etc/systemd/system/billing.service",
		"/etc/systemd/system/nginx.service.d/override.conf", "/etc/sysctl.d/99-tuning.conf",
		"/etc/app/secret.conf", "/etc/ssl/private/snakeoil.key", "/etc/hostname",
	} {
		h.AddStub(host.CommandStub{Match: "dpkg-query -S " + p, ExitCode: 1})
	}
	h.AddStub(host.CommandStub{Match: "dpkg-query -S", Stdout: "somepkg: /etc/somewhere"})
	h.AddStub(host.CommandStub{Match: "list-unit-files", Stdout: "nginx.service enabled enabled\nbilling.service enabled enabled\ne2scrub_reap.service enabled enabled\n"})
	h.AddStub(host.CommandStub{Match: "is-active e2scrub_reap.service", Stdout: "inactive\n", ExitCode: 3})
	h.AddStub(host.CommandStub{Match: "is-active", Stdout: "active\n"})
	h.AddStub(host.CommandStub{Match: "is-enabled", Stdout: "enabled\n"})
	h.AddStub(host.CommandStub{Match: "apt-mark showmanual", Stdout: "nginx\n"})
	h.AddStub(host.CommandStub{Match: "getent passwd", Stdout: "deploy:x:1001:1001::/home/deploy:/bin/bash\n"})

	h.SetFile("/etc/nginx/nginx.conf", "worker_processes 4;\n", 0o640, 0, 33)
	h.SetFile("/etc/app/my file.conf", "listen = 8080\n", 0o644, 33, 33)
	h.SetFile("/etc/local-thing.conf", "written by a human in 2020\n", 0o644, 0, 0)
	h.SetFile("/etc/systemd/system/billing.service", "[Service]\nExecStart=/usr/local/bin/billing\n", 0o644, 0, 0)
	h.SetFile("/etc/systemd/system/nginx.service.d/override.conf", "[Service]\nLimitNOFILE=65536\n", 0o644, 0, 0)
	h.SetFile("/etc/sysctl.d/99-tuning.conf", "net.core.somaxconn = 1024\n", 0o644, 0, 0)
	h.SetFile("/etc/app/secret.conf", "password=hunter2\n", 0o600, 0, 0)
	h.SetFile("/etc/ssl/private/snakeoil.key", "-----BEGIN PRIVATE KEY-----\n", 0o644, 0, 0)
	// Per-machine identity: copied into a role-wide manifest it would rename
	// every host in the role.
	h.SetFile("/etc/hostname", "app01\n", 0o644, 0, 0)
	for link, target := range map[string]string{
		"/etc/resolv.conf":                                  "../run/systemd/resolve/stub-resolv.conf",
		"/etc/systemd/system/redis.service":                 "/usr/lib/systemd/system/redis-server.service",
		"/etc/systemd/system/timers.target.wants/apt.timer": "/usr/lib/systemd/system/apt.timer",
	} {
		if err := h.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func TestGeneratedRepositoryValidatesAndPlansInSync(t *testing.T) {
	h := fleetHost(t)
	rep := scan(t, h, nil)
	res, err := Generate(rep, GenerateOptions{Host: h, Dir: "/srv/fleet", Targets: map[string]string{"role": "app"}})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	body, err := h.ReadFile(res.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"resolv.conf", "redis", "apt.timer", "secret.conf", "snakeoil", "hunter2", "/etc/hostname"} {
		if strings.Contains(string(body), bad) {
			t.Errorf("generated manifest mentions %q:\n%s", bad, body)
		}
	}
	for _, p := range res.FilePaths {
		data, _ := h.ReadFile("/srv/fleet/" + p)
		if strings.Contains(string(data), "hunter2") || strings.Contains(string(data), "PRIVATE KEY") {
			t.Errorf("payload %s holds a secret", p)
		}
	}

	docs, err := manifest.Parse(body, "manifests/discovered.yaml")
	if err != nil {
		t.Fatalf("generated manifest does not parse: %v\n%s", err, body)
	}
	repo := &manifest.Repository{Root: t.TempDir(), Documents: docs}
	if err := repo.Validate(); err != nil {
		t.Fatalf("generated manifest does not validate: %v\n%s", err, body)
	}
	if err := engine.Check(repo); err != nil {
		t.Fatalf("generated manifest does not build: %v\n%s", err, body)
	}

	// Every path resource must claim the path it was generated from.
	claimed := map[string]bool{}
	for _, d := range docs {
		r, err := resource.Build(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range resource.ClaimsOf(r) {
			claimed[c.String()] = true
		}
	}
	for _, want := range []string{
		"path:/etc/app/my file.conf",
		"path:/etc/systemd/system/nginx.service.d/override.conf",
		"path:/etc/systemd/system/billing.service",
		"path:/etc/sysctl.d/99-tuning.conf",
	} {
		if !claimed[want] {
			t.Errorf("no generated resource claims %s; claims = %v", want, claimed)
		}
	}

	// The README's promise: generate, then plan against the result, and
	// nothing needs to change.
	for _, p := range res.FilePaths {
		data, err := h.ReadFile("/srv/fleet/" + p)
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(repo.Root, p)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := engine.Reconcile(context.Background(), engine.Options{
		Repo: repo, Host: h, DryRun: true,
		Node: manifest.Node{Hostname: "testhost", Labels: map[string]string{"role": "app"}},
		Only: []string{"File/*", "SystemdUnit/*", "Service/*"},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Results) == 0 {
		t.Fatal("plan selected nothing")
	}
	for _, r := range plan.Results {
		if r.Action != engine.ActionNoop {
			t.Errorf("%s: %s %v %v; a fresh snapshot should be in sync", r.ID, r.Action, r.Diff, r.Err)
		}
	}
}

func TestGenerateQuotesValues(t *testing.T) {
	h := host.NewMem()
	h.SetFile("/etc/app/it #1: main.conf", "x\n", 0o644, 0, 0)
	rep := &Report{Hostname: "h", Items: []Item{{Category: CategoryUnpackagedConfig, Claim: "path:/etc/app/it #1: main.conf"}}}
	res, err := Generate(rep, GenerateOptions{Host: h, Dir: "/srv/fleet", Targets: map[string]string{"tier": "yes: really"}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := h.ReadFile(res.ManifestPath)
	docs, err := manifest.Parse(body, "discovered.yaml")
	if err != nil || len(docs) != 1 {
		t.Fatalf("parse: %v\n%s", err, body)
	}
	var spec struct {
		Path   string `yaml:"path"`
		Source string `yaml:"source"`
		Mode   string `yaml:"mode"`
		Owner  string `yaml:"owner"`
		Group  string `yaml:"group"`
	}
	if err := docs[0].DecodeSpec(&spec); err != nil {
		t.Fatal(err)
	}
	if spec.Path != "/etc/app/it #1: main.conf" || spec.Mode != "0644" {
		t.Errorf("spec = %+v, want the path and mode back verbatim:\n%s", spec, body)
	}
	if got := docs[0].Metadata.Targets.Labels["tier"]; got != "yes: really" {
		t.Errorf("label = %q:\n%s", got, body)
	}
}

func TestGenerateRefusesSecretsAndSymlinks(t *testing.T) {
	h := host.NewMem()
	h.SetFile("/etc/app/secret.conf", "password=hunter2\n", 0o640, 0, 0)
	h.SetFile("/etc/ssl/private/site.key", "-----BEGIN PRIVATE KEY-----\n", 0o644, 0, 0)
	if err := h.Symlink("../run/resolv.conf", "/etc/resolv.conf"); err != nil {
		t.Fatal(err)
	}
	rep := &Report{Hostname: "h", Items: []Item{
		{Category: CategoryUnpackagedConfig, Claim: "path:/etc/app/secret.conf"},
		{Category: CategoryUnpackagedConfig, Claim: "path:/etc/ssl/private/site.key"},
		{Category: CategoryUnpackagedConfig, Claim: "path:/etc/resolv.conf"},
	}}
	res, err := Generate(rep, GenerateOptions{Host: h, Dir: "/srv/fleet"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Documents != 0 || len(res.FilePaths) != 0 {
		t.Errorf("documents = %d, payloads = %v; nothing here should be extracted", res.Documents, res.FilePaths)
	}
	joined := strings.Join(res.Skipped, "\n")
	for _, want := range []string{"world-readable", "secrets", "symlink"} {
		if !strings.Contains(joined, want) {
			t.Errorf("skipped = %v, want an explanation mentioning %q", res.Skipped, want)
		}
	}
	for _, p := range h.Paths() {
		if strings.HasPrefix(p, "/srv/fleet/files/") {
			t.Errorf("payload written for a refused item: %s", p)
		}
	}
}

func TestGenerateDoesNotClobberExistingFiles(t *testing.T) {
	h := host.NewMem()
	h.SetFile("/etc/a/conf", "from the host\n", 0o644, 0, 0)
	rep := &Report{Hostname: "h", Items: []Item{{Category: CategoryUnpackagedConfig, Claim: "path:/etc/a/conf"}}}

	h.SetFile("/srv/fleet/files/etc-a-conf", "edited by the operator\n", 0o644, 0, 0)
	res, err := Generate(rep, GenerateOptions{Host: h, Dir: "/srv/fleet"})
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := h.ReadFile("/srv/fleet/files/etc-a-conf"); string(data) != "edited by the operator\n" {
		t.Errorf("existing payload was overwritten: %q", data)
	}
	if len(res.FilePaths) != 1 || res.FilePaths[0] == "files/etc-a-conf" {
		t.Errorf("payloads = %v, want a fresh name", res.FilePaths)
	}

	if _, err := Generate(rep, GenerateOptions{Host: h, Dir: "/srv/fleet"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second generate: err = %v, want a refusal to replace discovered.yaml", err)
	}
}

func TestGenerateCarriesScanGapsForward(t *testing.T) {
	h := host.NewMem()
	rep := &Report{Hostname: "h", Skipped: []string{"modified-config: needs dpkg or rpm"}}
	res, err := Generate(rep, GenerateOptions{Host: h, Dir: "/srv/fleet"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(res.Skipped, "\n"), "needs dpkg or rpm") {
		t.Errorf("skipped = %v, want the scan's own gaps reported", res.Skipped)
	}
}

func TestGeneratedNamesKeepWholeWords(t *testing.T) {
	raw := "etc/systemd/system/billing-platform-reconciliation-worker.service.d/override.conf"
	full := unsafeName.ReplaceAllString(raw, "-")
	name := uniqueName(raw, map[string]bool{})
	if len(name) > 60 || !strings.HasSuffix("-"+full, "-"+name) {
		t.Errorf("name = %q, want at most 60 characters ending %q on a word boundary", name, full)
	}
}
