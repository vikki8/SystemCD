package resource

import (
	"testing"

	"github.com/vikki8/systemcd/internal/host"
)

func TestFileRestoreLeavesUnmanagedOwnershipAlone(t *testing.T) {
	// The manifest manages the contents and the group, not the owner. After
	// adoption an operator hands the file to another account; restoring the
	// original contents must not undo that, while the managed group goes back.
	h := renameHost{host.NewMem()}
	h.Users["app"] = 1000
	h.Users["ops"] = 1001
	h.Groups["app"] = 1000
	h.Groups["adm"] = 4
	h.SetFile("/etc/app.conf", "original\n", 0o640, 1000, 1000)
	c := newContext(h)

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: app
spec:
  path: /etc/app.conf
  content: "managed\n"
  mode: "0640"
  group: adm
`)
	prior, err := r.Observe(c)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := r.(Baseliner).CaptureBaseline(c)
	if err != nil {
		t.Fatal(err)
	}
	converge(t, r, c)
	if err := h.Chown("/etc/app.conf", 1001, -1); err != nil {
		t.Fatal(err)
	}

	if err := r.(Restorer).Restore(c, prior, blob); err != nil {
		t.Fatalf("restore: %v", err)
	}
	data, _ := h.ReadFile("/etc/app.conf")
	if string(data) != "original\n" {
		t.Errorf("contents = %q, want the original back", data)
	}
	info, _ := h.Stat("/etc/app.conf")
	if info.UID != 1001 {
		t.Errorf("owner = %d, want 1001: the operator's own chown of a field systemcd never managed was undone", info.UID)
	}
	if info.GID != 1000 {
		t.Errorf("group = %d, want the managed group restored to 1000", info.GID)
	}
}

func TestFileRestoreOfAMissingFileUsesTheRecordedOwnership(t *testing.T) {
	// With nothing on disk to carry ownership over from, the recorded values
	// are the only description of what was there.
	h := renameHost{host.NewMem()}
	h.Users["app"] = 1000
	h.Groups["app"] = 1000
	h.SetFile("/etc/app.conf", "original\n", 0o640, 1000, 1000)
	c := newContext(h)

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: app
spec:
  path: /etc/app.conf
  content: "managed\n"
  mode: "0640"
`)
	prior, err := r.Observe(c)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := r.(Baseliner).CaptureBaseline(c)
	if err != nil {
		t.Fatal(err)
	}
	converge(t, r, c)
	if err := h.Remove("/etc/app.conf"); err != nil {
		t.Fatal(err)
	}

	if err := r.(Restorer).Restore(c, prior, blob); err != nil {
		t.Fatalf("restore: %v", err)
	}
	info, err := h.Stat("/etc/app.conf")
	if err != nil {
		t.Fatal(err)
	}
	if info.UID != 1000 || info.GID != 1000 {
		t.Errorf("ownership = %d:%d, want the recorded 1000:1000", info.UID, info.GID)
	}
}
