// Package host abstracts every side effect systemcd can have on a machine.
//
// Every resource provider talks to the world exclusively through the Host
// interface. That keeps providers honest (nothing reaches for os.* directly)
// and makes them unit-testable against MemHost without root or a real box.
package host

import (
	"context"
	"errors"
	"io/fs"
	"time"
)

// ErrNotExist is returned by Stat/ReadFile when a path is absent. Callers
// should test with errors.Is rather than comparing directly, since OSHost
// wraps the underlying *fs.PathError.
var ErrNotExist = fs.ErrNotExist

// ErrUnknownUser is returned when a user or group name cannot be resolved.
var ErrUnknownUser = errors.New("unknown user or group")

// Result captures the outcome of a command execution.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
}

// OK reports whether the command exited successfully.
func (r Result) OK() bool { return r.ExitCode == 0 }

// FileInfo is the subset of stat(2) that resources care about.
type FileInfo struct {
	Path  string
	Mode  fs.FileMode
	UID   int
	GID   int
	Size  int64
	IsDir bool
	// Target is the link destination when the path is a symlink.
	Target string
}

// Host is the seam between systemcd and the machine it manages.
type Host interface {
	// Run executes a command and waits for it to finish. A non-zero exit is
	// reported in Result.ExitCode, not as an error; error is reserved for
	// failures to execute at all (missing binary, context cancelled).
	Run(ctx context.Context, name string, args ...string) (Result, error)

	// RunShell executes a command line through /bin/sh -c.
	RunShell(ctx context.Context, script string) (Result, error)

	// LookPath reports whether an executable is available on PATH.
	LookPath(name string) (string, error)

	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, mode fs.FileMode) error
	Stat(path string) (FileInfo, error)
	ReadDir(path string) ([]string, error)
	MkdirAll(path string, mode fs.FileMode) error
	Remove(path string) error
	RemoveAll(path string) error
	Symlink(target, link string) error
	Chmod(path string, mode fs.FileMode) error
	Chown(path string, uid, gid int) error

	LookupUID(name string) (int, error)
	LookupGID(name string) (int, error)
	// LookupUserName resolves a uid back to a name for diff rendering.
	LookupUserName(uid int) (string, error)
	LookupGroupName(gid int) (string, error)

	Hostname() (string, error)
}
