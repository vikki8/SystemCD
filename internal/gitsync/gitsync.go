// Package gitsync keeps a local checkout of the desired-state repository in
// step with a remote branch.
//
// It shells out to git rather than embedding a git implementation: the host
// already has git configured with the credentials, SSH agent, and proxy
// settings the operator expects, and inheriting that is a feature.
package gitsync

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/vikki8/systemcd/internal/host"
)

// Source describes where desired state comes from.
type Source struct {
	// URL is the remote (https or ssh). Empty means the working directory is
	// already the repository and only the revision is read.
	URL string
	// Ref is the branch, tag, or commit to check out.
	Ref string
	// Dir is the local checkout path.
	Dir string
	// Depth limits history for a fresh clone; 1 is the usual choice.
	Depth int
}

// Syncer performs git operations against a host.
type Syncer struct {
	Host host.Host
	Logf func(format string, args ...any)
}

// New returns a Syncer bound to a host.
func New(h host.Host) *Syncer { return &Syncer{Host: h} }

func (s *Syncer) log(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Sync makes Dir contain Ref from URL and returns the resolved commit.
// It clones on first use and fetches thereafter.
func (s *Syncer) Sync(ctx context.Context, src Source) (string, error) {
	if src.Dir == "" {
		return "", errors.New("gitsync: no directory configured")
	}
	ref := src.Ref
	if ref == "" {
		ref = "main"
	}
	// Both end up as git arguments; one starting with "-" would be read as
	// an option (--upload-pack=... runs a command).
	if err := checkArg("repository URL", src.URL); err != nil {
		return "", err
	}
	if err := checkArg("ref", ref); err != nil {
		return "", err
	}

	exists, err := s.isRepo(ctx, src.Dir)
	if err != nil {
		return "", err
	}

	if !exists {
		if src.URL == "" {
			return "", fmt.Errorf("gitsync: %s is not a git repository and no repo URL is configured", src.Dir)
		}
		if err := s.clone(ctx, src, ref); err != nil {
			return "", err
		}
	} else if src.URL != "" {
		if err := s.fetch(ctx, src, ref); err != nil {
			return "", err
		}
	}

	return s.Revision(ctx, src.Dir)
}

func (s *Syncer) isRepo(ctx context.Context, dir string) (bool, error) {
	if _, err := s.Host.Stat(dir + "/.git"); err == nil {
		return true, nil
	} else if !errors.Is(err, host.ErrNotExist) {
		return false, err
	}
	return false, nil
}

func (s *Syncer) clone(ctx context.Context, src Source, ref string) error {
	if err := s.Host.MkdirAll(src.Dir, 0o755); err != nil {
		return err
	}
	args := []string{"clone", "--branch", ref}
	if src.Depth > 0 {
		args = append(args, "--depth", fmt.Sprint(src.Depth))
	}
	args = append(args, "--", src.URL, src.Dir)
	s.log("cloning %s (%s) into %s", redactURL(src.URL), ref, src.Dir)

	if err := s.git(ctx, "", args...); err != nil {
		// A commit SHA cannot be passed to --branch; fall back to a plain
		// clone followed by a checkout.
		s.log("branch clone failed (%v); retrying with a full clone", err)
		if err := s.git(ctx, "", "clone", "--", src.URL, src.Dir); err != nil {
			return err
		}
		target, err := s.resolveLocal(ctx, src.Dir, ref)
		if err != nil {
			return err
		}
		return s.checkoutClean(ctx, src.Dir, target)
	}
	return nil
}

func (s *Syncer) fetch(ctx context.Context, src Source, ref string) error {
	s.log("fetching %s from %s", ref, redactURL(src.URL))
	if err := s.git(ctx, src.Dir, "remote", "set-url", "origin", src.URL); err != nil {
		// A checkout made by hand may not call its remote "origin".
		if addErr := s.git(ctx, src.Dir, "remote", "add", "origin", src.URL); addErr != nil {
			return err
		}
	}

	target := "FETCH_HEAD"
	if err := s.git(ctx, src.Dir, "fetch", "--prune", "origin", ref); err != nil {
		// The ref may be a tag or a raw SHA that a refspec fetch misses.
		// FETCH_HEAD after a plain fetch is whatever branch the remote lists
		// first, not the ref, so the ref has to be resolved explicitly.
		if err2 := s.git(ctx, src.Dir, "fetch", "--prune", "--tags", "origin"); err2 != nil {
			return err
		}
		resolved, rerr := s.resolveLocal(ctx, src.Dir, ref)
		if rerr != nil && isHex(ref) && s.isShallow(src.Dir) {
			// A server that will not serve an older commit by id still
			// serves the history leading to it.
			if s.git(ctx, src.Dir, "fetch", "--unshallow", "origin") == nil {
				resolved, rerr = s.resolveLocal(ctx, src.Dir, ref)
			}
		}
		if rerr != nil {
			return err
		}
		target = resolved
	}
	// The checkout is a cache of the remote, never a place to edit. Local
	// changes there are drift, not work to preserve, and they must not be
	// able to block the update either.
	return s.checkoutClean(ctx, src.Dir, target)
}

func (s *Syncer) isShallow(dir string) bool {
	_, err := s.Host.Stat(dir + "/.git/shallow")
	return err == nil
}

// resolveLocal finds the commit a ref names after a fetch that could not ask
// for it by name. Only remote-tracking branches, tags and commit ids are
// looked up: a bare branch name would resolve to the local branch the clone
// created, which is never updated and would silently roll the host back to
// clone time.
func (s *Syncer) resolveLocal(ctx context.Context, dir, ref string) (string, error) {
	candidates := []string{"refs/remotes/origin/" + ref, "refs/tags/" + ref}
	if isHex(ref) {
		candidates = append(candidates, ref)
	}
	for _, c := range candidates {
		res, err := s.Host.Run(ctx, "git", "-C", dir, "rev-parse", "--verify", "--quiet", c+"^{commit}")
		if err == nil && res.OK() {
			if sha := strings.TrimSpace(res.Stdout); sha != "" {
				return sha, nil
			}
		}
	}
	return "", fmt.Errorf("gitsync: %q is not a branch, tag or commit on the remote", ref)
}

// checkoutClean moves the working tree to rev, discarding local edits and
// removing files git does not track. An untracked manifest dropped into the
// checkout would otherwise be loaded and applied as if it came from git.
func (s *Syncer) checkoutClean(ctx context.Context, dir, rev string) error {
	if err := s.git(ctx, dir, "checkout", "--force", "--detach", rev); err != nil {
		return err
	}
	return s.git(ctx, dir, "clean", "-ffdx")
}

// Checkout moves an existing checkout to a specific revision, used by
// `rollback` to return to a known-good commit.
func (s *Syncer) Checkout(ctx context.Context, dir, revision string) error {
	if err := checkArg("revision", revision); err != nil {
		return err
	}
	if revision == "" {
		return errors.New("gitsync: no revision given")
	}
	if !s.hasCommit(ctx, dir, revision) {
		// A shallow clone may not hold an older commit yet; ask for it by
		// id, and if the server will not serve that, for the full history.
		s.log("%s is not in the local checkout; fetching it", revision)
		if s.git(ctx, dir, "fetch", "origin", revision) != nil && s.isShallow(dir) {
			_ = s.git(ctx, dir, "fetch", "--unshallow", "--tags", "origin")
		}
	}
	return s.checkoutClean(ctx, dir, revision)
}

func (s *Syncer) hasCommit(ctx context.Context, dir, rev string) bool {
	res, err := s.Host.Run(ctx, "git", "-C", dir, "cat-file", "-e", rev+"^{commit}")
	return err == nil && res.OK()
}

// Restore puts a checkout back exactly at its current HEAD and returns that
// revision. After a sync that failed part-way, HEAD still names the last
// revision that was fully checked out, but the working tree may hold a mix
// of two; this is what makes it safe to keep reconciling the last good
// revision while the remote is unreachable.
func (s *Syncer) Restore(ctx context.Context, dir string) (string, error) {
	exists, err := s.isRepo(ctx, dir)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("gitsync: %s is not a git checkout", dir)
	}
	if err := s.checkoutClean(ctx, dir, "HEAD"); err != nil {
		return "", err
	}
	return s.Revision(ctx, dir)
}

