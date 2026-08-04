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
}

var _ Resource = (*File)(nil)
var _ Deletable = (*File)(nil)

func buildFile(doc *manifest.Document) (Resource, error) {
	f := &File{name: doc.Metadata.Name}
	if err := doc.DecodeSpec(&f.spec); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *File) ID() ID { return ID{Kind: "File", Name: f.name} }

func (f *File) Validate() error {
	if f.spec.Path == "" {
		return errors.New("spec.path is required")
	}
	if !strings.HasPrefix(f.spec.Path, "/") {
		return fmt.Errorf("spec.path %q must be absolute", f.spec.Path)
	}
	if f.spec.Content != "" && f.spec.Source != "" {
		return errors.New("spec.content and spec.source are mutually exclusive")
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
		p := filepath.Join(c.RepoRoot, filepath.Clean("/"+f.spec.Source))
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("spec.source %q: %w", f.spec.Source, err)
		}
		return data, nil
	}
	return []byte(f.spec.Content), nil
}

func (f *File) Desired(c *Context) (State, error) {
	s := State{"state": f.spec.State, "_path": f.spec.Path}
	if f.spec.State == Absent {
		return s, nil
	}
	data, err := f.content(c)
	if err != nil {
		return nil, err
	}
	s["checksum"] = checksum(data)
	s["mode"] = modeString(f.mode)
	if f.spec.Owner != "" {
		s["owner"] = f.spec.Owner
	}
	if f.spec.Group != "" {
		s["group"] = f.spec.Group
	}
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

	s := State{"state": Present, "_path": f.spec.Path, "mode": modeString(info.Mode)}
	data, err := c.Host.ReadFile(f.spec.Path)
	if err != nil {
		return nil, err
	}
	s["checksum"] = checksum(data)
	if name, err := c.Host.LookupUserName(info.UID); err == nil {
		s["owner"] = name
	}
	if name, err := c.Host.LookupGroupName(info.GID); err == nil {
		s["group"] = name
	}
	s["_bytes"] = strconv.FormatInt(info.Size, 10)
	return s, nil
}

func (f *File) Apply(c *Context, d Diff) error {
	if f.spec.State == Absent {
		return f.Delete(c)
	}
	if d.Has("state") || d.Has("checksum") {
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
	if d.Has("mode") || d.Has("state") {
		if err := c.Host.Chmod(f.spec.Path, f.mode); err != nil {
			return err
		}
	}
	return applyOwnership(c, f.spec.Path, f.spec.Owner, f.spec.Group, d)
}

func (f *File) Delete(c *Context) error {
	err := c.Host.Remove(f.spec.Path)
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
	if d.spec.Path == "" {
		return errors.New("spec.path is required")
	}
	if !strings.HasPrefix(d.spec.Path, "/") {
		return fmt.Errorf("spec.path %q must be absolute", d.spec.Path)
	}
	state, err := normalizePresence(d.spec.State)
	if err != nil {
		return err
	}
	d.spec.State = state
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
	if name, err := c.Host.LookupUserName(info.UID); err == nil {
		s["owner"] = name
	}
	if name, err := c.Host.LookupGroupName(info.GID); err == nil {
		s["group"] = name
	}
	return s, nil
}

func (d *Directory) Apply(c *Context, diff Diff) error {
	if d.spec.State == Absent {
		return d.Delete(c)
	}
	if diff.Has("state") {
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
	return applyOwnership(c, d.spec.Path, d.spec.Owner, d.spec.Group, diff)
}

func (d *Directory) Delete(c *Context) error {
	var err error
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
func applyOwnership(c *Context, path, owner, group string, d Diff) error {
	if owner == "" && group == "" {
		return nil
	}
	if !d.Has("owner") && !d.Has("group") && !d.Has("state") && !d.Has("checksum") {
		return nil
	}
	uid, gid := -1, -1
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

// backupExisting copies the current contents of path into the backup
// directory before it is overwritten.
func backupExisting(c *Context, path string) error {
	data, err := c.Host.ReadFile(path)
	if errors.Is(err, host.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	dest := filepath.Join(paths.BackupDir, strings.ReplaceAll(strings.TrimPrefix(path, "/"), "/", "_")+"."+stamp)
	if err := c.Host.MkdirAll(paths.BackupDir, 0o700); err != nil {
		return err
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
	return fs.FileMode(v).Perm(), nil
}
