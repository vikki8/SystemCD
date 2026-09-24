package resource

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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

func TestFileRefusesAFIFOInsteadOfBlocking(t *testing.T) {
	// Reading a FIFO blocks until a writer appears, so a FIFO at a managed
	// path used to hang plan forever. It must be refused, promptly.
	h, root := rootedHost(t)
	if err := syscall.Mkfifo(filepath.Join(root, "etc", "app.conf"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	c := newContext(h)
	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: app
spec:
  path: /etc/app.conf
  content: "x\n"
`)

	done := make(chan error, 1)
	go func() {
		_, err := r.Observe(c)
		if err == nil {
			_, err = r.(Baseliner).CaptureBaseline(c)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("err = %v, want a refusal naming a non-regular file", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("observing a FIFO blocked")
	}

	pruned := make(chan error, 1)
	go func() { pruned <- r.(Deletable).Delete(c) }()
	select {
	case err := <-pruned:
		if err == nil {
			t.Error("prune removed a FIFO the File never managed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pruning past a FIFO blocked in the backup")
	}
}
