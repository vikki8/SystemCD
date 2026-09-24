package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// OSHost is the real implementation, rooted at Root ("/" in production).
// A non-"/" root is useful for integration tests that want real filesystem
// semantics inside a scratch directory.
type OSHost struct {
	Root string
	// CommandTimeout bounds any single Run when the caller's context has no
	// deadline of its own. Zero means no additional bound.
	CommandTimeout time.Duration
	// Env, when non-nil, replaces the inherited environment for commands.
	Env []string
}

// NewOS returns a Host backed by the running machine.
func NewOS() *OSHost {
	return &OSHost{Root: "/", CommandTimeout: 10 * time.Minute}
}

// NewRootedOS returns a Host whose filesystem operations are confined to root.
func NewRootedOS(root string) *OSHost {
	return &OSHost{Root: root, CommandTimeout: 10 * time.Minute}
}

var _ Host = (*OSHost)(nil)

// resolve maps a manifest-visible absolute path onto the real filesystem.
func (h *OSHost) resolve(path string) string {
	if h.Root == "" || h.Root == "/" {
		return path
	}
	return filepath.Join(h.Root, path)
}

// waitDelay bounds two waits in Run: how long a command gets to exit after
// SIGTERM before it is killed, and how long Run keeps reading output after the
// command has exited but a child it left behind still holds stdout open.
const waitDelay = 5 * time.Second

func (h *OSHost) Run(ctx context.Context, name string, args ...string) (Result, error) {
	if _, ok := ctx.Deadline(); !ok && h.CommandTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.CommandTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if h.Env != nil {
		cmd.Env = h.Env
	}
	// Ask before killing: git removes its index.lock on SIGTERM but cannot on
	// SIGKILL, and a stale lock wedges every later sync of the checkout.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	// Without a bound, a command that backgrounds a daemon which inherits
	// stdout (an Exec starting a service by hand) blocks Run until that
	// daemon exits, which can be never.
	cmd.WaitDelay = waitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	res := Result{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start),
	}
	if err == nil {
		return res, nil
	}
	if ctx.Err() != nil {
		// A command stopped by a timeout or a shutdown did not fail on its
		// own terms. Reporting its signal exit as an ordinary non-zero status
		// would make a guard like `unless` read "condition false" and act.
		res.ExitCode = -1
		if cmd.ProcessState != nil {
			res.ExitCode = cmd.ProcessState.ExitCode()
		}
		return res, fmt.Errorf("%s: %w", name, ctx.Err())
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		// The command itself exited successfully; only a child it started
		// kept the output pipes open.
		return res, nil
	}
	var ee *exec.ExitError
	if ok := asExitError(err, &ee); ok {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	return res, err
}

func (h *OSHost) RunShell(ctx context.Context, script string) (Result, error) {
	return h.Run(ctx, "/bin/sh", "-c", script)
}

func (h *OSHost) LookPath(name string) (string, error) { return exec.LookPath(name) }

func (h *OSHost) ReadFile(path string) ([]byte, error) { return os.ReadFile(h.resolve(path)) }

