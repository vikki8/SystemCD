package resource

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vikki8/systemcd/internal/host"
	"github.com/vikki8/systemcd/internal/paths"
)

// renameHost is MemHost with OSHost's write semantics: WriteFile publishes a
// brand-new file by rename, so the result belongs to whoever wrote it (root)
// rather than keeping the owner of the file it replaced.
type renameHost struct{ *host.MemHost }

func (h renameHost) WriteFile(p string, data []byte, mode fs.FileMode) error {
	_ = h.MemHost.Remove(p)
	return h.MemHost.WriteFile(p, data, mode)
}

// rootedHost returns an OSHost confined to a scratch directory, for tests
// that need real symlink semantics.
func rootedHost(t *testing.T) (*host.OSHost, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	return host.NewRootedOS(root), root
}

func TestFileRewriteKeepsOwnershipTheManifestDoesNotManage(t *testing.T) {
	// A config owned by the service account, with no owner/group in the
	// manifest: a content change must not hand it to root.
	h := renameHost{host.NewMem()}
	h.Users["www-data"] = 33
	h.Groups["www-data"] = 33
	h.SetFile("/etc/app.conf", "old\n", 0o640, 33, 33)
	h.SetFile("/etc/other.conf", "old\n", 0o640, 33, 33)
	c := newContext(h)

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: app
spec:
  path: /etc/app.conf
  content: "new\n"
  mode: "0640"
`)
	converge(t, r, c)
	info, _ := h.Stat("/etc/app.conf")
	if info.UID != 33 || info.GID != 33 {
		t.Errorf("ownership = %d:%d after a content rewrite, want the original 33:33", info.UID, info.GID)
	}
	assertConverged(t, r, c)

	// Managing only the owner still keeps the unmanaged group.
	r2 := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: other
spec:
  path: /etc/other.conf
  content: "new\n"
  mode: "0640"
  owner: root
`)
	converge(t, r2, c)
	info, _ = h.Stat("/etc/other.conf")
	if info.UID != 0 || info.GID != 33 {
		t.Errorf("ownership = %d:%d, want 0:33 (owner managed, group kept)", info.UID, info.GID)
	}
	assertConverged(t, r2, c)
}

func TestFileOwnerGivenByIDIsIdempotent(t *testing.T) {
	h := host.NewMem()
	h.Users["www-data"] = 33
	h.Groups["www-data"] = 33
	c := newContext(h)

	for _, src := range []string{`
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: by-id
spec:
  path: /etc/by-id.conf
  content: "x\n"
  owner: "33"
  group: "33"
`, `
apiVersion: systemcd.dev/v1
kind: Directory
metadata:
  name: by-id
spec:
  path: /srv/by-id
  owner: "33"
  group: "33"
`} {
		r := buildFrom(t, src)
		converge(t, r, c)
		// The host lists uid 33 as www-data; the manifest said 33. Same
		// account, so the next plan must be clean.
		assertConverged(t, r, c)
	}
}

