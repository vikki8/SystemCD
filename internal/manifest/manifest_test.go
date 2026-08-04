package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseMultiDocument(t *testing.T) {
	src := `
apiVersion: systemcd.dev/v1
kind: Package
metadata:
  name: nginx
spec:
  name: nginx
---
# a comment-only document is skipped
---
apiVersion: systemcd.dev/v1
kind: Service
metadata:
  name: nginx
spec:
  enabled: true
dependsOn:
  - Package/nginx
`
	docs, err := Parse([]byte(src), "test.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("got %d documents, want 2", len(docs))
	}
	if docs[0].Ref() != "Package/nginx" {
		t.Errorf("first ref = %q", docs[0].Ref())
	}
	if got := docs[1].DependsOn; len(got) != 1 || got[0] != "Package/nginx" {
		t.Errorf("dependsOn = %v", got)
	}
}

func TestDecodeSpecRejectsUnknownFields(t *testing.T) {
	docs, err := Parse([]byte(`
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: x
spec:
  path: /etc/x
  ownr: root
`), "t.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var spec struct {
		Path string `yaml:"path"`
	}
	err = docs[0].DecodeSpec(&spec)
	if err == nil {
		t.Fatal("want an error for the misspelled field, got nil")
	}
	if !strings.Contains(err.Error(), "ownr") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
}

func TestTargetsMatches(t *testing.T) {
	node := Node{Hostname: "web-01.prod", Labels: map[string]string{"role": "web", "env": "prod"}}

	cases := []struct {
		name    string
		targets *Targets
		want    bool
	}{
		{"nil targets match everything", nil, true},
		{"hostname glob", &Targets{Hosts: []string{"web-*"}}, true},
		{"hostname glob miss", &Targets{Hosts: []string{"db-*"}}, false},
		{"any of several hosts", &Targets{Hosts: []string{"db-*", "web-01.*"}}, true},
		{"label match", &Targets{Labels: map[string]string{"role": "web"}}, true},
		{"label mismatch", &Targets{Labels: map[string]string{"role": "db"}}, false},
		{"labels are ANDed", &Targets{Labels: map[string]string{"role": "web", "env": "dev"}}, false},
		{"hosts and labels are ANDed", &Targets{Hosts: []string{"web-*"}, Labels: map[string]string{"env": "dev"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.targets.Matches(node); got != tc.want {
				t.Errorf("Matches() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateCatchesStructuralErrors(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			"wrong api version",
			"apiVersion: systemcd.dev/v2\nkind: File\nmetadata:\n  name: a\n",
			"is not supported",
		},
		{
			"missing name",
			"apiVersion: systemcd.dev/v1\nkind: File\nmetadata: {}\n",
			"metadata.name is required",
		},
		{
			"unknown dependency",
			"apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: a\ndependsOn: [Package/ghost]\n",
			"unknown resource",
		},
		{
			"duplicate resource",
			"apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: a\n---\napiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: a\n",
			"duplicate resource",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			docs, err := Parse([]byte(tc.src), "t.yaml")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			repo := &Repository{Documents: docs}
			err = repo.Validate()
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadReadsConfigAndSkipsAssetDirs(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "systemcd.yaml", `
apiVersion: systemcd.dev/v1
kind: Config
metadata:
  name: default
spec:
  prune: true
  interval: 30s
  labels:
    role: web
`)
	write(t, dir, "manifests/nginx.yaml", `
apiVersion: systemcd.dev/v1
kind: Package
metadata:
  name: nginx
`)
	// A payload referenced by File.source must not be parsed as a manifest,
	// even though it has a .yaml extension.
	write(t, dir, "files/app-config.yaml", "this: is: not: a: manifest\n")

	repo, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !repo.Config.Prune {
		t.Error("Config.Prune was not read")
	}
	if repo.Config.Interval != "30s" {
		t.Errorf("Config.Interval = %q", repo.Config.Interval)
	}
	if repo.Config.Labels["role"] != "web" {
		t.Errorf("Config.Labels = %v", repo.Config.Labels)
	}
	if len(repo.Documents) != 1 || repo.Documents[0].Ref() != "Package/nginx" {
		t.Fatalf("documents = %v", refs(repo.Documents))
	}
}

func TestLoadRespectsConfigPaths(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "systemcd.yaml", "apiVersion: systemcd.dev/v1\nkind: Config\nmetadata:\n  name: c\nspec:\n  paths: [roles]\n")
	write(t, dir, "roles/a.yaml", "apiVersion: systemcd.dev/v1\nkind: Package\nmetadata:\n  name: a\n")
	write(t, dir, "unused/b.yaml", "apiVersion: systemcd.dev/v1\nkind: Package\nmetadata:\n  name: b\n")

	repo, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := refs(repo.Documents); len(got) != 1 || got[0] != "Package/a" {
		t.Errorf("documents = %v, want only Package/a", got)
	}
}

func TestSelectForFiltersByNode(t *testing.T) {
	docs, err := Parse([]byte(`
apiVersion: systemcd.dev/v1
kind: Package
metadata:
  name: everywhere
---
apiVersion: systemcd.dev/v1
kind: Package
metadata:
  name: web-only
  targets:
    labels:
      role: web
`), "t.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	repo := &Repository{Documents: docs}

	web := repo.SelectFor(Node{Hostname: "h", Labels: map[string]string{"role": "web"}})
	if len(web) != 2 {
		t.Errorf("web node selected %v, want both", refs(web))
	}
	db := repo.SelectFor(Node{Hostname: "h", Labels: map[string]string{"role": "db"}})
	if len(db) != 1 || db[0].Ref() != "Package/everywhere" {
		t.Errorf("db node selected %v, want only Package/everywhere", refs(db))
	}
}

func refs(docs []*Document) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.Ref()
	}
	return out
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