// Revision resolves the current commit of a checkout. A directory that is not
// a git repository yields an empty revision rather than an error, so systemcd
// still works against a plain directory of manifests.
func (s *Syncer) Revision(ctx context.Context, dir string) (string, error) {
	res, err := s.Host.Run(ctx, "git", "-C", dir, "rev-parse", "HEAD")
	if err != nil {
		return "", nil
	}
	if !res.OK() {
		return "", nil
	}
	return strings.TrimSpace(res.Stdout), nil
}

// Describe returns a human-friendly revision label such as
// "a1b2c3d (main) Add nginx role".
func (s *Syncer) Describe(ctx context.Context, dir string) string {
	res, err := s.Host.Run(ctx, "git", "-C", dir, "log", "-1", "--pretty=%h %s")
	if err != nil || !res.OK() {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

func (s *Syncer) git(ctx context.Context, dir string, args ...string) error {
	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	res, err := s.Host.Run(ctx, "git", full...)
	if err != nil {
		return errors.New(scrub(fmt.Sprintf("git %s: %v", strings.Join(args, " "), err), args))
	}
	if !res.OK() {
		return errors.New(scrub(fmt.Sprintf("git %s: exit %d: %s", strings.Join(args, " "), res.ExitCode, strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout))), args))
	}
	return nil
}

// checkArg rejects values git would parse as options.
func checkArg(what, v string) error {
	if strings.HasPrefix(v, "-") {
		return fmt.Errorf("gitsync: %s %q must not start with '-'", what, v)
	}
	if strings.ContainsAny(v, "\x00\n\r") {
		return fmt.Errorf("gitsync: %s %q contains a control character", what, v)
	}
	return nil
}

func isHex(s string) bool {
	if len(s) < 4 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

// redactURL hides credentials embedded in a remote URL, so a token in
// https://user:token@host/repo never reaches logs or error messages. For
// http(s) the whole userinfo goes, since a bare token is often passed as the
// user name; for other schemes only the password does.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	_, hasPassword := u.User.Password()
	switch {
	case u.Scheme == "http" || u.Scheme == "https":
		u.User = url.User("xxxxx")
	case hasPassword:
		u.User = url.UserPassword(u.User.Username(), "xxxxx")
	default:
		return raw
	}
	return u.String()
}

// scrub removes credentials found in any URL-shaped argument from text that
// is about to be logged or returned, including git's own stderr.
func scrub(text string, args []string) string {
	for _, a := range args {
		u, err := url.Parse(a)
		if err != nil || u.User == nil {
			continue
		}
		if redacted := redactURL(a); redacted != a {
			text = strings.ReplaceAll(text, a, redacted)
		}
		if pw, ok := u.User.Password(); ok && pw != "" {
			text = strings.ReplaceAll(text, pw, "xxxxx")
		}
		if (u.Scheme == "http" || u.Scheme == "https") && u.User.Username() != "" {
			text = strings.ReplaceAll(text, u.User.Username()+"@", "xxxxx@")
		}
	}
	return text
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
