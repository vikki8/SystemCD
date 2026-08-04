package resource

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/vikki8/systemcd/internal/manifest"
)

func init() {
	Register("User", buildUser)
	Register("Group", buildGroup)
}

// UserSpec declares a system account.
type UserSpec struct {
	User  string `yaml:"user,omitempty"`
	UID   *int   `yaml:"uid,omitempty"`
	Home  string `yaml:"home,omitempty"`
	Shell string `yaml:"shell,omitempty"`
	// Groups declares supplementary group membership. See GroupMembership for
	// the two forms and what each one asserts.
	Groups GroupMembership `yaml:"groups,omitempty"`
	// Group is the primary group.
	Group string `yaml:"group,omitempty"`
	// System creates a system account (no aging, low uid).
	System bool `yaml:"system,omitempty"`
	// CreateHome creates the home directory. Defaults to false for system
	// accounts and true otherwise.
	CreateHome *bool  `yaml:"createHome,omitempty"`
	State      string `yaml:"state,omitempty"`
}

// User is the User resource kind.
type User struct {
	name string
	spec UserSpec
	user string
}

var _ Resource = (*User)(nil)
var _ Deletable = (*User)(nil)

func buildUser(doc *manifest.Document) (Resource, error) {
	u := &User{name: doc.Metadata.Name}
	if err := doc.DecodeSpec(&u.spec); err != nil {
		return nil, err
	}
	return u, nil
}

func (u *User) ID() ID { return ID{Kind: "User", Name: u.name} }

func (u *User) Validate() error {
	u.user = u.spec.User
	if u.user == "" {
		u.user = u.name
	}
	state, err := normalizePresence(u.spec.State)
	if err != nil {
		return err
	}
	u.spec.State = state
	if u.spec.Shell != "" && !strings.HasPrefix(u.spec.Shell, "/") {
		return fmt.Errorf("spec.shell %q must be an absolute path", u.spec.Shell)
	}
	return u.spec.Groups.normalize()
}

func (u *User) Desired(c *Context) (State, error) {
	s := State{"state": u.spec.State, "_user": u.user}
	if u.spec.State == Absent {
		return s, nil
	}
	if u.spec.UID != nil {
		s["uid"] = strconv.Itoa(*u.spec.UID)
	}
	if u.spec.Home != "" {
		s["home"] = u.spec.Home
	}
	if u.spec.Shell != "" {
		s["shell"] = u.spec.Shell
	}
	if !u.spec.Groups.Empty() {
		s["groups"] = normalizeList(u.spec.Groups.Ensure)
		s["_groupsMode"] = u.spec.Groups.Mode
	}
	return s, nil
}

func (u *User) Observe(c *Context) (State, error) {
	entry, found, err := getent(c, "passwd", u.user)
	if err != nil {
		return nil, err
	}
	if !found {
		return State{"state": Absent, "_user": u.user}, nil
	}
	// passwd format: name:passwd:uid:gid:gecos:home:shell
	fields := strings.Split(entry, ":")
	s := State{"state": Present, "_user": u.user}
	if len(fields) >= 7 {
		s["uid"] = fields[2]
		s["home"] = fields[5]
		s["shell"] = fields[6]
		s["_gid"] = fields[3]
	}

	if !u.spec.Groups.Empty() {
		groups, err := u.observeGroups(c, s["_gid"])
		if err != nil {
			return nil, err
		}
		s["groups"] = groups
		s["_groupsMode"] = u.spec.Groups.Mode
	}
	return s, nil
}

// observeGroups renders the account's supplementary membership in whatever
// terms the manifest asked the question.
//
// In additive mode only the named groups are reported, so an unrelated group
// the account picked up elsewhere is not drift. In exact mode the full list
// is reported (minus the primary group, which `id -nG` includes but
// `usermod --groups` does not manage), so extra membership *is* drift.
func (u *User) observeGroups(c *Context, primaryGID string) (string, error) {
	res, err := c.Host.Run(c.Ctx, "id", "-nG", u.user)
	if err != nil {
		return "", err
	}
	if !res.OK() {
		return "", nil
	}
	have := strings.Fields(res.Stdout)

	if u.spec.Groups.Mode == MembershipAdditive {
		present := make([]string, 0, len(u.spec.Groups.Ensure))
		set := map[string]bool{}
		for _, g := range have {
			set[g] = true
		}
		for _, g := range u.spec.Groups.Ensure {
			if set[g] {
				present = append(present, g)
			}
		}
		return normalizeList(present), nil
	}

	primary := ""
	if primaryGID != "" {
		if entry, found, err := getent(c, "group", primaryGID); err == nil && found {
			if fields := strings.Split(entry, ":"); len(fields) > 0 {
				primary = fields[0]
			}
		}
	}
	supplementary := make([]string, 0, len(have))
	for _, g := range have {
		if g != primary {
			supplementary = append(supplementary, g)
		}
	}
	return normalizeList(supplementary), nil
}

