package resource

import "sort"

// ClaimKind names a namespace of physical things on a host that a resource
// can take responsibility for.
type ClaimKind string

const (
	// ClaimPath is a filesystem path.
	ClaimPath ClaimKind = "path"
	// ClaimUnit is a systemd unit's runtime state (not its file, which is a
	// path claim — writing a unit file and controlling the running unit are
	// separable responsibilities).
	ClaimUnit ClaimKind = "unit"
	// ClaimPackage is a package name in the host's package database.
	ClaimPackage ClaimKind = "package"
	// ClaimUser and ClaimGroup are account-database entries.
	ClaimUser  ClaimKind = "user"
	ClaimGroup ClaimKind = "group"
	// ClaimSysctl is a kernel parameter.
	ClaimSysctl ClaimKind = "sysctl"
)

// Claim is one thing on the host a resource asserts ownership of.
//
// Claims answer the question an operator actually asks in an incident:
// "who owns /etc/nginx/nginx.conf?" They also let systemcd refuse, before
// touching anything, to run a repository where two resources would fight
// over the same file.
type Claim struct {
	Kind ClaimKind
	Key  string
}

func (c Claim) String() string { return string(c.Kind) + ":" + c.Key }

// ParseClaim reverses Claim.String for values read back from state.
func ParseClaim(s string) Claim {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return Claim{Kind: ClaimKind(s[:i]), Key: s[i+1:]}
		}
	}
	return Claim{Key: s}
}

// Claimer is implemented by kinds that take ownership of something
// identifiable on the host.
//
// Exec deliberately does not implement it: it claims nothing, which is the
// honest description of an escape hatch and the reason it needs a guard.
type Claimer interface {
	Claims() []Claim
}

// ClaimsOf returns a resource's claims, sorted for stable output, or nil for
// kinds that claim nothing.
func ClaimsOf(r Resource) []Claim {
	c, ok := r.(Claimer)
	if !ok {
		return nil
	}
	claims := c.Claims()
	sort.Slice(claims, func(i, j int) bool { return claims[i].String() < claims[j].String() })
	return claims
}

// ClaimStrings renders claims for storage in the state file.
func ClaimStrings(claims []Claim) []string {
	out := make([]string, len(claims))
	for i, c := range claims {
		out[i] = c.String()
	}
	return out
}

func (f *File) Claims() []Claim { return []Claim{{Kind: ClaimPath, Key: f.spec.Path}} }

func (d *Directory) Claims() []Claim { return []Claim{{Kind: ClaimPath, Key: d.spec.Path}} }

// Claims for SystemdUnit cover the unit file only. The running unit belongs
// to a Service resource, so declaring both for one daemon is not a conflict.
func (u *SystemdUnit) Claims() []Claim { return []Claim{{Kind: ClaimPath, Key: u.file.spec.Path}} }

func (s *Service) Claims() []Claim { return []Claim{{Kind: ClaimUnit, Key: s.unit}} }

func (p *Package) Claims() []Claim {
	out := make([]Claim, 0, len(p.names))
	for _, n := range p.names {
		out = append(out, Claim{Kind: ClaimPackage, Key: n})
	}
	return out
}

func (u *User) Claims() []Claim { return []Claim{{Kind: ClaimUser, Key: u.user}} }

func (g *Group) Claims() []Claim { return []Claim{{Kind: ClaimGroup, Key: g.group}} }

func (s *Sysctl) Claims() []Claim {
	out := make([]Claim, 0, len(s.keys)+1)
	for _, k := range s.keys {
		out = append(out, Claim{Kind: ClaimSysctl, Key: k})
	}
	if s.persist() {
		out = append(out, Claim{Kind: ClaimPath, Key: s.dropInPath()})
	}
	return out
}

// Deleting a package pulls software off the machine, and systemcd keeps no
// copy of it; the same goes for accounts and for wiping a directory tree.
// File deletion is not on this list because Delete backs the contents up.

func (p *Package) DestructiveDelete() bool { return true }

func (u *User) DestructiveDelete() bool { return true }

func (g *Group) DestructiveDelete() bool { return true }

func (d *Directory) DestructiveDelete() bool { return d.spec.Recursive }
