package host

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// memFile is one entry in MemHost's filesystem.
type memFile struct {
	data   []byte
	mode   fs.FileMode
	uid    int
	gid    int
	isDir  bool
	target string
}

// CommandStub scripts one command response for MemHost.
type CommandStub struct {
	// Match is a substring of the joined command line ("systemctl is-active
	// nginx"). The first stub whose Match is contained in the command line
	// wins, so register specific stubs before general ones.
	Match    string
	Stdout   string
	Stderr   string
	ExitCode int
	// Err makes the command fail to execute at all.
	Err error
	// Do runs when the stub matches, letting a test mutate state (for example
	// making `systemctl start` flip what `is-active` reports next time).
	Do func(m *MemHost, args []string)
}

// MemHost is a fully in-memory Host for unit tests: a filesystem, a user
// database, and a scripted command runner that records everything it was
// asked to execute.
type MemHost struct {
	mu    sync.Mutex
	files map[string]*memFile
	locks map[string]bool

	// Users and Groups map names to ids. Unlisted names fail to resolve.
	Users  map[string]int
	Groups map[string]int

	// Stubs are consulted in order for each command.
	Stubs []CommandStub
	// DefaultExitCode applies when no stub matches.
	DefaultExitCode int
	// Commands records every command line executed, in order.
	Commands []string
	// Binaries listed here are reported present by LookPath.
	Binaries map[string]bool

	HostName string
}

var _ Host = (*MemHost)(nil)

// NewMem returns an empty in-memory host with a root directory and the
// standard root user/group present.
func NewMem() *MemHost {
	m := &MemHost{
		files:    map[string]*memFile{},
		Users:    map[string]int{"root": 0},
		Groups:   map[string]int{"root": 0},
		Binaries: map[string]bool{},
		HostName: "testhost",
	}
	m.files["/"] = &memFile{mode: 0o755, isDir: true}
	return m
}

func clean(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

// AddStub appends a scripted command response.
func (m *MemHost) AddStub(s CommandStub) { m.Stubs = append(m.Stubs, s) }

// SetFile seeds a file, creating parent directories.
func (m *MemHost) SetFile(p, content string, mode fs.FileMode, uid, gid int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mkdirAllLocked(path.Dir(clean(p)), 0o755)
	m.files[clean(p)] = &memFile{data: []byte(content), mode: mode, uid: uid, gid: gid}
}

// Paths returns every path present, sorted. Useful for test assertions.
func (m *MemHost) Paths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.files))
	for p := range m.files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Ran reports whether any executed command line contains sub.
func (m *MemHost) Ran(sub string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.Commands {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func (m *MemHost) Run(ctx context.Context, name string, args ...string) (Result, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))

	m.mu.Lock()
	m.Commands = append(m.Commands, line)
	stubs := make([]CommandStub, len(m.Stubs))
	copy(stubs, m.Stubs)
	def := m.DefaultExitCode
	m.mu.Unlock()

	for _, s := range stubs {
		if s.Match == "" || !strings.Contains(line, s.Match) {
			continue
		}
		if s.Do != nil {
			s.Do(m, args)
		}
		if s.Err != nil {
			return Result{}, s.Err
		}
		return Result{Stdout: s.Stdout, Stderr: s.Stderr, ExitCode: s.ExitCode}, nil
	}
	return Result{ExitCode: def}, nil
}

func (m *MemHost) RunShell(ctx context.Context, script string) (Result, error) {
	return m.Run(ctx, "/bin/sh", "-c", script)
}

func (m *MemHost) LookPath(name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Binaries[name] {
		return "/usr/bin/" + name, nil
	}
	return "", fmt.Errorf("exec: %q: executable file not found in $PATH", name)
}

func (m *MemHost) ReadFile(p string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[clean(p)]
	if !ok || f.isDir {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	out := make([]byte, len(f.data))
	copy(out, f.data)
	return out, nil
}

// TryLock models flock within the process, which is enough to exercise the
// "another apply is already running" path in tests.
func (m *MemHost) TryLock(p string) (func() error, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks == nil {
		m.locks = map[string]bool{}
	}
	cp := clean(p)
	if m.locks[cp] {
		return nil, ErrLocked
	}
	m.locks[cp] = true
	return func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.locks, cp)
		return nil
	}, nil
}

