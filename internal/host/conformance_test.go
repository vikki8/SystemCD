package host

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"testing"
)

// Every resource test runs against MemHost, so a behaviour MemHost gets
// differently from the real filesystem is a bug the tests cannot see. These
// cases run the same script against both implementations.

type hostFactory struct {
	name string
	new  func(t *testing.T) Host
}

func hosts() []hostFactory {
	return []hostFactory{
		{"OSHost", func(t *testing.T) Host { return NewRootedOS(t.TempDir()) }},
		{"MemHost", func(t *testing.T) Host { return NewMem() }},
	}
}

func eachHost(t *testing.T, fn func(t *testing.T, h Host)) {
	t.Helper()
	for _, f := range hosts() {
		t.Run(f.name, func(t *testing.T) { fn(t, f.new(t)) })
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestChmodRefusesToFollowASymlink(t *testing.T) {
	// A symlink planted at a managed path must not redirect the chmod to
	// whatever it points at.
	eachHost(t, func(t *testing.T, h Host) {
		must(t, h.WriteFile("/secret", []byte("x"), 0o600))
		must(t, h.Symlink("secret", "/link"))

		err := h.Chmod("/link", 0o644)
		if !errors.Is(err, ErrSymlink) {
			t.Fatalf("Chmod through a symlink: err = %v, want ErrSymlink", err)
		}
		info, err := h.Stat("/secret")
		must(t, err)
		if info.Mode != 0o600 {
			t.Errorf("the link target's mode changed to %o", info.Mode)
		}
	})
}

func TestChmodOnARegularFile(t *testing.T) {
	eachHost(t, func(t *testing.T, h Host) {
		must(t, h.WriteFile("/f", []byte("x"), 0o600))
		must(t, h.Chmod("/f", 0o640))
		info, err := h.Stat("/f")
		must(t, err)
		if info.Mode != 0o640 {
			t.Errorf("mode = %o, want 640", info.Mode)
		}
		must(t, h.MkdirAll("/d", 0o755))
		must(t, h.Chmod("/d", 0o700))
		if info, _ := h.Stat("/d"); info.Mode != 0o700 {
			t.Errorf("directory mode = %o, want 700", info.Mode)
		}
		if err := h.Chmod("/missing", 0o644); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Chmod of a missing path: err = %v, want ErrNotExist", err)
		}
	})
}

func TestWriteFileReplacesASymlinkInsteadOfFollowingIt(t *testing.T) {
	eachHost(t, func(t *testing.T, h Host) {
		must(t, h.WriteFile("/target", []byte("original"), 0o600))
		must(t, h.Symlink("target", "/link"))

		must(t, h.WriteFile("/link", []byte("new"), 0o644))

		got, err := h.ReadFile("/target")
		must(t, err)
		if string(got) != "original" {
			t.Errorf("the link target was written through: %q", got)
		}
		info, err := h.Stat("/link")
		must(t, err)
		if info.Target != "" {
			t.Errorf("the path is still a symlink to %q", info.Target)
		}
		if got, _ := h.ReadFile("/link"); string(got) != "new" {
			t.Errorf("content = %q", got)
		}
	})
}

func TestWriteFileOverADirectoryFails(t *testing.T) {
	eachHost(t, func(t *testing.T, h Host) {
		must(t, h.WriteFile("/d/child", []byte("x"), 0o644))
		if err := h.WriteFile("/d", []byte("x"), 0o644); err == nil {
			t.Fatal("writing a file over a directory succeeded")
		}
		if got, err := h.ReadFile("/d/child"); err != nil || string(got) != "x" {
			t.Errorf("the directory's contents were lost: %q, %v", got, err)
		}
	})
}

func TestMkdirAllDoesNotReplaceAFile(t *testing.T) {
	eachHost(t, func(t *testing.T, h Host) {
		must(t, h.WriteFile("/etc/thing", []byte("keep me"), 0o644))
		if err := h.MkdirAll("/etc/thing/sub", 0o755); err == nil {
			t.Error("MkdirAll through a regular file succeeded")
		}
		if err := h.WriteFile("/etc/thing/child", []byte("x"), 0o644); err == nil {
			t.Error("WriteFile beneath a regular file succeeded")
		}
		if got, err := h.ReadFile("/etc/thing"); err != nil || string(got) != "keep me" {
			t.Errorf("the file was replaced: %q, %v", got, err)
		}
	})
}

func TestRemoveRefusesANonEmptyDirectory(t *testing.T) {
	eachHost(t, func(t *testing.T, h Host) {
		must(t, h.WriteFile("/d/child", []byte("x"), 0o644))
		err := h.Remove("/d")
		if err == nil {
			t.Fatal("Remove of a non-empty directory succeeded")
		}
		if errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v, which callers would read as 'already gone'", err)
		}
		if _, err := h.Stat("/d/child"); err != nil {
			t.Errorf("the directory's contents were lost: %v", err)
		}
		must(t, h.Remove("/d/child"))
		must(t, h.Remove("/d"))
	})
}

