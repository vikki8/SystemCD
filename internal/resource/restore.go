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
	info, err := c.Host.Stat(f.spec.Path)
	if errors.Is(err, host.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Target != "" {
		// What a symlink "contains" is where it points. Reading through it
		// would preserve someone else's file, and a later restore would write
		// those bytes back with the link's 0777 mode.
		return []byte(info.Target), nil
	}
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
	if prior["state"] == stateSymlink {
		target := string(blob)
		if target == "" {
			target = prior["_target"]
		}
		if target == "" {
			return fmt.Errorf("no symlink target was captured for %s", f.spec.Path)
		}
		if err := c.Host.Symlink(target, f.spec.Path); err != nil {
			return err
		}
		c.Log("restored %s as a symlink to %s", f.spec.Path, target)
		return nil
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
	// Writing publishes a new file, owned by systemcd. Whichever of owner and
	// group the manifest never managed is carried over from the file being
	// replaced, since systemcd did not change it and restoring the recorded
	// value would undo an operator's own later chown.
	// With no file to carry them over from, the recorded values are the best
	// description of what was there.
	keepUID, keepGID, replacing := -1, -1, false
	if info, err := c.Host.Stat(f.spec.Path); err == nil && !info.IsDir && info.Target == "" {
		keepUID, keepGID, replacing = info.UID, info.GID, true
	}
	// WriteFile sets the mode on the new file before publishing it; a chmod
	// by path afterwards would follow a symlink swapped in after the rename.
	if err := c.Host.WriteFile(f.spec.Path, blob, mode); err != nil {
		return err
	}
	c.Log("restored %s to its pre-adoption contents (%d bytes)", f.spec.Path, len(blob))
	return restoreOwnership(c, f.spec.Path, prior, f.spec.Owner != "" || !replacing, f.spec.Group != "" || !replacing, keepUID, keepGID)
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
	return restoreOwnership(c, d.spec.Path, prior, d.spec.Owner != "", d.spec.Group != "", -1, -1)
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
	// Observe records both fields whatever the manifest manages, but only a
	// field systemcd managed can have been changed by it. Restoring the other
	// would revert an operator's own later start, stop, enable or disable.
	if want, ok := prior["enabled"]; ok && s.spec.Enabled != nil {
		verb := "disable"
		if want == "true" {
			verb = "enable"
		}
		if err := s.run(c, verb, s.unit); err != nil {
			return err
		}
		c.Log("systemctl %s %s (pre-adoption state)", verb, s.unit)
	}
	if want, ok := prior["active"]; ok && s.spec.State != serviceUnmanaged {
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

// restoreOwnership puts back the recorded owner and group where restoreOwner
// and restoreGroup say to: normally only the ones the manifest managed, since
// systemcd never changed the other and restoring it would undo an operator's
// own later chown. keepUID and keepGID (-1 for none) are the ids to leave in
// place of a field that is not restored.
func restoreOwnership(c *Context, path string, prior State, restoreOwner, restoreGroup bool, keepUID, keepGID int) error {
	uid, gid := keepUID, keepGID
	if owner, ok := prior["owner"]; ok && restoreOwner {
		id, err := lookupIDOrNumeric(owner, c.Host.LookupUID)
		if err != nil {
			return fmt.Errorf("restoring owner %q: %w", owner, err)
		}
		uid = id
	}
	if group, ok := prior["group"]; ok && restoreGroup {
		id, err := lookupIDOrNumeric(group, c.Host.LookupGID)
		if err != nil {
			return fmt.Errorf("restoring group %q: %w", group, err)
		}
		gid = id
	}
	if uid == -1 && gid == -1 {
		return nil
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
	// Never read through a symlink: its "before" is someone else's file, and
	// printing it in a plan would leak whatever the link was pointed at.
	if info, err := c.Host.Stat(f.spec.Path); err == nil && info.Target != "" {
		return "", "", false, nil
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
