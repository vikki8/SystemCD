package manifest

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestParseRejectsMisspelledDocumentFields(t *testing.T) {
	// A misspelled `targets` used to be dropped silently, which applied a
	// document meant for one host to the whole fleet.
	cases := []struct{ name, src, want string }{
		{
			"metadata.target",
			"apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: x\n  target:\n    hosts: [web01]\nspec:\n  path: /etc/x\n",
			`unknown field "target" in metadata`,
		},
		{
			"metadata.targets.host",
			"apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: x\n  targets:\n    host: [web01]\n",
			`unknown field "host" in metadata.targets`,
		},
		{
			"top-level dependson",
			"apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: x\ndependson: [Package/a]\n",
			`unknown field "dependson" in the document`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src), "t.yaml")
			if err == nil {
				t.Fatal("want an error for the misspelled field, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}

	// YAML that is not a systemcd document is still skipped, not rejected.
	docs, err := Parse([]byte("services:\n  web:\n    image: nginx\n"), "compose.yaml")
	if err != nil || len(docs) != 0 {
		t.Errorf("foreign YAML: docs=%v err=%v, want it skipped", docs, err)
	}
}

func TestDecodeSpecResolvesAnchorsDefinedOutsideSpec(t *testing.T) {
	docs, err := Parse([]byte(`
apiVersion: systemcd.dev/v1
kind: Package
metadata:
  name: &pkg nginx
spec:
  name: *pkg
`), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Name string `yaml:"name"`
	}
	if err := docs[0].DecodeSpec(&spec); err != nil {
		t.Fatalf("DecodeSpec: %v", err)
	}
	if spec.Name != "nginx" {
		t.Errorf("name = %q, want the anchored value", spec.Name)
	}
}

func TestPatchScopedWiderThanItsTargetIsANoOpElsewhere(t *testing.T) {
	// The resource exists only on web hosts; the patch applies to all of prod.
	// On a prod database host there is nothing to narrow, and the run must
	// not fail.
	docs, err := Parse([]byte(`
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: nginx-conf
  targets:
    labels: {role: web}
spec:
  path: /etc/nginx/nginx.conf
  mode: "0644"
---
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: motd
spec:
  path: /etc/motd
---
apiVersion: systemcd.dev/v1
kind: Patch
metadata:
  name: prod-hardening
  targets:
    labels: {env: prod}
spec:
  target: File/nginx-conf
  patch:
    mode: "0600"
`), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	repo := &Repository{Documents: docs}
	if err := repo.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	db, err := repo.SelectForE(Node{Hostname: "db01", Labels: map[string]string{"env": "prod", "role": "db"}})
	if err != nil {
		t.Fatalf("db01: %v", err)
	}
	if got := refs(db); len(got) != 1 || got[0] != "File/motd" {
		t.Errorf("db01 selected %v, want only File/motd", got)
	}

	web, err := repo.SelectForE(Node{Hostname: "web01", Labels: map[string]string{"env": "prod", "role": "web"}})
	if err != nil {
		t.Fatalf("web01: %v", err)
	}
	if got := specValue(t, web[0], "mode"); got != "0600" {
		t.Errorf("mode on web01 = %q, want the patch applied", got)
	}
}

func TestEmptyPatchDoesNotResetTheTargetSpec(t *testing.T) {
	// A `patch:` whose fields were all commented out is null. Treating it as
	// "replace the spec with null" reset a stopped service to the default of
	// started.
	docs, err := Parse([]byte(`
apiVersion: systemcd.dev/v1
kind: Service
metadata:
  name: telnet
spec:
  enabled: false
  state: stopped
---
apiVersion: systemcd.dev/v1
kind: Patch
metadata:
  name: p
spec:
  target: Service/telnet
  patch:
    # state: started
  notify: []
`), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	repo := &Repository{Documents: docs}
	out, err := repo.SelectForE(Node{Hostname: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if got := specValue(t, out[0], "state"); got != "stopped" {
		t.Errorf("state = %q, want the base value kept", got)
	}
	if got := specValue(t, out[0], "enabled"); got != "false" {
		t.Errorf("enabled = %q, want the base value kept", got)
	}
}

func TestNonMappingPatchIsRejected(t *testing.T) {
	docs, err := Parse([]byte(`
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: conf
spec:
  path: /etc/conf
---
apiVersion: systemcd.dev/v1
kind: Patch
metadata:
  name: p
spec:
  target: File/conf
  patch: [mode, "0600"]
`), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	repo := &Repository{Documents: docs}
	if _, err := repo.SelectForE(Node{Hostname: "h"}); err == nil || !strings.Contains(err.Error(), "must be a mapping") {
		t.Errorf("SelectForE error = %v, want a mapping error", err)
	}
	if err := repo.Validate(); err == nil || !strings.Contains(err.Error(), "must be a mapping") {
		t.Errorf("Validate error = %v, want a mapping error", err)
	}
}

func TestPatchMergesThroughAliases(t *testing.T) {
	docs, err := Parse([]byte(`
apiVersion: systemcd.dev/v1
kind: Sysctl
metadata:
  name: net
  labels: &defaults
    net.core.somaxconn: "1024"
    net.ipv4.tcp_syncookies: "1"
spec:
  values: *defaults
---
apiVersion: systemcd.dev/v1
kind: Patch
metadata:
  name: p
spec:
  target: Sysctl/net
  patch:
    values:
      net.core.somaxconn: "4096"
`), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	repo := &Repository{Documents: docs}
	out, err := repo.SelectForE(Node{Hostname: "h"})
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Values map[string]string `yaml:"values"`
	}
	if err := out[0].DecodeSpec(&spec); err != nil {
		t.Fatal(err)
	}
	if spec.Values["net.core.somaxconn"] != "4096" || spec.Values["net.ipv4.tcp_syncookies"] != "1" {
		t.Errorf("values = %v, want the aliased map merged key by key", spec.Values)
	}
}

func TestSelectForKeepsDocumentsWhenAPatchFails(t *testing.T) {
	// SelectFor promises the unpatched set on a patch error. Returning nothing
	// told `release` that no resource was still declared, so releasing one the
	// repository still wanted went through without --force.
	docs, err := Parse([]byte(`
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: conf
spec:
  path: /etc/conf
---
apiVersion: systemcd.dev/v1
kind: Patch
metadata:
  name: broken
spec:
  target: File/ghost
`), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	repo := &Repository{Documents: docs}
	got := refs(repo.SelectFor(Node{Hostname: "h"}))
	found := false
	for _, ref := range got {
		found = found || ref == "File/conf"
	}
	if !found {
		t.Errorf("SelectFor = %v, want File/conf still reported", got)
	}
}

func TestValidateChecksPatchDocuments(t *testing.T) {
	const base = "apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: conf\nspec:\n  path: /etc/conf\n---\n"
	cases := []struct{ name, src, want string }{
		{
			"dangling spec.notify",
			base + "apiVersion: systemcd.dev/v1\nkind: Patch\nmetadata:\n  name: p\nspec:\n  target: File/conf\n  notify: [Service/ngnix]\n",
			`spec.notify references unknown resource "Service/ngnix"`,
		},
		{
			"dangling spec.dependsOn",
			base + "apiVersion: systemcd.dev/v1\nkind: Patch\nmetadata:\n  name: p\nspec:\n  target: File/conf\n  dependsOn: [Package/ghost]\n",
			`spec.dependsOn references unknown resource "Package/ghost"`,
		},
		{
			"edges on the patch document itself",
			base + "apiVersion: systemcd.dev/v1\nkind: Patch\nmetadata:\n  name: p\nspec:\n  target: File/conf\nnotify: [File/conf]\n",
			"put them under spec",
		},
		{
			"unknown target",
			base + "apiVersion: systemcd.dev/v1\nkind: Patch\nmetadata:\n  name: p\nspec:\n  target: File/ghost\n",
			"no document in this repository declares",
		},
		{
			"patch apiVersion",
			base + "apiVersion: systemcd.dev/v2\nkind: Patch\nmetadata:\n  name: p\nspec:\n  target: File/conf\n",
			"is not supported",
		},
		{
			"patch name",
			base + "apiVersion: systemcd.dev/v1\nkind: Patch\nmetadata: {}\nspec:\n  target: File/conf\n",
			"metadata.name is required",
		},
		{
			"duplicate patch",
			base + "apiVersion: systemcd.dev/v1\nkind: Patch\nmetadata:\n  name: p\nspec:\n  target: File/conf\n---\napiVersion: systemcd.dev/v1\nkind: Patch\nmetadata:\n  name: p\nspec:\n  target: File/conf\n",
			"duplicate resource Patch/p",
		},
		{
			"misspelled patch field",
			base + "apiVersion: systemcd.dev/v1\nkind: Patch\nmetadata:\n  name: p\nspec:\n  target: File/conf\n  notfiy: [File/conf]\n",
			"notfiy",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			docs, err := Parse([]byte(tc.src), "t.yaml")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			err = (&Repository{Documents: docs}).Validate()
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateRejectsNamesThatAreNotOnePathComponent(t *testing.T) {
	// A name becomes part of file names (a Sysctl's drop-in is
	// 60-systemcd-<name>.conf), so it must not be able to walk out of the
	// directory it belongs in.
	for _, name := range []string{"../../../tmp/evil", "a/b", "a..b", "has space", "tab\there", "nl\nhere"} {
		src := "apiVersion: systemcd.dev/v1\nkind: Sysctl\nmetadata:\n  name: " + strconv.Quote(name) + "\nspec:\n  key: vm.swappiness\n  value: \"10\"\n"
		docs, err := Parse([]byte(src), "t.yaml")
		if err != nil {
			t.Fatalf("%q: Parse: %v", name, err)
		}
		if err := (&Repository{Documents: docs}).Validate(); err == nil || !strings.Contains(err.Error(), "metadata.name") {
			t.Errorf("%q: Validate error = %v, want the name rejected", name, err)
		}
	}
	docs, err := Parse([]byte("apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: nginx.conf_v2-web01\nspec:\n  path: /etc/x\n"), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Repository{Documents: docs}).Validate(); err != nil {
		t.Errorf("an ordinary name was rejected: %v", err)
	}
}

func TestValidateRejectsMalformedHostGlobs(t *testing.T) {
	// A malformed pattern matches no hostname, which silently removes the
	// document from every host.
	docs, err := Parse([]byte("apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: a\n  targets:\n    hosts: [\"web-[0-9\"]\nspec:\n  path: /etc/a\n"), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	err = (&Repository{Documents: docs}).Validate()
	if err == nil || !strings.Contains(err.Error(), "not a valid glob") {
		t.Errorf("error = %v, want the bad glob reported", err)
	}
}

const pkgDoc = "apiVersion: systemcd.dev/v1\nkind: Package\nmetadata:\n  name: %s\n"

func pkg(name string) string { return strings.Replace(pkgDoc, "%s", name, 1) }

func TestLoadRefusesPathsOutsideTheRepository(t *testing.T) {
	outside := t.TempDir()
	write(t, outside, "evil.yaml", pkg("evil"))

	t.Run("dot-dot", func(t *testing.T) {
		parent := t.TempDir()
		write(t, parent, "shared/x.yaml", pkg("x"))
		dir := filepath.Join(parent, "repo")
		write(t, dir, "systemcd.yaml", "apiVersion: systemcd.dev/v1\nkind: Config\nmetadata:\n  name: c\nspec:\n  paths: [../shared]\n")
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "outside the repository") {
			t.Errorf("Load error = %v, want the escaping path refused", err)
		}
	})

	t.Run("symlinked directory in paths", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "systemcd.yaml", "apiVersion: systemcd.dev/v1\nkind: Config\nmetadata:\n  name: c\nspec:\n  paths: [ext]\n")
		if err := os.Symlink(outside, filepath.Join(dir, "ext")); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "outside the repository") {
			t.Errorf("Load error = %v, want the escaping symlink refused", err)
		}
	})

	t.Run("symlinked manifest", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "manifests/ok.yaml", pkg("ok"))
		if err := os.Symlink(filepath.Join(outside, "evil.yaml"), filepath.Join(dir, "manifests", "evil.yaml")); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "outside the repository") {
			t.Errorf("Load error = %v, want the escaping symlink refused", err)
		}
	})
}

func TestLoadFollowsASymlinkedPathInsideTheRepository(t *testing.T) {
	// WalkDir does not descend into a root that is a symlink, so a listed
	// path that was a link to a directory used to load nothing at all.
	dir := t.TempDir()
	write(t, dir, "systemcd.yaml", "apiVersion: systemcd.dev/v1\nkind: Config\nmetadata:\n  name: c\nspec:\n  paths: [current]\n")
	write(t, dir, "releases/v2/a.yaml", pkg("a"))
	if err := os.Symlink("releases/v2", filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}
	repo, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := refs(repo.Documents); len(got) != 1 || got[0] != "Package/a" {
		t.Errorf("documents = %v, want Package/a", got)
	}
}

func TestLoadSkipsHiddenEntries(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "manifests/a.yaml", pkg("a"))
	// Tooling directories hold YAML that is not ours.
	write(t, dir, ".circleci/config.yml", "apiVersion: systemcd.dev/v1\nkind: Config\nmetadata:\n  name: ci\n")
	// An editor lock file is a dangling symlink with a .yaml name.
	if err := os.Symlink("user@host.1234", filepath.Join(dir, "manifests", ".#a.yaml")); err != nil {
		t.Fatal(err)
	}
	repo, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := refs(repo.Documents); len(got) != 1 || got[0] != "Package/a" {
		t.Errorf("documents = %v, want only Package/a", got)
	}
}

func TestLoadRejectsAmbiguousConfig(t *testing.T) {
	cfg := "apiVersion: systemcd.dev/v1\nkind: Config\nmetadata:\n  name: c\nspec:\n  prune: true\n"
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"two config files", map[string]string{"systemcd.yaml": cfg, ".systemcd.yaml": cfg}, "keep one"},
		{"two Config documents", map[string]string{"systemcd.yaml": cfg + "---\n" + strings.Replace(cfg, "true", "false", 1)}, "second Config document"},
		{"wrong apiVersion", map[string]string{"systemcd.yaml": strings.Replace(cfg, "v1", "v9", 1)}, "is not supported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tc.files {
				write(t, dir, name, content)
			}
			if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