func TestChownMinusOneLeavesThatIdAlone(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown to another uid needs root")
	}
	eachHost(t, func(t *testing.T, h Host) {
		must(t, h.WriteFile("/f", []byte("x"), 0o644))
		must(t, h.Chown("/f", 1234, 5678))
		must(t, h.Chown("/f", -1, 42))
		info, err := h.Stat("/f")
		must(t, err)
		if info.UID != 1234 || info.GID != 42 {
			t.Errorf("uid:gid = %d:%d, want 1234:42", info.UID, info.GID)
		}
	})
}

func TestWriteFileKeepsTheOwnerOfAnExistingFile(t *testing.T) {
	// Rewriting a config is not an ownership change. A 0600 file owned by
	// the daemon that reads it must stay readable to that daemon.
	if os.Geteuid() != 0 {
		t.Skip("chown to another uid needs root")
	}
	eachHost(t, func(t *testing.T, h Host) {
		must(t, h.WriteFile("/etc/app.conf", []byte("old"), 0o600))
		must(t, h.Chown("/etc/app.conf", 1234, 5678))

		must(t, h.WriteFile("/etc/app.conf", []byte("new"), 0o600))

		info, err := h.Stat("/etc/app.conf")
		must(t, err)
		if info.UID != 1234 || info.GID != 5678 {
			t.Errorf("owner = %d:%d after a content rewrite, want 1234:5678", info.UID, info.GID)
		}
		if info.Mode != 0o600 {
			t.Errorf("mode = %o", info.Mode)
		}
	})
}

func TestReadFileFollowsSymlinks(t *testing.T) {
	eachHost(t, func(t *testing.T, h Host) {
		must(t, h.WriteFile("/etc/real.conf", []byte("contents"), 0o644))
		must(t, h.Symlink("real.conf", "/etc/alias.conf"))
		got, err := h.ReadFile("/etc/alias.conf")
		must(t, err)
		if string(got) != "contents" {
			t.Errorf("ReadFile through a symlink = %q, want the target's contents", got)
		}

		must(t, h.Symlink("nowhere", "/etc/dangling"))
		if _, err := h.ReadFile("/etc/dangling"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("dangling symlink: err = %v, want ErrNotExist", err)
		}

		info, err := h.Stat("/etc/alias.conf")
		must(t, err)
		if info.Target != "real.conf" || info.Size != int64(len("real.conf")) {
			t.Errorf("Stat of a symlink = %+v, want the link itself", info)
		}
	})
}

func TestReadingADirectoryIsAnErrorNotAbsence(t *testing.T) {
	// Callers treat ErrNotExist as "nothing there". A directory is there.
	eachHost(t, func(t *testing.T, h Host) {
		must(t, h.MkdirAll("/d", 0o755))
		_, err := h.ReadFile("/d")
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Errorf("ReadFile of a directory: err = %v, want a non-ErrNotExist error", err)
		}
		must(t, h.WriteFile("/f", []byte("x"), 0o644))
		_, err = h.ReadDir("/f")
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Errorf("ReadDir of a file: err = %v, want a non-ErrNotExist error", err)
		}
		if !errors.Is(err, syscall.ENOTDIR) {
			t.Errorf("ReadDir of a file: err = %v, want ENOTDIR", err)
		}
	})
}
