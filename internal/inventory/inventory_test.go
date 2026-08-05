package inventory

import (
	"context"
	"strings"
	"testing"

	"github.com/vikki8/systemcd/internal/host"
	"github.com/vikki8/systemcd/internal/state"
)

// debianHost models an inherited Debian box: a couple of hand-edited packaged
// configs, some locally-created files, a first-party unit, and a service
// account nobody documented.
func debianHost(t *testing.T) *host.MemHost {
	t.Helper()
	h := host.NewMem()
	h.Binaries["dpkg"] = true
	h.Binaries["dpkg-query"] = true
	h.Binaries["apt-mark"] = true

	h.AddStub(host.CommandStub{Match: "dpkg -V", Stdout: strings.Join([]string{
		"??5??????   c /etc/nginx/nginx.conf",
		"??5??????   c /etc/ssh/sshd_config",
		"missing     c /etc/skel/.bashrc", // not a checksum failure
		"..?.....    c /etc/hosts",        // mode-only, deliberately ignored
	}, "\n")})
	h.AddStub(host.CommandStub{Match: "dpkg-query -S /etc/nginx/nginx.conf", Stdout: "nginx-common: /etc/nginx/nginx.conf"})
	h.AddStub(host.CommandStub{Match: "dpkg-query -S /etc/ssh/sshd_config", Stdout: "openssh-server: /etc/ssh/sshd_config"})
	h.AddStub(host.CommandStub{Match: "dpkg-query -S /etc/local-thing.conf", ExitCode: 1})
	h.AddStub(host.CommandStub{Match: "dpkg-query -S", Stdout: "somepkg: file"})

	h.AddStub(host.CommandStub{Match: "apt-mark showmanual", Stdout: "nginx\npostgresql\n"})
	h.AddStub(host.CommandStub{Match: "list-unit-files", Stdout: "nginx.service enabled enabled\nbilling.service enabled enabled\n"})
	h.AddStub(host.CommandStub{Match: "getent passwd", Stdout: strings.Join([]string{
		"root:x:0:0:root:/root:/bin/bash",
		"daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin",
		"deploy:x:1001:1001::/home/deploy:/bin/bash",
		"nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin",
	}, "\n")})

	h.SetFile("/etc/nginx/nginx.conf", "worker_processes 4;\n", 0o644, 0, 0)
	h.SetFile("/etc/local-thing.conf", "written by a human in 2020\n", 0o644, 0, 0)
	h.SetFile("/etc/systemd/system/billing.service", "[Service]\nExecStart=/usr/local/bin/billing\n", 0o644, 0, 0)
	h.SetFile("/etc/sysctl.d/99-tuning.conf", "net.core.somaxconn = 1024\n", 0o644, 0, 0)
	// Deliberately noisy paths that must not be reported.
	h.SetFile("/etc/shadow", "root:!:19000::::::\n", 0o640, 0, 0)
	h.SetFile("/etc/ssl/certs/ca.pem", "-----BEGIN CERTIFICATE-----\n", 0o644, 0, 0)
	h.SetFile("/etc/nginx/nginx.conf.dpkg-old", "old\n", 0o644, 0, 0)
	return h
}

func scan(t *testing.T, h host.Host, snap *state.Snapshot) *Report {
	t.Helper()
	rep, err := Scan(context.Background(), Options{Host: h, Snapshot: snap})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return rep
}

func claims(rep *Report, category Category) []string {
	var out []string
	for _, item := range rep.Items {
		if item.Category == category {
			out = append(out, item.Claim)
		}
	}
	return out
}

