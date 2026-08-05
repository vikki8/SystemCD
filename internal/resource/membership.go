package resource

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Membership modes for supplementary group lists.
const (
	// MembershipAdditive asserts "this user must be in these groups" and says
	// nothing about any other group. Memberships added by a package
	// post-install or by another tool are left alone.
	MembershipAdditive = "additive"
	// MembershipExact asserts "these groups and no others". Anything else the
	// account belongs to is drift and gets removed. Compliance environments
	// want this; most other places do not.
	MembershipExact = "exact"
)

// GroupMembership expresses what a manifest actually means by `groups:`.
//
// The shorthand is a plain list:
//
//	groups: [docker, adm]
//
// which is additive — the common intent, "I require docker membership", not
// "I own this account's entire group list". The explicit form opts into the
// stronger assertion:
//
//	groups:
//	  ensure: [docker, adm]
//	  mode: exact
//
// Making the shorthand additive keeps the dangerous reading from being the
// accidental one: nobody types `groups: [docker]` intending to strip an
// account out of `sudo`.
type GroupMembership struct {
	Ensure []string `yaml:"ensure,omitempty"`
	Mode   string   `yaml:"mode,omitempty"`
}

// UnmarshalYAML accepts either the list shorthand or the explicit mapping.
func (g *GroupMembership) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.SequenceNode:
		var list []string
		if err := node.Decode(&list); err != nil {
			return err
		}
		g.Ensure, g.Mode = list, MembershipAdditive
		return nil
	case yaml.MappingNode:
		// A named type avoids recursing into this method.
		type raw GroupMembership
		var out raw
		if err := node.Decode(&out); err != nil {
			return err
		}
		*g = GroupMembership(out)
		return nil
	default:
		return fmt.Errorf("groups must be a list of group names or a mapping with `ensure` and `mode`")
	}
}

// Empty reports whether any membership was declared.
func (g GroupMembership) Empty() bool { return len(g.Ensure) == 0 }

// normalize validates the mode and applies the default.
func (g *GroupMembership) normalize() error {
	switch g.Mode {
	case "":
		g.Mode = MembershipAdditive
	case MembershipAdditive, MembershipExact:
	default:
		return fmt.Errorf("spec.groups.mode must be %q or %q, got %q", MembershipAdditive, MembershipExact, g.Mode)
	}
	if g.Mode == MembershipExact && len(g.Ensure) == 0 {
		return fmt.Errorf("spec.groups.mode: exact with an empty list would remove every supplementary group; list them explicitly or use `mode: additive`")
	}
	return nil
}
