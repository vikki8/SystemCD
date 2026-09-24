package resource

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vikki8/systemcd/internal/host"
	"github.com/vikki8/systemcd/internal/manifest"
	"github.com/vikki8/systemcd/internal/paths"
	"gopkg.in/yaml.v3"
)

func init() {
	Register("File", buildFile)
	Register("Directory", buildDirectory)
}

// FileSpec declares a managed file.
type FileSpec struct {
	Path string `yaml:"path"`
	// Content is the literal file body.
	Content string `yaml:"content,omitempty"`
	// Source names a file in the repository, relative to the repo root.
	// Exactly one of Content or Source may be set.
	Source string `yaml:"source,omitempty"`
	Mode   string `yaml:"mode,omitempty"`
	Owner  string `yaml:"owner,omitempty"`
	Group  string `yaml:"group,omitempty"`
	State  string `yaml:"state,omitempty"`
	// Backup copies the previous contents into the backup directory before
	// overwriting. On by default; the point of GitOps is that you can undo.
	Backup *bool `yaml:"backup,omitempty"`
	// Sensitive hides content hashes and diffs from output.
	Sensitive bool `yaml:"sensitive,omitempty"`
}

// File is the File resource kind.
type File struct {
	name string
	spec FileSpec
	mode fs.FileMode
	// contentUnmanaged is set when the manifest names neither content nor
	// source. The file is then held to its mode and ownership only, and
	// created empty if missing, rather than truncated on every apply: an
	// omitted field means "not managed", the same thing `content: null` in a
	// Patch means.
	contentUnmanaged bool
}

var _ Resource = (*File)(nil)
var _ Deletable = (*File)(nil)

func buildFile(doc *manifest.Document) (Resource, error) {
	f := &File{name: doc.Metadata.Name}
	if err := doc.DecodeSpec(&f.spec); err != nil {
		return nil, err
	}
	f.contentUnmanaged = !specHasKey(&doc.Spec, "content") && !specHasKey(&doc.Spec, "source")
	return f, nil
}

// specHasKey reports whether a document's spec mapping sets key at all, which
// is what tells an omitted field apart from one set to its zero value.
func specHasKey(spec *yaml.Node, key string) bool {
	n := spec
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1].Tag != "!!null"
		}
	}
	return false
}

func (f *File) ID() ID { return ID{Kind: "File", Name: f.name} }

func (f *File) Validate() error {
	p, err := cleanAbsPath(f.spec.Path)
	if err != nil {
		return err
	}
	if p == "/" {
		return errors.New("spec.path must name a file, not /")
	}
	f.spec.Path = p
	if f.spec.Content != "" && f.spec.Source != "" {
		return errors.New("spec.content and spec.source are mutually exclusive")
	}
	if f.spec.Source != "" && !filepath.IsLocal(f.spec.Source) {
		return fmt.Errorf("spec.source %q must be a relative path inside the repository", f.spec.Source)
	}
	state, err := normalizePresence(f.spec.State)
	if err != nil {
		return err
	}
	f.spec.State = state

	mode, err := parseMode(f.spec.Mode, 0o644)
	if err != nil {
		return err
	}
	f.mode = mode
	return nil
}

func (f *File) SensitiveFields() map[string]bool {
	if !f.spec.Sensitive {
		return nil
	}
	return map[string]bool{"checksum": true}
}

// content returns the bytes the file should contain.
func (f *File) content(c *Context) ([]byte, error) {
	if f.spec.Source != "" {
		p, err := repoPath(c.RepoRoot, f.spec.Source)
		if err != nil {
			return nil, fmt.Errorf("spec.source %q: %w", f.spec.Source, err)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("spec.source %q: %w", f.spec.Source, err)
		}
		return data, nil
	}
	return []byte(f.spec.Content), nil
}