// WriteFileSync has no separate durability story in memory.
func (m *MemHost) WriteFileSync(p string, data []byte, mode fs.FileMode) error {
	return m.WriteFile(p, data, mode)
}

func (m *MemHost) WriteFile(p string, data []byte, mode fs.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := clean(p)
	m.mkdirAllLocked(path.Dir(cp), 0o755)
	buf := make([]byte, len(data))
	copy(buf, data)
	if existing, ok := m.files[cp]; ok && !existing.isDir {
		// Preserve ownership across a content rewrite, matching rename-based
		// atomic writes followed by an explicit chown.
		existing.data = buf
		existing.mode = mode
		return nil
	}
	m.files[cp] = &memFile{data: buf, mode: mode}
	return nil
}

func (m *MemHost) Stat(p string) (FileInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := clean(p)
	f, ok := m.files[cp]
	if !ok {
		return FileInfo{}, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
	}
	return FileInfo{
		Path:   cp,
		Mode:   f.mode.Perm(),
		UID:    f.uid,
		GID:    f.gid,
		Size:   int64(len(f.data)),
		IsDir:  f.isDir,
		Target: f.target,
	}, nil
}

func (m *MemHost) ReadDir(p string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := clean(p)
	if f, ok := m.files[cp]; !ok || !f.isDir {
		return nil, &fs.PathError{Op: "readdir", Path: p, Err: fs.ErrNotExist}
	}
	seen := map[string]bool{}
	var out []string
	for q := range m.files {
		if q == cp || path.Dir(q) != cp {
			continue
		}
		base := path.Base(q)
		if !seen[base] {
			seen[base] = true
			out = append(out, base)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (m *MemHost) mkdirAllLocked(p string, mode fs.FileMode) {
	cp := clean(p)
	if cp == "/" {
		return
	}
	if f, ok := m.files[cp]; ok && f.isDir {
		return
	}
	m.mkdirAllLocked(path.Dir(cp), mode)
	m.files[cp] = &memFile{mode: mode, isDir: true}
}

func (m *MemHost) MkdirAll(p string, mode fs.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mkdirAllLocked(p, mode)
	return nil
}

func (m *MemHost) Remove(p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := clean(p)
	if _, ok := m.files[cp]; !ok {
		return &fs.PathError{Op: "remove", Path: p, Err: fs.ErrNotExist}
	}
	delete(m.files, cp)
	return nil
}

func (m *MemHost) RemoveAll(p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := clean(p)
	for q := range m.files {
		if q == cp || strings.HasPrefix(q, cp+"/") {
			delete(m.files, q)
		}
	}
	return nil
}

func (m *MemHost) Symlink(target, link string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cl := clean(link)
	m.mkdirAllLocked(path.Dir(cl), 0o755)
	m.files[cl] = &memFile{mode: 0o777, target: target}
	return nil
}

func (m *MemHost) Chmod(p string, mode fs.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[clean(p)]
	if !ok {
		return &fs.PathError{Op: "chmod", Path: p, Err: fs.ErrNotExist}
	}
	f.mode = mode
	return nil
}

func (m *MemHost) Chown(p string, uid, gid int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[clean(p)]
	if !ok {
		return &fs.PathError{Op: "chown", Path: p, Err: fs.ErrNotExist}
	}
	f.uid, f.gid = uid, gid
	return nil
}

func (m *MemHost) LookupUID(name string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.Users[name]; ok {
		return id, nil
	}
	if id, err := strconv.Atoi(name); err == nil {
		return id, nil
	}
	return 0, ErrUnknownUser
}

func (m *MemHost) LookupGID(name string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.Groups[name]; ok {
		return id, nil
	}
	if id, err := strconv.Atoi(name); err == nil {
		return id, nil
	}
	return 0, ErrUnknownUser
}

func (m *MemHost) LookupUserName(uid int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for n, id := range m.Users {
		if id == uid {
			return n, nil
		}
	}
	return strconv.Itoa(uid), nil
}

func (m *MemHost) LookupGroupName(gid int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for n, id := range m.Groups {
		if id == gid {
			return n, nil
		}
	}
	return strconv.Itoa(gid), nil
}

func (m *MemHost) Hostname() (string, error) { return m.HostName, nil }
