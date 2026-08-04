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
	args = append(args, src.URL, src.Dir)
	s.log("cloning %s (%s) into %s", src.URL, ref, src.Dir)

	if err := s.git(ctx, "", args...); err != nil {
		// A commit SHA or tag cannot be passed to --branch; fall back to a
		// plain clone followed by a checkout.
		s.log("branch clone failed, retrying with a full clone")
		if err := s.git(ctx, "", "clone", src.URL, src.Dir); err != nil {
			return err
		}
		return s.git(ctx, src.Dir, "checkout", "--detach", ref)
	}
	return nil
}

func (s *Syncer) fetch(ctx context.Context, src Source, ref string) error {
	s.log("fetching %s from %s", ref, src.URL)
	if err := s.git(ctx, src.Dir, "remote", "set-url", "origin", src.URL); err != nil {
		return err
	}
	if err := s.git(ctx, src.Dir, "fetch", "--prune", "origin", ref); err != nil {
		// The ref may be a tag or a raw SHA that a refspec fetch misses.
		if err := s.git(ctx, src.Dir, "fetch", "--prune", "--tags", "origin"); err != nil {
			return err
		}
	}
	// Reset hard: the checkout is a cache of the remote, never a place to
	// edit. Local changes there are drift, not work to preserve.
	if err := s.git(ctx, src.Dir, "checkout", "--detach", "FETCH_HEAD"); err != nil {
		if err := s.git(ctx, src.Dir, "checkout", "--detach", ref); err != nil {
			return err
		}
	}
	return s.git(ctx, src.Dir, "reset", "--hard")
}

// Checkout moves an existing checkout to a specific revision, used by
// `rollback` to return to a known-good commit.
func (s *Syncer) Checkout(ctx context.Context, dir, revision string) error {
	return s.git(ctx, dir, "checkout", "--detach", revision)
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
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	if !res.OK() {
		return fmt.Errorf("git %s: exit %d: %s", strings.Join(args, " "), res.ExitCode, strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout)))
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
