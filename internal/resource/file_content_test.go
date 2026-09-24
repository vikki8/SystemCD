package resource

import (
	"testing"

	"github.com/vikki8/systemcd/internal/host"
)

func TestFileWithoutContentOrSourceLeavesContentsAlone(t *testing.T) {
	// A File that names neither content nor source manages mode and
	// ownership. It must not truncate a file that already has contents.
	h := host.NewMem()
	h.SetFile("/etc/keep.conf", "keep me\n", 0o600, 0, 0)
	c := newContext(h)

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: keep
spec:
  path: /etc/keep.conf
  mode: "0644"
`)
	converge(t, r, c)
	data, err := h.ReadFile("/etc/keep.conf")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep me\n" {
		t.Errorf("contents = %q, want them left alone", data)
	}
	info, _ := h.Stat("/etc/keep.conf")
	if info.Mode.Perm() != 0o644 {
		t.Errorf("mode = %o, want 0644", info.Mode.Perm())
	}
	assertConverged(t, r, c)
}

func TestFileWithoutContentCreatesAnEmptyFile(t *testing.T) {
	h := host.NewMem()
	c := newContext(h)

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: marker
spec:
  path: /etc/marker
`)
	converge(t, r, c)
	data, err := h.ReadFile("/etc/marker")
	if err != nil {
		t.Fatalf("file was not created: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("contents = %q, want an empty file", data)
	}
	assertConverged(t, r, c)
}

func TestFileExplicitEmptyContentIsManaged(t *testing.T) {
	// `content: ""` is a statement about the contents, unlike an omitted
	// field, so the file is emptied.
	h := host.NewMem()
	h.SetFile("/etc/empty.conf", "stale\n", 0o644, 0, 0)
	c := newContext(h)

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: empty
spec:
  path: /etc/empty.conf
  content: ""
`)
	converge(t, r, c)
	data, _ := h.ReadFile("/etc/empty.conf")
	if len(data) != 0 {
		t.Errorf("contents = %q, want the file emptied", data)
	}
	assertConverged(t, r, c)
}