// repoPath resolves a manifest-relative asset path inside the repository.
// A symlink committed to the repository is followed only as far as the
// repository root: a payload that resolves to /etc/shadow would otherwise be
// copied onto the host and printed in plan diffs.
func repoPath(root, rel string) (string, error) {
	if root == "" {
		return "", errors.New("no repository root to resolve it against")
	}
	if !filepath.IsLocal(rel) {
		return "", errors.New("must be a relative path inside the repository")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	p, err := filepath.EvalSymlinks(filepath.Join(realRoot, rel))
	if err != nil {
		return "", err
	}
	if inside, err := filepath.Rel(realRoot, p); err != nil || !filepath.IsLocal(inside) {
		return "", errors.New("resolves outside the repository")
	}
	return p, nil
}

func (f *File) Desired(c *Context) (State, error) {
	s := State{"state": f.spec.State, "_path": f.spec.Path}
	if f.spec.State == Absent {
		return s, nil
	}
	s["mode"] = modeString(f.mode)
	if f.spec.Owner != "" {
		s["owner"] = f.spec.Owner
	}
	if f.spec.Group != "" {
		s["group"] = f.spec.Group
	}
	if f.contentUnmanaged {
		return s, nil
	}
	data, err := f.content(c)
	if err != nil {
		return nil, err
	}
	s["checksum"] = checksum(data)
	s["_bytes"] = strconv.Itoa(len(data))
	return s, nil
}

func (f *File) Observe(c *Context) (State, error) {
	info, err := c.Host.Stat(f.spec.Path)
	if errors.Is(err, host.ErrNotExist) {
		return State{"state": Absent, "_path": f.spec.Path}, nil
	}
	if err != nil {
		return nil, err
	}
	if info.IsDir {
		return nil, fmt.Errorf("%s exists but is a directory", f.spec.Path)
	}
	if info.Target != "" {
		// A symlink is not the file the manifest describes. Reading through
		// it would report (and chmod, and print in a diff) whatever it points
		// at, and its lstat mode is always 0777, so it could never converge.
		// Reporting it as its own state makes Apply replace the link.
		return State{"state": stateSymlink, "_path": f.spec.Path, "_target": info.Target}, nil
	}
	if !info.Regular {
		// Reading a FIFO blocks until something writes to it, which would
		// hang every plan; a socket or device is not a config file either.
		return nil, errNotRegular(f.spec.Path)
	}

	s := State{"state": Present, "_path": f.spec.Path, "mode": modeString(info.Mode)}
	data, err := c.Host.ReadFile(f.spec.Path)
	if err != nil {
		return nil, err
	}
	s["checksum"] = checksum(data)
	if name, ok := observedOwner(c, info.UID, f.spec.Owner); ok {
		s["owner"] = name
	}
	if name, ok := observedGroup(c, info.GID, f.spec.Group); ok {
		s["group"] = name
	}
	s["_bytes"] = strconv.FormatInt(info.Size, 10)
	return s, nil
}

// errNotRegular refuses a path that holds a FIFO, socket or device.
func errNotRegular(path string) error {
	return fmt.Errorf("%s exists but is not a regular file (a FIFO, socket or device); systemcd will not read or replace it", path)
}

// stateSymlink is what File reports when its path holds a symbolic link
// rather than a regular file.
const stateSymlink = "symlink"

func (f *File) Apply(c *Context, d Diff) error {
	if f.spec.State == Absent {
		return f.Delete(c)
	}
	written := d.Has("state") || d.Has("checksum")
	// Writing replaces the file with a new one owned by systemcd, so whichever
	// of owner and group the manifest leaves unmanaged has to be carried over
	// from the file being replaced, or a www-data config silently becomes
	// root-only on its first content change.
	keepUID, keepGID := -1, -1
	if written {
		if info, err := c.Host.Stat(f.spec.Path); err == nil && !info.IsDir && info.Target == "" {
			keepUID, keepGID = info.UID, info.GID
		}
		data, err := f.content(c)
		if err != nil {
			return err
		}
		if f.backupEnabled() {
			if err := backupExisting(c, f.spec.Path); err != nil {
				return err
			}
		}
		if err := c.Host.WriteFile(f.spec.Path, data, f.mode); err != nil {
			return err
		}
		c.Log("wrote %s (%d bytes)", f.spec.Path, len(data))
	}
	// WriteFile already set the mode on the new file before publishing it. A
	// second chmod by path would follow a symlink swapped in after the rename.
	if d.Has("mode") && !written {
		if err := c.Host.Chmod(f.spec.Path, f.mode); err != nil {
			return err
		}
	}
	return applyOwnership(c, f.spec.Path, f.spec.Owner, f.spec.Group, d, keepUID, keepGID)
}

func (f *File) Delete(c *Context) error {
	info, err := c.Host.Stat(f.spec.Path)
	if errors.Is(err, host.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir {
		return fmt.Errorf("%s exists but is a directory; a File resource will not remove it", f.spec.Path)
	}
	// Back up before removing, not just before overwriting: a delete is the
	// one operation with nothing left to recover from afterwards.
	if f.backupEnabled() {
		if err := backupExisting(c, f.spec.Path); err != nil {
			return err
		}
	}
	err = c.Host.Remove(f.spec.Path)
	if errors.Is(err, host.ErrNotExist) {
		return nil
	}
	if err == nil {
		c.Log("removed %s", f.spec.Path)
	}
	return err
}

func (f *File) backupEnabled() bool {
	return f.spec.Backup == nil || *f.spec.Backup
}

// DirectorySpec declares a managed directory.
type DirectorySpec struct {
	Path  string `yaml:"path"`
	Mode  string `yaml:"mode,omitempty"`
	Owner string `yaml:"owner,omitempty"`
	Group string `yaml:"group,omitempty"`
	State string `yaml:"state,omitempty"`
	// Recursive allows `state: absent` to remove a non-empty directory.
	Recursive bool `yaml:"recursive,omitempty"`
}

// Directory is the Directory resource kind.
type Directory struct {
	name string
	spec DirectorySpec
	mode fs.FileMode
}

var _ Resource = (*Directory)(nil)
var _ Deletable = (*Directory)(nil)

func buildDirectory(doc *manifest.Document) (Resource, error) {
	d := &Directory{name: doc.Metadata.Name}
	if err := doc.DecodeSpec(&d.spec); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Directory) ID() ID { return ID{Kind: "Directory", Name: d.name} }

func (d *Directory) Validate() error {
	p, err := cleanAbsPath(d.spec.Path)
	if err != nil {
		return err
	}
	d.spec.Path = p
	state, err := normalizePresence(d.spec.State)
	if err != nil {
		return err
	}
	d.spec.State = state
	if p == "/" && (state == Absent || d.spec.Recursive) {
		return errors.New("spec.path / cannot be removed; refusing `state: absent` or `recursive: true` on it")
	}
	mode, err := parseMode(d.spec.Mode, 0o755)
	if err != nil {
		return err
	}
	d.mode = mode
	return nil
}

func (d *Directory) Desired(c *Context) (State, error) {
	s := State{"state": d.spec.State, "_path": d.spec.Path}
	if d.spec.State == Absent {
		return s, nil
	}
	s["mode"] = modeString(d.mode)
	if d.spec.Owner != "" {
		s["owner"] = d.spec.Owner
	}
	if d.spec.Group != "" {
		s["group"] = d.spec.Group
	}
	return s, nil
}

func (d *Directory) Observe(c *Context) (State, error) {
	info, err := c.Host.Stat(d.spec.Path)
	if errors.Is(err, host.ErrNotExist) {
		return State{"state": Absent, "_path": d.spec.Path}, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir {
		return nil, fmt.Errorf("%s exists but is not a directory", d.spec.Path)
	}
	s := State{"state": Present, "_path": d.spec.Path, "mode": modeString(info.Mode)}
	if name, ok := observedOwner(c, info.UID, d.spec.Owner); ok {
		s["owner"] = name
	}
	if name, ok := observedGroup(c, info.GID, d.spec.Group); ok {
		s["group"] = name
	}
	return s, nil
}

func (d *Directory) Apply(c *Context, diff Diff) error {
	if d.spec.State == Absent {
		return d.Delete(c)
	}
	if diff.Has("state") {
		// MkdirAll gives every directory it creates the same mode, so a 0700
		// leaf would otherwise make its missing parents untraversable too.
		if err := c.Host.MkdirAll(filepath.Dir(d.spec.Path), 0o755); err != nil {
			return err
		}
		if err := c.Host.MkdirAll(d.spec.Path, d.mode); err != nil {
			return err
		}
		c.Log("created directory %s", d.spec.Path)
	}
	if diff.Has("mode") || diff.Has("state") {
		if err := c.Host.Chmod(d.spec.Path, d.mode); err != nil {
			return err
		}
	}
	return applyOwnership(c, d.spec.Path, d.spec.Owner, d.spec.Group, diff, -1, -1)
}

func (d *Directory) Delete(c *Context) error {
	if d.spec.Path == "/" {
		return errors.New("refusing to remove /")
	}
	info, err := c.Host.Stat(d.spec.Path)
	if errors.Is(err, host.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir {
		// Prune reaches here without an Observe. Whatever now sits at the
		// path is not the directory systemcd managed, and removing it would
		// take a file (or a symlink) with no backup.
		return fmt.Errorf("%s exists but is not a directory; a Directory resource will not remove it", d.spec.Path)
	}
	if d.spec.Recursive {
		err = c.Host.RemoveAll(d.spec.Path)
	} else {
		err = c.Host.Remove(d.spec.Path)
	}
	if errors.Is(err, host.ErrNotExist) {
		return nil
	}
	return err
}

// applyOwnership resolves owner/group names and chowns when either differs.
// keepUID and keepGID (-1 for none) are the ids of a file that was just
// replaced; they are put back for whichever of owner and group the manifest
// does not manage.
func applyOwnership(c *Context, path, owner, group string, d Diff, keepUID, keepGID int) error {
	replaced := keepUID != -1 || keepGID != -1
	if owner == "" && group == "" && !replaced {
		return nil
	}
	if !replaced && !d.Has("owner") && !d.Has("group") && !d.Has("state") && !d.Has("checksum") {
		return nil
	}
	uid, gid := keepUID, keepGID
	if owner != "" {
		id, err := c.Host.LookupUID(owner)
		if err != nil {
			return fmt.Errorf("owner %q: %w", owner, err)
		}
		uid = id
	}
	if group != "" {
		id, err := c.Host.LookupGID(group)
		if err != nil {
			return fmt.Errorf("group %q: %w", group, err)
		}
		gid = id
	}
	return c.Host.Chown(path, uid, gid)
}

// observedOwner renders a file's uid for comparison with the manifest. When
// the manifest's owner resolves to that same uid it is reported verbatim, so
// `owner: "33"` or a second name for the same uid is not permanent drift
// against the name the account database happens to list first.
func observedOwner(c *Context, uid int, want string) (string, bool) {
	if want != "" {
		if id, err := c.Host.LookupUID(want); err == nil && id == uid {
			return want, true
		}
	}
	name, err := c.Host.LookupUserName(uid)
	return name, err == nil
}

// observedGroup is observedOwner for the group.
func observedGroup(c *Context, gid int, want string) (string, bool) {
	if want != "" {
		if id, err := c.Host.LookupGID(want); err == nil && id == gid {
			return want, true
		}
	}
	name, err := c.Host.LookupGroupName(gid)
	return name, err == nil
}

// cleanAbsPath validates spec.path and returns it in canonical form, so
// /etc/x, /etc//x and /etc/./x are one path to claims and to the host.
func cleanAbsPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("spec.path is required")
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("spec.path %q must be absolute", p)
	}
	return filepath.Clean(p), nil
}

// backupExisting copies the current contents of path into the backup
// directory before it is overwritten.
func backupExisting(c *Context, path string) error {
	info, err := c.Host.Stat(path)
	if errors.Is(err, host.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Target != "" {
		// A symlink's contents belong to whatever it points at, which
		// replacing or removing the link leaves untouched. Reading through it
		// could also copy a device or another user's file into the backups.
		return nil
	}
	if !info.Regular {
		return errNotRegular(path)
	}
	data, err := c.Host.ReadFile(path)
	if errors.Is(err, host.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	base := filepath.Join(paths.BackupDir, strings.ReplaceAll(strings.TrimPrefix(path, "/"), "/", "_")+"."+stamp)
	if err := c.Host.MkdirAll(paths.BackupDir, 0o700); err != nil {
		return err
	}
	// Two backups in the same second (the same path twice, or /etc/a_b and
	// /etc/a/b) must not overwrite each other: a backup is the only copy.
	dest := base
	for i := 1; ; i++ {
		_, err := c.Host.Stat(dest)
		if errors.Is(err, host.ErrNotExist) {
			break
		}
		if err != nil {
			return err
		}
		dest = base + "." + strconv.Itoa(i)
	}
	return c.Host.WriteFile(dest, data, 0o600)
}

func checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func modeString(m fs.FileMode) string {
	return "0" + strconv.FormatUint(uint64(m.Perm()), 8)
}

func parseMode(s string, def fs.FileMode) (fs.FileMode, error) {
	if s == "" {
		return def, nil
	}
	v, err := strconv.ParseUint(strings.TrimPrefix(s, "0o"), 8, 32)
	if err != nil {
		return 0, fmt.Errorf("spec.mode %q is not an octal permission string", s)
	}
	if v > 0o777 {
		// fs.FileMode keeps setuid, setgid and sticky outside the low bits,
		// so "1777" would silently become 0777: a world-writable directory
		// without the sticky bit, reported as in sync.
		return 0, fmt.Errorf("spec.mode %q: setuid, setgid and sticky bits are not supported; use a mode no higher than 0777", s)
	}
	return fs.FileMode(v).Perm(), nil
}
