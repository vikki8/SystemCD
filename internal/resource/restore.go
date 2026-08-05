package resource

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/vikki8/systemcd/internal/host"
)

// Restorer is implemented by kinds that can put a resource back the way
// systemcd found it before adopting it.
//
// This is what makes `release --restore` a promise rather than a slogan.
// Kinds that cannot honestly restore — a removed package cannot be
// un-removed to a version that may no longer exist in any repository — simply
// do not implement it, and `release` says so instead of pretending.
type Restorer interface {
	// Restore returns the resource to the observed state recorded at
	// adoption. Blob carries captured file contents for kinds that need it.
	Restore(c *Context, prior State, blob []byte) error
	// NeedsBaseline reports whether Restore requires captured content. A
	// checksum records that a file changed, not what it used to say.
	NeedsBaseline() bool
}

// Baseliner is implemented by kinds whose full contents must be captured at
// adoption time for a later restore to be possible.
type Baseliner interface {
	// CaptureBaseline returns the bytes to preserve, or nil when there is
	// nothing on the host to capture.
	CaptureBaseline(c *Context) ([]byte, error)
}

// CanRestore reports whether a resource supports being restored, and if not,
// why. The reason is shown to the operator rather than swallowed.
func CanRestore(r Resource) (bool, string) {
	res, ok := r.(Restorer)
	if !ok {
		return false, fmt.Sprintf(
			"%s cannot be restored: systemcd keeps no copy of what it replaced, "+
				"and re-creating it from a recorded checksum is not possible", r.ID().Kind)
	}
	_ = res
	return true, ""
}

// File ------------------------------------------------------------------

var (
	_ Restorer  = (*File)(nil)
	_ Baseliner = (*File)(nil)
)

func (f *File) NeedsBaseline() bool { return true }