func TestFileSymlinkIsReplacedNotFollowed(t *testing.T) {
	h, root := rootedHost(t)
	c := newContext(h)
	target := filepath.Join(root, "etc", "target")
	if err := os.WriteFile(target, []byte("same\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(root, "etc", "link")); err != nil {
		t.Fatal(err)
	}

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: link
spec:
  path: /etc/link
  content: "same\n"
`)
	converge(t, r, c)

	// The file the link pointed at is not ours and must be untouched.
	ti, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if ti.Mode().Perm() != 0o600 {
		t.Errorf("link target mode = %o, want 0600: apply chmodded through the symlink", ti.Mode().Perm())
	}
	li, err := os.Lstat(filepath.Join(root, "etc", "link"))
	if err != nil {
		t.Fatal(err)
	}
	if li.Mode()&fs.ModeSymlink != 0 {
		t.Error("the managed path is still a symlink; it should have been replaced by the declared file")
	}
	assertConverged(t, r, c)
}

func TestFileContentDiffDoesNotReadThroughSymlink(t *testing.T) {
	h, root := rootedHost(t)
	c := newContext(h)
	if err := os.WriteFile(filepath.Join(root, "etc", "shadow"), []byte("root:TOPSECRET:19000::::::\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("shadow", filepath.Join(root, "etc", "motd")); err != nil {
		t.Fatal(err)
	}
	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: motd
spec:
  path: /etc/motd
  content: "hello\n"
`)
	before, _, _, err := r.(ContentDiffer).ContentDiff(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(before, "TOPSECRET") {
		t.Fatalf("a plan diff would print the symlink target's contents: %q", before)
	}
}

func TestFileDeleteDoesNotBackUpThroughSymlink(t *testing.T) {
	h, root := rootedHost(t)
	c := newContext(h)
	if err := os.WriteFile(filepath.Join(root, "etc", "secret"), []byte("TOPSECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("secret", filepath.Join(root, "etc", "old.conf")); err != nil {
		t.Fatal(err)
	}
	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: old
spec:
  path: /etc/old.conf
  state: absent
`)
	converge(t, r, c)
	if _, err := os.Lstat(filepath.Join(root, "etc", "old.conf")); !os.IsNotExist(err) {
		t.Errorf("the symlink should be removed, lstat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "etc", "secret")); err != nil {
		t.Errorf("the link target must survive: %v", err)
	}
	names, _ := h.ReadDir(paths.BackupDir)
	for _, n := range names {
		data, _ := h.ReadFile(paths.BackupDir + "/" + n)
		if strings.Contains(string(data), "TOPSECRET") {
			t.Errorf("backup %s copied the symlink target's contents", n)
		}
	}
	assertConverged(t, r, c)
}

func TestFileDeleteRefusesADirectory(t *testing.T) {
	h, root := rootedHost(t)
	c := newContext(h)
	dir := filepath.Join(root, "etc", "conf.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: conf
spec:
  path: /etc/conf.d
  backup: false
`)
	if err := r.(Deletable).Delete(c); err == nil {
		t.Error("pruning a File whose path is now a directory should be refused")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the directory was removed by a File prune: %v", err)
	}
}

func TestFileModeRejectsSpecialBits(t *testing.T) {
	for _, kind := range []string{"File", "Directory"} {
		for _, mode := range []string{"1777", "2775", "4755", "0o1777"} {
			err := buildErr(t, "apiVersion: systemcd.dev/v1\nkind: "+kind+"\nmetadata:\n  name: t\nspec:\n  path: /srv/t\n  mode: \""+mode+"\"\n")
			if err == nil || !strings.Contains(err.Error(), "sticky") {
				t.Errorf("%s mode %s: error = %v, want a refusal instead of silently dropping the bit", kind, mode, err)
			}
		}
	}
}

func TestFilePathIsCanonicalForClaims(t *testing.T) {
	cases := []struct{ src, want string }{
		{"kind: File\nmetadata:\n  name: a\nspec:\n  path: /etc//nginx/./nginx.conf\n", "path:/etc/nginx/nginx.conf"},
		{"kind: File\nmetadata:\n  name: b\nspec:\n  path: /etc/nginx/conf.d/../nginx.conf\n", "path:/etc/nginx/nginx.conf"},
		{"kind: Directory\nmetadata:\n  name: c\nspec:\n  path: /opt/app/\n", "path:/opt/app"},
	}
	for _, tc := range cases {
		r := buildFrom(t, "apiVersion: systemcd.dev/v1\n"+tc.src)
		got := ClaimStrings(ClaimsOf(r))
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s claims = %v, want [%s]: spellings of one path must collide", r.ID(), got, tc.want)
		}
	}
}

func TestFileSourceMustStayInsideTheRepository(t *testing.T) {
	for _, src := range []string{"../outside.conf", "/etc/shadow", "files/../../x"} {
		err := buildErr(t, "apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: t\nspec:\n  path: /etc/t\n  source: "+src+"\n")
		if err == nil || !strings.Contains(err.Error(), "inside the repository") {
			t.Errorf("source %q: error = %v, want it rejected at validation", src, err)
		}
	}

	// A symlink committed to the repository must not reach out of it.
	repo := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("TOPSECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "files", "leak")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "files", "real"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(repo, "files", "alias")); err != nil {
		t.Fatal(err)
	}
	c := newContext(host.NewMem())
	c.RepoRoot = repo

	leak := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: t\nspec:\n  path: /etc/t\n  source: files/leak\n")
	if _, err := leak.Desired(c); err == nil || !strings.Contains(err.Error(), "outside the repository") {
		t.Errorf("error = %v, want the escaping symlink refused", err)
	}
	// A symlink that stays inside the repository is fine.
	alias := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: t\nspec:\n  path: /etc/t\n  source: files/alias\n")
	if _, err := alias.Desired(c); err != nil {
		t.Errorf("an in-repository symlink should resolve: %v", err)
	}
}

