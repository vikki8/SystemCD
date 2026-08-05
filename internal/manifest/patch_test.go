package manifest

import (
	"fmt"
	"strings"
	"testing"
)

// specValue reads one scalar out of a document's spec after patching.
func specValue(t *testing.T, d *Document, key string) string {
	t.Helper()
	var spec map[string]any
	if err := d.DecodeSpec(&spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	if v, ok := spec[key]; ok {
		return strings.TrimSpace(toString(v))
	}
	return ""
}

func toString(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

const layered = `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: nginx-conf
spec:
  path: /etc/nginx/nginx.conf
  content: "worker_processes 4;\n"
  mode: "0644"
  owner: root
---
apiVersion: systemcd.dev/v1
kind: Patch
metadata:
  name: web-hardening
  targets:
    labels:
      role: web
spec:
  target: File/nginx-conf
  patch:
    mode: "0600"
---
apiVersion: systemcd.dev/v1
kind: Patch
metadata:
  name: web01-owner
  targets:
    hosts: ["web01"]
spec:
  target: File/nginx-conf
  patch:
    owner: www-data
`

func TestPatchesLayerByTargeting(t *testing.T) {
	docs, err := Parse([]byte(layered), "layers.yaml")
	if err != nil {
		t.Fatal(err)
	}
	repo := &Repository{Documents: docs}

	t.Run("base only", func(t *testing.T) {
		out, err := repo.SelectForE(Node{Hostname: "db01", Labels: map[string]string{"role": "db"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 {
			t.Fatalf("patches must not survive into the resource set: %v", refs(out))
		}
		if got := specValue(t, out[0], "mode"); got != "0644" {
			t.Errorf("mode = %q, want the base value", got)
		}
	})

	t.Run("role layer applies", func(t *testing.T) {
		out, err := repo.SelectForE(Node{Hostname: "web02", Labels: map[string]string{"role": "web"}})
		if err != nil {
			t.Fatal(err)
		}
		if got := specValue(t, out[0], "mode"); got != "0600" {
			t.Errorf("mode = %q, want the role patch to win", got)
		}
		if got := specValue(t, out[0], "owner"); got != "root" {
			t.Errorf("owner = %q, want the base value on a host the host-patch does not target", got)
		}
	})

	t.Run("host layer stacks on the role layer", func(t *testing.T) {
		out, err := repo.SelectForE(Node{Hostname: "web01", Labels: map[string]string{"role": "web"}})
		if err != nil {
			t.Fatal(err)
		}
		if got := specValue(t, out[0], "mode"); got != "0600" {
			t.Errorf("mode = %q, want the role patch", got)
		}
		if got := specValue(t, out[0], "owner"); got != "www-data" {
			t.Errorf("owner = %q, want the host patch", got)
		}
	})

	t.Run("patching one node does not leak into another", func(t *testing.T) {
		// The repository is loaded once and selected for many nodes; a
		// mutation that survived would silently spread web01's overrides.
		if _, err := repo.SelectForE(Node{Hostname: "web01", Labels: map[string]string{"role": "web"}}); err != nil {
			t.Fatal(err)
		}
		out, err := repo.SelectForE(Node{Hostname: "db01", Labels: map[string]string{"role": "db"}})
		if err != nil {
			t.Fatal(err)
		}
		if got := specValue(t, out[0], "mode"); got != "0644" {
			t.Errorf("mode = %q on db01 after selecting web01; patches leaked", got)
		}
		if got := specValue(t, out[0], "owner"); got != "root" {
			t.Errorf("owner = %q on db01 after selecting web01; patches leaked", got)
		}
	})
}

func TestPatchPreservesUnmentionedFields(t *testing.T) {
	docs, _ := Parse([]byte(layered), "t.yaml")
	repo := &Repository{Documents: docs}
	out, err := repo.SelectForE(Node{Hostname: "web01", Labels: map[string]string{"role": "web"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := specValue(t, out[0], "path"); got != "/etc/nginx/nginx.conf" {
		t.Errorf("path = %q; a patch must not drop fields it did not mention", got)
	}
}

func TestPatchAppendsEdgesRatherThanReplacingThem(t *testing.T) {
	docs, err := Parse([]byte(`
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: conf
spec:
  path: /etc/conf
  content: "x\n"
notify: [Service/a]
---
apiVersion: systemcd.dev/v1
kind: Patch
metadata:
  name: also-restart-b
spec:
  target: File/conf
  notify: [Service/b]
`), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	repo := &Repository{Documents: docs}
	out, err := repo.SelectForE(Node{Hostname: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out[0].Notify) != 2 {
		t.Errorf("notify = %v, want both the base and the patch edge — a narrowing patch must not drop ordering", out[0].Notify)
	}
}

func TestPatchNullRemovesAField(t *testing.T) {
	docs, _ := Parse([]byte(`
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: conf
spec:
  path: /etc/conf
  content: "x\n"
  owner: root
---
apiVersion: systemcd.dev/v1
kind: Patch
metadata:
  name: unmanage-owner
spec:
  target: File/conf
  patch:
    owner: null
`), "t.yaml")
	repo := &Repository{Documents: docs}
	out, err := repo.SelectForE(Node{Hostname: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if got := specValue(t, out[0], "owner"); got != "" {
		t.Errorf("owner = %q, want it removed by the null patch", got)
	}
}

func TestPatchWithNoTargetIsRejected(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{
			"missing target",
			"apiVersion: systemcd.dev/v1\nkind: Patch\nmetadata:\n  name: p\nspec:\n  patch:\n    mode: \"0600\"\n",
			"spec.target is required",
		},
		{
			"unknown target",
			"apiVersion: systemcd.dev/v1\nkind: Patch\nmetadata:\n  name: p\nspec:\n  target: File/ghost\n  patch:\n    mode: \"0600\"\n",
			"cannot create a resource",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			docs, err := Parse([]byte(tc.src), "t.yaml")
			if err != nil {
				t.Fatal(err)
			}
			repo := &Repository{Documents: docs}
			_, err = repo.SelectForE(Node{Hostname: "h"})
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