func (f *File) CaptureBaseline(c *Context) ([]byte, error) {
	data, err := c.Host.ReadFile(f.spec.Path)
	if errors.Is(err, host.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

func (f *File) Restore(c *Context, prior State, blob []byte) error {
	// The file did not exist before systemcd created it, so putting the host
	// back means removing it again.
	if prior["state"] == Absent {
		err := c.Host.Remove(f.spec.Path)
		if errors.Is(err, host.ErrNotExist) {
			return nil
		}
		if err == nil {
			c.Log("removed %s (it did not exist before systemcd managed it)", f.spec.Path)
		}
		return err
	}
	if blob == nil {
		return fmt.Errorf("no baseline contents were captured for %s", f.spec.Path)
	}

	mode := f.mode
	if m, ok := prior["mode"]; ok {
		parsed, err := parseMode(m, f.mode)
		if err != nil {
			return err
		}
		mode = parsed
	}
	if err := c.Host.WriteFile(f.spec.Path, blob, mode); err != nil {
		return err
	}
	if err := c.Host.Chmod(f.spec.Path, mode); err != nil {
		return err
	}
	c.Log("restored %s to its pre-adoption contents (%d bytes)", f.spec.Path, len(blob))
	return restoreOwnership(c, f.spec.Path, prior)
}

// Directory -------------------------------------------------------------

var _ Restorer = (*Directory)(nil)

func (d *Directory) NeedsBaseline() bool { return false }

func (d *Directory) Restore(c *Context, prior State, _ []byte) error {
	if prior["state"] == Absent {
		err := c.Host.Remove(d.spec.Path)
		if errors.Is(err, host.ErrNotExist) {
			return nil
		}
		return err
	}
	if m, ok := prior["mode"]; ok {
		mode, err := parseMode(m, d.mode)
		if err != nil {
			return err
		}
		if err := c.Host.Chmod(d.spec.Path, mode); err != nil {
			return err
		}
	}
	return restoreOwnership(c, d.spec.Path, prior)
}

// SystemdUnit -----------------------------------------------------------

var (
	_ Restorer  = (*SystemdUnit)(nil)
	_ Baseliner = (*SystemdUnit)(nil)
)

func (u *SystemdUnit) NeedsBaseline() bool { return true }

func (u *SystemdUnit) CaptureBaseline(c *Context) ([]byte, error) {
	return u.file.CaptureBaseline(c)
}

func (u *SystemdUnit) Restore(c *Context, prior State, blob []byte) error {
	if err := u.file.Restore(c, prior, blob); err != nil {
		return err
	}
	// systemd's view of the unit must match the file we just put back.
	return u.daemonReload(c)
}

// Service ---------------------------------------------------------------

var _ Restorer = (*Service)(nil)

func (s *Service) NeedsBaseline() bool { return false }

func (s *Service) Restore(c *Context, prior State, _ []byte) error {
	if want, ok := prior["enabled"]; ok {
		verb := "disable"
		if want == "true" {
			verb = "enable"
		}
		if err := s.run(c, verb, s.unit); err != nil {
			return err
		}
		c.Log("systemctl %s %s (pre-adoption state)", verb, s.unit)
	}
	if want, ok := prior["active"]; ok {
		verb := "stop"
		if want == "active" {
			verb = "start"
		}
		if err := s.run(c, verb, s.unit); err != nil {
			return err
		}
		c.Log("systemctl %s %s (pre-adoption state)", verb, s.unit)
	}
	return nil
}

// Sysctl ----------------------------------------------------------------

var _ Restorer = (*Sysctl)(nil)

func (s *Sysctl) NeedsBaseline() bool { return false }

func (s *Sysctl) Restore(c *Context, prior State, _ []byte) error {
	for _, k := range s.keys {
		was, ok := prior["sysctl:"+k]
		if !ok || was == "<unreadable>" {
			continue
		}
		if err := run(c, "sysctl", "-w", k+"="+was); err != nil {
			return err
		}
		c.Log("sysctl %s = %s (pre-adoption value)", k, was)
	}
	// The drop-in is systemcd's own creation; releasing means taking it away.
	if s.persist() {
		err := c.Host.Remove(s.dropInPath())
		if err != nil && !errors.Is(err, host.ErrNotExist) {
			return err
		}
	}
	return nil
}

// restoreOwnership puts back the recorded owner and group, when they were
// captured and can still be resolved.
func restoreOwnership(c *Context, path string, prior State) error {
	owner, hasOwner := prior["owner"]
	group, hasGroup := prior["group"]
	if !hasOwner && !hasGroup {
		return nil
	}
	uid, gid := -1, -1
	if hasOwner {
		id, err := lookupIDOrNumeric(owner, c.Host.LookupUID)
		if err != nil {
			return fmt.Errorf("restoring owner %q: %w", owner, err)
		}
		uid = id
	}
	if hasGroup {
		id, err := lookupIDOrNumeric(group, c.Host.LookupGID)
		if err != nil {
			return fmt.Errorf("restoring group %q: %w", group, err)
		}
		gid = id
	}
	return c.Host.Chown(path, uid, gid)
}

func lookupIDOrNumeric(name string, lookup func(string) (int, error)) (int, error) {
	if id, err := strconv.Atoi(strings.TrimSpace(name)); err == nil {
		return id, nil
	}
	return lookup(name)
}

// ContentDiffer is implemented by kinds whose drift is best explained by
// showing the text that changed rather than a pair of checksums.
type ContentDiffer interface {
	// ContentDiff returns the body currently on the host and the body the
	// manifest wants. ok is false when a text diff would be meaningless —
	// binary content, or a resource that is being removed.
	ContentDiff(c *Context) (before, after string, ok bool, err error)
}

var (
	_ ContentDiffer = (*File)(nil)
	_ ContentDiffer = (*SystemdUnit)(nil)
)

func (f *File) ContentDiff(c *Context) (string, string, bool, error) {
	// A file whose contents are marked sensitive must not have them printed,
	// however useful the diff would be.
	if f.spec.Sensitive || f.spec.State == Absent {
		return "", "", false, nil
	}
	after, err := f.content(c)
	if err != nil {
		return "", "", false, err
	}
	before, err := c.Host.ReadFile(f.spec.Path)
	if errors.Is(err, host.ErrNotExist) {
		before = nil
	} else if err != nil {
		return "", "", false, err
	}
	if !isTextContent(before) || !isTextContent(after) {
		return "", "", false, nil
	}
	return string(before), string(after), true, nil
}

func (u *SystemdUnit) ContentDiff(c *Context) (string, string, bool, error) {
	return u.file.ContentDiff(c)
}

// isTextContent rejects binaries by looking for NUL bytes.
func isTextContent(data []byte) bool {
	limit := len(data)
	if limit > 8000 {
		limit = 8000
	}
	for _, b := range data[:limit] {
		if b == 0 {
			return false
		}
	}
	return true
}