func TestDirectoryRefusesToRemoveRoot(t *testing.T) {
	for _, spec := range []string{
		"  path: /\n  recursive: true\n",
		"  path: /\n  state: absent\n",
		"  path: /etc/..\n  recursive: true\n",
	} {
		err := buildErr(t, "apiVersion: systemcd.dev/v1\nkind: Directory\nmetadata:\n  name: t\nspec:\n"+spec)
		if err == nil {
			t.Errorf("spec %q was accepted; it can only ever mean deleting /", spec)
		}
	}
	if err := buildErr(t, "apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: t\nspec:\n  path: /\n"); err == nil {
		t.Error("a File at / was accepted")
	}

	// A plain Directory on / may manage its mode, but a prune never removes it.
	h := host.NewMem()
	h.SetFile("/etc/keep", "x", 0o644, 0, 0)
	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Directory\nmetadata:\n  name: root\nspec:\n  path: /\n")
	if err := r.(Deletable).Delete(newContext(h)); err == nil {
		t.Error("deleting / should be refused")
	}
	if _, err := h.Stat("/"); err != nil {
		t.Error("/ was removed")
	}
}

func TestDirectoryCreatesMissingParentsTraversable(t *testing.T) {
	h := host.NewMem()
	c := newContext(h)
	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: Directory
metadata:
  name: secret
spec:
  path: /srv/app/secret
  mode: "0700"
`)
	converge(t, r, c)
	for _, p := range []string{"/srv", "/srv/app"} {
		info, err := h.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode.Perm() != 0o755 {
			t.Errorf("%s mode = %o, want 0755: a private leaf must not lock other users out of its parents", p, info.Mode.Perm())
		}
	}
	info, _ := h.Stat("/srv/app/secret")
	if info.Mode.Perm() != 0o700 {
		t.Errorf("leaf mode = %o, want 0700", info.Mode.Perm())
	}
	assertConverged(t, r, c)
}

func TestDirectoryPruneRefusesANonDirectory(t *testing.T) {
	h := host.NewMem()
	h.SetFile("/srv/data", "someone's file\n", 0o644, 0, 0)
	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: Directory
metadata:
  name: data
spec:
  path: /srv/data
  recursive: true
`)
	if err := r.(Deletable).Delete(newContext(h)); err == nil {
		t.Error("pruning a Directory whose path now holds a file should be refused")
	}
	if _, err := h.ReadFile("/srv/data"); err != nil {
		t.Errorf("the file was removed without a backup: %v", err)
	}
}

func TestBackupsInTheSameSecondDoNotOverwriteEachOther(t *testing.T) {
	// /etc/a_b and /etc/a/b share a flattened backup name; taken in the same
	// second, the second backup used to replace the first.
	for attempt := 0; attempt < 5; attempt++ {
		h := host.NewMem()
		h.SetFile("/etc/a_b", "first\n", 0o644, 0, 0)
		h.SetFile("/etc/a/b", "second\n", 0o644, 0, 0)
		c := newContext(h)

		start := time.Now().UTC().Truncate(time.Second)
		if err := backupExisting(c, "/etc/a_b"); err != nil {
			t.Fatal(err)
		}
		if err := backupExisting(c, "/etc/a/b"); err != nil {
			t.Fatal(err)
		}
		if !time.Now().UTC().Truncate(time.Second).Equal(start) {
			continue // straddled a second boundary; the names differ anyway
		}
		names, _ := h.ReadDir(paths.BackupDir)
		got := map[string]bool{}
		for _, n := range names {
			data, _ := h.ReadFile(paths.BackupDir + "/" + n)
			got[string(data)] = true
		}
		if !got["first\n"] || !got["second\n"] {
			t.Fatalf("backups = %v, want both originals preserved", names)
		}
		return
	}
	t.Skip("could not take two backups within one second")
}
