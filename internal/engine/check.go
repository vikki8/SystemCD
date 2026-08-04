package engine

import (
	"github.com/vikki8/systemcd/internal/manifest"
)

// Check validates a repository without contacting the host: every document
// must build into a resource whose spec passes its own validation, and the
// dependency graph must be acyclic.
//
// Targeting is deliberately ignored here so `systemcd validate` catches a
// broken manifest for web servers even when run on a database host.
func Check(repo *manifest.Repository) error {
	_, err := build(repo.Documents)
	return err
}