func (u *User) Apply(c *Context, d Diff) error {
	if u.spec.State == Absent {
		return u.Delete(c)
	}

	if d.Has("state") {
		args := []string{}
		if u.spec.System {
			args = append(args, "--system")
		}
		if u.spec.UID != nil {
			args = append(args, "--uid", strconv.Itoa(*u.spec.UID))
		}
		if u.spec.Group != "" {
			args = append(args, "--gid", u.spec.Group)
		}
		if u.spec.Home != "" {
			args = append(args, "--home-dir", u.spec.Home)
		}
		if u.spec.Shell != "" {
			args = append(args, "--shell", u.spec.Shell)
		}
		if !u.spec.Groups.Empty() {
			args = append(args, "--groups", strings.Join(u.spec.Groups.Ensure, ","))
		}
		if u.createHome() {
			args = append(args, "--create-home")
		} else {
			args = append(args, "--no-create-home")
		}
		args = append(args, u.user)
		if err := run(c, "useradd", args...); err != nil {
			return err
		}
		c.Log("created user %s", u.user)
		return nil
	}

	var mod []string
	if d.Has("uid") && u.spec.UID != nil {
		mod = append(mod, "--uid", strconv.Itoa(*u.spec.UID))
	}
	if d.Has("home") && u.spec.Home != "" {
		mod = append(mod, "--home", u.spec.Home)
	}
	if d.Has("shell") && u.spec.Shell != "" {
		mod = append(mod, "--shell", u.spec.Shell)
	}
	if d.Has("groups") && !u.spec.Groups.Empty() {
		if u.spec.Groups.Mode == MembershipAdditive {
			// --append keeps memberships the manifest does not mention.
			mod = append(mod, "--append")
		}
		mod = append(mod, "--groups", strings.Join(u.spec.Groups.Ensure, ","))
	}
	if len(mod) == 0 {
		return nil
	}
	mod = append(mod, u.user)
	if err := run(c, "usermod", mod...); err != nil {
		return err
	}
	c.Log("updated user %s", u.user)
	return nil
}

func (u *User) Delete(c *Context) error {
	_, found, err := getent(c, "passwd", u.user)
	if err != nil || !found {
		return err
	}
	if err := run(c, "userdel", u.user); err != nil {
		return err
	}
	c.Log("deleted user %s", u.user)
	return nil
}

func (u *User) createHome() bool {
	if u.spec.CreateHome != nil {
		return *u.spec.CreateHome
	}
	return !u.spec.System
}

// GroupSpec declares a system group.
type GroupSpec struct {
	Group  string `yaml:"group,omitempty"`
	GID    *int   `yaml:"gid,omitempty"`
	System bool   `yaml:"system,omitempty"`
	State  string `yaml:"state,omitempty"`
}

// Group is the Group resource kind.
type Group struct {
	name  string
	spec  GroupSpec
	group string
}

var _ Resource = (*Group)(nil)
var _ Deletable = (*Group)(nil)

func buildGroup(doc *manifest.Document) (Resource, error) {
	g := &Group{name: doc.Metadata.Name}
	if err := doc.DecodeSpec(&g.spec); err != nil {
		return nil, err
	}
	return g, nil
}

func (g *Group) ID() ID { return ID{Kind: "Group", Name: g.name} }

func (g *Group) Validate() error {
	g.group = g.spec.Group
	if g.group == "" {
		g.group = g.name
	}
	state, err := normalizePresence(g.spec.State)
	if err != nil {
		return err
	}
	g.spec.State = state
	return nil
}

func (g *Group) Desired(c *Context) (State, error) {
	s := State{"state": g.spec.State, "_group": g.group}
	if g.spec.State == Present && g.spec.GID != nil {
		s["gid"] = strconv.Itoa(*g.spec.GID)
	}
	return s, nil
}

func (g *Group) Observe(c *Context) (State, error) {
	entry, found, err := getent(c, "group", g.group)
	if err != nil {
		return nil, err
	}
	if !found {
		return State{"state": Absent, "_group": g.group}, nil
	}
	s := State{"state": Present, "_group": g.group}
	if fields := strings.Split(entry, ":"); len(fields) >= 3 {
		s["gid"] = fields[2]
	}
	return s, nil
}

func (g *Group) Apply(c *Context, d Diff) error {
	if g.spec.State == Absent {
		return g.Delete(c)
	}
	if d.Has("state") {
		args := []string{}
		if g.spec.System {
			args = append(args, "--system")
		}
		if g.spec.GID != nil {
			args = append(args, "--gid", strconv.Itoa(*g.spec.GID))
		}
		args = append(args, g.group)
		if err := run(c, "groupadd", args...); err != nil {
			return err
		}
		c.Log("created group %s", g.group)
		return nil
	}
	if d.Has("gid") && g.spec.GID != nil {
		if err := run(c, "groupmod", "--gid", strconv.Itoa(*g.spec.GID), g.group); err != nil {
			return err
		}
	}
	return nil
}

func (g *Group) Delete(c *Context) error {
	_, found, err := getent(c, "group", g.group)
	if err != nil || !found {
		return err
	}
	return run(c, "groupdel", g.group)
}

// getent looks up one entry in a name-service database. A non-zero exit means
// "not found", which is not an error.
func getent(c *Context, database, key string) (string, bool, error) {
	res, err := c.Host.Run(c.Ctx, "getent", database, key)
	if err != nil {
		return "", false, err
	}
	if !res.OK() {
		return "", false, nil
	}
	line := firstLine(res.Stdout)
	if line == "no output" || line == "" {
		return "", false, nil
	}
	return line, true, nil
}

// run executes a command and turns a non-zero exit into an error.
func run(c *Context, name string, args ...string) error {
	res, err := c.Host.Run(c.Ctx, name, args...)
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("%s %s: exit %d: %s", name, strings.Join(args, " "), res.ExitCode, firstLine(res.Stderr, res.Stdout))
	}
	return nil
}

// normalizeList renders a set as a stable, comparable string.
func normalizeList(items []string) string {
	cp := append([]string(nil), items...)
	sort.Strings(cp)
	return strings.Join(cp, ",")
}