func has(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestScanFindsHandEditedPackagedConfigs(t *testing.T) {
	// This is the headline finding on an inherited machine: the package
	// manager already knows which of its files somebody changed.
	rep := scan(t, debianHost(t), nil)
	got := claims(rep, CategoryModifiedConfig)

	for _, want := range []string{"path:/etc/nginx/nginx.conf", "path:/etc/ssh/sshd_config"} {
		if !has(got, want) {
			t.Errorf("modified configs = %v, want it to include %q", got, want)
		}
	}
	// A missing file and a mode-only difference are not "somebody edited the
	// contents", and reporting them would bury the signal.
	if has(got, "path:/etc/skel/.bashrc") || has(got, "path:/etc/hosts") {
		t.Errorf("modified configs = %v, want only checksum mismatches", got)
	}
}

func TestScanAttributesTheOwningPackage(t *testing.T) {
	rep := scan(t, debianHost(t), nil)
	for _, item := range rep.Items {
		if item.Claim == "path:/etc/nginx/nginx.conf" && item.Category == CategoryModifiedConfig {
			if item.Package != "nginx-common" {
				t.Errorf("package = %q, want nginx-common", item.Package)
			}
			return
		}
	}
	t.Fatal("nginx.conf was not reported")
}

func TestScanFindsLocallyCreatedConfiguration(t *testing.T) {
	rep := scan(t, debianHost(t), nil)
	if got := claims(rep, CategoryUnpackagedConfig); !has(got, "path:/etc/local-thing.conf") {
		t.Errorf("unpackaged configs = %v, want the locally-created file", got)
	}
}

func TestScanSkipsSecretsAndNoise(t *testing.T) {
	// An inventory that invites you to commit /etc/shadow to git is worse
	// than no inventory.
	rep := scan(t, debianHost(t), nil)
	for _, item := range rep.Items {
		for _, forbidden := range []string{"/etc/shadow", "/etc/ssl/certs/", ".dpkg-old"} {
			if strings.Contains(item.Claim, forbidden) {
				t.Errorf("scan reported %q, which must never be offered for a repository", item.Claim)
			}
		}
	}
}

func TestScanFindsUnitsPackagesAndAccounts(t *testing.T) {
	rep := scan(t, debianHost(t), nil)

	if got := claims(rep, CategoryLocalUnit); !has(got, "path:/etc/systemd/system/billing.service") {
		t.Errorf("local units = %v", got)
	}
	if got := claims(rep, CategoryEnabledService); !has(got, "unit:billing.service") {
		t.Errorf("enabled services = %v", got)
	}
	if got := claims(rep, CategoryManualPackage); !has(got, "package:postgresql") {
		t.Errorf("manual packages = %v", got)
	}
	users := claims(rep, CategoryLocalUser)
	if !has(users, "user:deploy") {
		t.Errorf("local users = %v, want the service account", users)
	}
	// System accounts and `nobody` are the distro's business, not ours.
	if has(users, "user:root") || has(users, "user:daemon") || has(users, "user:nobody") {
		t.Errorf("local users = %v, want only non-system accounts", users)
	}
	if got := claims(rep, CategorySysctlDropIn); !has(got, "path:/etc/sysctl.d/99-tuning.conf") {
		t.Errorf("sysctl drop-ins = %v", got)
	}
}

func TestScanMarksWhatSystemcdAlreadyOwns(t *testing.T) {
	// The number that matters during an onboarding is the *remaining*
	// surface, so already-managed items must be attributed, not re-listed.
	snap := &state.Snapshot{Resources: map[string]state.Record{
		"File/nginx-conf": {Kind: "File", Name: "nginx-conf", Claims: []string{"path:/etc/nginx/nginx.conf"}},
	}}
	rep := scan(t, debianHost(t), snap)

	var found bool
	for _, item := range rep.Items {
		if item.Claim == "path:/etc/nginx/nginx.conf" && item.Category == CategoryModifiedConfig {
			found = true
			if item.ManagedBy != "File/nginx-conf" {
				t.Errorf("managedBy = %q, want File/nginx-conf", item.ManagedBy)
			}
		}
	}
	if !found {
		t.Fatal("the managed item should still be listed, just attributed")
	}
	for _, item := range rep.Unmanaged() {
		if item.Claim == "path:/etc/nginx/nginx.conf" && item.Category == CategoryModifiedConfig {
			t.Error("an already-owned item must not count toward the remaining work")
		}
	}
	if c := rep.Counts(); c.Managed != 1 {
		t.Errorf("counts = %+v, want one managed item", c)
	}
}

func TestScanReportsWhatItCouldNotDo(t *testing.T) {
	// A scan that silently finds nothing because a tool is missing is worse
	// than no scan: it reads as "this machine is clean".
	h := host.NewMem() // no dpkg, no rpm, no apt-mark
	rep := scan(t, h, nil)
	if len(rep.Skipped) == 0 {
		t.Fatal("a scan with no package manager must say so")
	}
	joined := strings.Join(rep.Skipped, " ")
	if !strings.Contains(joined, "dpkg") && !strings.Contains(joined, "rpm") {
		t.Errorf("skipped = %v, want an explanation naming the missing tool", rep.Skipped)
	}
}

func TestGenerateProducesAdoptableManifests(t *testing.T) {
	h := debianHost(t)
	rep := scan(t, h, nil)

	res, err := Generate(rep, GenerateOptions{
		Host:       h,
		Dir:        "/tmp/fleet",
		Categories: []Category{CategoryModifiedConfig, CategoryUnpackagedConfig, CategoryLocalUnit},
		Targets:    map[string]string{"role": "web"},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if res.Documents == 0 {
		t.Fatal("nothing was generated")
	}

	manifest, err := h.ReadFile(res.ManifestPath)
	if err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
	body := string(manifest)

	for _, want := range []string{
		"apiVersion: systemcd.dev/v1",
		"path: /etc/nginx/nginx.conf",
		"role: web",
		"source: files/",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("generated manifest is missing %q:\n%s", want, body)
		}
	}
	// A mode must be quoted, or YAML reads 0644 as a number and the octal
	// meaning is lost.
	if !strings.Contains(body, `mode: "0644"`) {
		t.Errorf("mode must be quoted in generated output:\n%s", body)
	}
	// A unit file becomes the kind that knows to run daemon-reload.
	if !strings.Contains(body, "kind: SystemdUnit") {
		t.Errorf("a unit file should generate a SystemdUnit:\n%s", body)
	}
	// The payload must actually be extracted, not just referenced.
	if len(res.FilePaths) == 0 {
		t.Fatal("no payloads were extracted")
	}
	data, err := h.ReadFile("/tmp/fleet/" + res.FilePaths[0])
	if err != nil || len(data) == 0 {
		t.Errorf("payload %s was not written: %v", res.FilePaths[0], err)
	}
}

func TestGenerateSkipsBinaryPayloads(t *testing.T) {
	h := host.NewMem()
	h.SetFile("/etc/blob.dat", "text\x00binary", 0o644, 0, 0)
	rep := &Report{Hostname: "h", Items: []Item{
		{Category: CategoryUnpackagedConfig, Claim: "path:/etc/blob.dat"},
	}}

	res, err := Generate(rep, GenerateOptions{Host: h, Dir: "/tmp/fleet"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Documents != 0 {
		t.Error("a binary file must not be turned into a manifest")
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "text") {
		t.Errorf("skipped = %v, want an explanation", res.Skipped)
	}
}

func TestGeneratedNamesAreUniqueAndSafe(t *testing.T) {
	h := host.NewMem()
	h.SetFile("/etc/a/conf", "1\n", 0o644, 0, 0)
	h.SetFile("/etc/b/conf", "2\n", 0o644, 0, 0)
	rep := &Report{Hostname: "h", Items: []Item{
		{Category: CategoryUnpackagedConfig, Claim: "path:/etc/a/conf"},
		{Category: CategoryUnpackagedConfig, Claim: "path:/etc/b/conf"},
	}}

	res, err := Generate(rep, GenerateOptions{Host: h, Dir: "/tmp/fleet"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Documents != 2 {
		t.Fatalf("documents = %d, want 2", res.Documents)
	}
	body, _ := h.ReadFile(res.ManifestPath)
	if strings.Count(string(body), "name: etc-a-conf") != 1 {
		t.Errorf("names should be derived and unique:\n%s", body)
	}
	if !strings.Contains(string(body), "etc-b-conf") {
		t.Errorf("second resource missing:\n%s", body)
	}
}
