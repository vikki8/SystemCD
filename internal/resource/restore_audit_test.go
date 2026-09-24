package resource

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/vikki8/systemcd/internal/host"
)

func TestFileBaselineOfASymlinkRestoresTheLink(t *testing.T) {
	// /etc/resolv.conf is a symlink on most systemd hosts. Adopting it and
	// then releasing with --restore must put the link back, not a copy of
	// whatever it pointed at written out with the link's 0777 mode.
	h, root := rootedHost(t)
	c := newContext(h)
	target := filepath.Join(root, "etc", "stub-resolv.conf")
	if err := os.WriteFile(target, []byte("nameserver 127.0.0.53\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "etc", "resolv.conf")
	if err := os.Symlink("stub-resolv.conf", link); err != nil {
		t.Fatal(err)
	}

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: resolv
spec:
  path: /etc/resolv.conf
  content: "nameserver 10.0.0.1\n"
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

	if err := r.(Restorer).Restore(c, prior, blob); err != nil {
		t.Fatalf("restore: %v", err)
	}
	li, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if li.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("restore left a regular file with mode %o; want the original symlink back", li.Mode().Perm())
	}
	if got, _ := os.Readlink(link); got != "stub-resolv.conf" {
		t.Errorf("link target = %q, want stub-resolv.conf", got)
	}
	data, _ := os.ReadFile(target)
	ti, _ := os.Stat(target)
	if string(data) != "nameserver 127.0.0.53\n" || ti.Mode().Perm() != 0o600 {
		t.Errorf("the link target was modified: %q mode %o", data, ti.Mode().Perm())
	}
}

func TestServiceRestoreLeavesUnmanagedFieldsAlone(t *testing.T) {
	// Adoption records every observed field, but release --restore must only
	// undo what systemcd managed; the rest may have been changed since by an
	// operator, and reverting that is not systemcd's call.
	prior := State{"enabled": "false", "active": "inactive"}

	h := host.NewMem()
	enabledOnly := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: Service
metadata:
  name: nginx
spec:
  enabled: true
  state: unmanaged
`)
	if err := enabledOnly.(Restorer).Restore(newContext(h), prior, nil); err != nil {
		t.Fatal(err)
	}
	if !h.Ran("systemctl disable nginx.service") {
		t.Errorf("the managed enabled state was not restored; commands = %v", h.Commands)
	}
	if h.Ran("systemctl stop") || h.Ran("systemctl start") {
		t.Errorf("restore touched the runtime state systemcd never managed; commands = %v", h.Commands)
	}

	h2 := host.NewMem()
	activeOnly := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: Service
metadata:
  name: nginx
spec:
  state: started
`)
	if err := activeOnly.(Restorer).Restore(newContext(h2), prior, nil); err != nil {
		t.Fatal(err)
	}
	if !h2.Ran("systemctl stop nginx.service") {
		t.Errorf("the managed runtime state was not restored; commands = %v", h2.Commands)
	}
	if h2.Ran("systemctl enable") || h2.Ran("systemctl disable") {
		t.Errorf("restore touched the boot state systemcd never managed; commands = %v", h2.Commands)
	}
}