// TryLock takes a non-blocking exclusive flock. The lock file is created if
// needed and deliberately never removed: unlinking it races with another
// process that has already opened it.
func (h *OSHost) TryLock(path string) (func() error, error) {
	real := h.resolve(path)
	if err := os.MkdirAll(filepath.Dir(real), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(real, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	// Record who holds it, so an operator staring at a "locked" message can
	// find the process.
	_ = f.Truncate(0)
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		// Losing the annotation is not worth failing the run over.
		_ = err
	}
	return func() error {
		defer f.Close()
		return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}, nil
}

func (h *OSHost) WriteFile(path string, data []byte, mode fs.FileMode) error {
	return h.writeFile(path, data, mode, false)
}

// WriteFileSync additionally fsyncs the parent directory so the rename is
// durable, not just the file contents.
func (h *OSHost) WriteFileSync(path string, data []byte, mode fs.FileMode) error {
	return h.writeFile(path, data, mode, true)
}

func (h *OSHost) writeFile(path string, data []byte, mode fs.FileMode, syncDir bool) error {
	real := h.resolve(path)
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		return err
	}

	// Rewriting a file's contents is not an ownership change. The temp file
	// below is born owned by root, so without this a config that a daemon
	// reads as its own user (0600 postgres:postgres) would silently become
	// unreadable to it. A symlink at the path is replaced, not followed, so
	// only a regular file lends its owner.
	uid, gid := -1, -1
	if fi, err := os.Lstat(real); err == nil {
		if fi.IsDir() {
			return &fs.PathError{Op: "write", Path: path, Err: syscall.EISDIR}
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && fi.Mode().IsRegular() {
			uid, gid = int(st.Uid), int(st.Gid)
		}
	}

	// Write to a temp file in the same directory and rename, so readers never
	// observe a half-written config.
	tmp, err := os.CreateTemp(filepath.Dir(real), ".systemcd-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// Ownership and mode are set through the open descriptor rather than by
	// name: in a directory someone else can write to, the name could be
	// swapped for a symlink between close and chmod. Chown comes first
	// because it clears setuid bits.
	if uid >= 0 {
		if err := tmp.Chown(uid, gid); err != nil && !errors.Is(err, syscall.EPERM) {
			// EPERM only happens when not running as root, where keeping
			// someone else's ownership was never possible anyway.
			tmp.Close()
			return err
		}
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, real); err != nil {
		return err
	}
	if !syncDir {
		return nil
	}
	dir, err := os.Open(filepath.Dir(real))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (h *OSHost) Stat(path string) (FileInfo, error) {
	real := h.resolve(path)
	fi, err := os.Lstat(real)
	if err != nil {
		return FileInfo{}, err
	}
	info := FileInfo{
		Path:  path,
		Mode:  fi.Mode().Perm(),
		Size:  fi.Size(),
		IsDir: fi.IsDir(),
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		info.UID = int(st.Uid)
		info.GID = int(st.Gid)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		if target, err := os.Readlink(real); err == nil {
			info.Target = target
		}
	}
	return info, nil
}

func (h *OSHost) ReadDir(path string) ([]string, error) {
	entries, err := os.ReadDir(h.resolve(path))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

func (h *OSHost) MkdirAll(path string, mode fs.FileMode) error {
	return os.MkdirAll(h.resolve(path), mode)
}

func (h *OSHost) Remove(path string) error    { return os.Remove(h.resolve(path)) }
func (h *OSHost) RemoveAll(path string) error { return os.RemoveAll(h.resolve(path)) }

func (h *OSHost) Symlink(target, link string) error {
	real := h.resolve(link)
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		return err
	}
	_ = os.Remove(real)
	return os.Symlink(target, real)
}

// Chmod changes a path's mode without following a symlink at that path.
//
// Linux has no lchmod, and chmod(2) follows links: a symlink planted at a
// managed path by whoever can write its directory would redirect the chmod
// to, say, /etc/shadow. The path is opened with O_NOFOLLOW and changed
// through the descriptor, so the check and the change hit the same inode.
func (h *OSHost) Chmod(path string, mode fs.FileMode) error {
	real := h.resolve(path)
	f, err := os.OpenFile(real, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err == nil {
		defer f.Close()
		return f.Chmod(mode)
	}
	if errors.Is(err, syscall.ELOOP) {
		return &fs.PathError{Op: "chmod", Path: path, Err: ErrSymlink}
	}
	if errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Some paths cannot be opened for reading at all (a socket, or a file an
	// unprivileged caller may chmod but not read). Fall back to checking by
	// name.
	fi, lerr := os.Lstat(real)
	if lerr != nil {
		return lerr
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return &fs.PathError{Op: "chmod", Path: path, Err: ErrSymlink}
	}
	return os.Chmod(real, mode)
}

func (h *OSHost) Chown(path string, uid, gid int) error {
	return os.Lchown(h.resolve(path), uid, gid)
}

func (h *OSHost) LookupUID(name string) (int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		// Numeric ids are accepted verbatim so manifests can avoid a
		// name-resolution round trip on minimal images.
		if id, convErr := strconv.Atoi(name); convErr == nil {
			return id, nil
		}
		return 0, ErrUnknownUser
	}
	return strconv.Atoi(u.Uid)
}

func (h *OSHost) LookupGID(name string) (int, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		if id, convErr := strconv.Atoi(name); convErr == nil {
			return id, nil
		}
		return 0, ErrUnknownUser
	}
	return strconv.Atoi(g.Gid)
}

func (h *OSHost) LookupUserName(uid int) (string, error) {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return strconv.Itoa(uid), nil
	}
	return u.Username, nil
}

func (h *OSHost) LookupGroupName(gid int) (string, error) {
	g, err := user.LookupGroupId(strconv.Itoa(gid))
	if err != nil {
		return strconv.Itoa(gid), nil
	}
	return g.Name, nil
}

func (h *OSHost) Hostname() (string, error) { return os.Hostname() }

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}
