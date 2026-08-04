package resource

import (
	"errors"
	"fmt"
	"strings"

	"github.com/vikki8/systemcd/internal/manifest"
)

func init() { Register("Package", buildPackage) }

// PackageSpec declares packages that must be installed or absent.
type PackageSpec struct {
	// Name declares a single package; Names declares several. A resource may
	// use either, and both are merged.
	Name  string   `yaml:"name,omitempty"`
	Names []string `yaml:"names,omitempty"`
	State string   `yaml:"state,omitempty"`
	// Version pins an exact version. Only valid with a single package.
	Version string `yaml:"version,omitempty"`
	// Manager forces a package manager instead of detecting one.
	Manager string `yaml:"manager,omitempty"`
	// Update refreshes the package index before installing (apt-get update
	// and friends). Off by default: it is slow and rarely needed per-run.
	Update bool `yaml:"update,omitempty"`
}

// Package is the Package resource kind.
type Package struct {
	name  string
	spec  PackageSpec
	names []string
}

var _ Resource = (*Package)(nil)
var _ Deletable = (*Package)(nil)

func buildPackage(doc *manifest.Document) (Resource, error) {
	p := &Package{name: doc.Metadata.Name}
	if err := doc.DecodeSpec(&p.spec); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Package) ID() ID { return ID{Kind: "Package", Name: p.name} }

func (p *Package) Validate() error {
	if p.spec.Name != "" {
		p.names = append(p.names, p.spec.Name)
	}
	p.names = append(p.names, p.spec.Names...)
	if len(p.names) == 0 {
		// Default to the resource name so the common single-package case can
		// omit the spec entirely.
		p.names = []string{p.name}
	}
	state, err := normalizePresence(p.spec.State)
	if err != nil {
		return err
	}
	p.spec.State = state
	if p.spec.Version != "" && len(p.names) > 1 {
		return errors.New("spec.version requires exactly one package")
	}
	if p.spec.Manager != "" {
		if _, ok := managers[p.spec.Manager]; !ok {
			return fmt.Errorf("spec.manager %q is not supported (known: %s)", p.spec.Manager, strings.Join(managerNames(), ", "))
		}
	}
	return nil
}

func (p *Package) Desired(c *Context) (State, error) {
	s := State{}
	for _, n := range p.names {
		want := p.spec.State
		if p.spec.Version != "" && p.spec.State == Present {
			want = p.spec.Version
		}
		s["pkg:"+n] = want
	}
	return s, nil
}

func (p *Package) Observe(c *Context) (State, error) {
	mgr, err := p.manager(c)
	if err != nil {
		return nil, err
	}
	s := State{"_manager": mgr.name}
	for _, n := range p.names {
		installed, version, err := mgr.query(c, n)
		if err != nil {
			return nil, err
		}
		switch {
		case !installed:
			s["pkg:"+n] = Absent
		case p.spec.Version != "":
			s["pkg:"+n] = version
		default:
			s["pkg:"+n] = Present
		}
		if version != "" {
			s["_version:"+n] = version
		}
	}
	return s, nil
}

func (p *Package) Apply(c *Context, d Diff) error {
	mgr, err := p.manager(c)
	if err != nil {
		return err
	}
	var install, remove []string
	for _, f := range d {
		name := strings.TrimPrefix(f.Field, "pkg:")
		if f.Want == Absent {
			remove = append(remove, name)
			continue
		}
		if p.spec.Version != "" {
			name = mgr.pin(name, p.spec.Version)
		}
		install = append(install, name)
	}

	if len(install) > 0 && p.spec.Update {
		if err := mgr.update(c); err != nil {
			return err
		}
	}
	if len(install) > 0 {
		if err := mgr.install(c, install); err != nil {
			return err
		}
		c.Log("installed %s", strings.Join(install, ", "))
	}
	if len(remove) > 0 {
		if err := mgr.remove(c, remove); err != nil {
			return err
		}
		c.Log("removed %s", strings.Join(remove, ", "))
	}
	return nil
}

// Delete removes every package the resource declared, used when a Package
// resource is pruned from the repository.
func (p *Package) Delete(c *Context) error {
	mgr, err := p.manager(c)
	if err != nil {
		return err
	}
	return mgr.remove(c, p.names)
}

func (p *Package) manager(c *Context) (*packageManager, error) {
	if p.spec.Manager != "" {
		return managers[p.spec.Manager], nil
	}
	return detectManager(c)
}

// packageManager adapts one distro package tool to a common interface.
type packageManager struct {
	name string
	// binary is what detection looks for on PATH.
	binary  string
	query   func(c *Context, pkg string) (installed bool, version string, err error)
	install func(c *Context, pkgs []string) error
	remove  func(c *Context, pkgs []string) error
	update  func(c *Context) error
	pin     func(pkg, version string) string
}

// detectionOrder is deliberate: a machine may carry more than one tool (a
// dnf box with apk in a container image), and the first match should be the
// distro's primary manager.
var detectionOrder = []string{"apt", "dnf", "yum", "zypper", "pacman", "apk"}

// managers is populated by init below.
var managers map[string]*packageManager

func managerNames() []string {
	out := make([]string, 0, len(managers))
	for _, n := range detectionOrder {
		if _, ok := managers[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

func detectManager(c *Context) (*packageManager, error) {
	for _, name := range detectionOrder {
		mgr := managers[name]
		if mgr == nil {
			continue
		}
		if _, err := c.Host.LookPath(mgr.binary); err == nil {
			return mgr, nil
		}
	}
	return nil, fmt.Errorf("no supported package manager found (looked for %s)", strings.Join(detectionOrder, ", "))
}

// dpkgQuery reports installation status on Debian-family systems.
func dpkgQuery(c *Context, pkg string) (bool, string, error) {
	res, err := c.Host.Run(c.Ctx, "dpkg-query", "-W", "-f=${db:Status-Status} ${Version}", pkg)
	if err != nil {
		return false, "", err
	}
	if !res.OK() {
		return false, "", nil
	}
	fields := strings.Fields(res.Stdout)
	if len(fields) == 0 || fields[0] != "installed" {
		return false, "", nil
	}
	version := ""
	if len(fields) > 1 {
		version = fields[1]
	}
	return true, version, nil
}

func rpmQuery(c *Context, pkg string) (bool, string, error) {
	res, err := c.Host.Run(c.Ctx, "rpm", "-q", "--queryformat", "%{VERSION}-%{RELEASE}", pkg)
	if err != nil {
		return false, "", err
	}
	if !res.OK() {
		return false, "", nil
	}
	return true, strings.TrimSpace(res.Stdout), nil
}

func runManager(c *Context, script string) error {
	res, err := c.Host.RunShell(c.Ctx, script)
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("%s: exit %d: %s", strings.Fields(script)[0], res.ExitCode, firstLine(res.Stderr, res.Stdout))
	}
	return nil
}

func quoteAll(pkgs []string) string {
	quoted := make([]string, len(pkgs))
	for i, p := range pkgs {
		quoted[i] = "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

func init() {
	managers = map[string]*packageManager{
		"apt": {
			name: "apt", binary: "apt-get", query: dpkgQuery,
			install: func(c *Context, p []string) error {
				return runManager(c, "DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "+quoteAll(p))
			},
			remove: func(c *Context, p []string) error {
				return runManager(c, "DEBIAN_FRONTEND=noninteractive apt-get remove -y "+quoteAll(p))
			},
			update: func(c *Context) error { return runManager(c, "apt-get update") },
			pin:    func(pkg, v string) string { return pkg + "=" + v },
		},
		"dnf": {
			name: "dnf", binary: "dnf", query: rpmQuery,
			install: func(c *Context, p []string) error { return runManager(c, "dnf install -y "+quoteAll(p)) },
			remove:  func(c *Context, p []string) error { return runManager(c, "dnf remove -y "+quoteAll(p)) },
			update:  func(c *Context) error { return runManager(c, "dnf makecache") },
			pin:     func(pkg, v string) string { return pkg + "-" + v },
		},
		"yum": {
			name: "yum", binary: "yum", query: rpmQuery,
			install: func(c *Context, p []string) error { return runManager(c, "yum install -y "+quoteAll(p)) },
			remove:  func(c *Context, p []string) error { return runManager(c, "yum remove -y "+quoteAll(p)) },
			update:  func(c *Context) error { return runManager(c, "yum makecache") },
			pin:     func(pkg, v string) string { return pkg + "-" + v },
		},
		"zypper": {
			name: "zypper", binary: "zypper", query: rpmQuery,
			install: func(c *Context, p []string) error {
				return runManager(c, "zypper --non-interactive install "+quoteAll(p))
			},
			remove: func(c *Context, p []string) error {
				return runManager(c, "zypper --non-interactive remove "+quoteAll(p))
			},
			update: func(c *Context) error { return runManager(c, "zypper --non-interactive refresh") },
			pin:    func(pkg, v string) string { return pkg + "-" + v },
		},
		"pacman": {
			name: "pacman", binary: "pacman",
			query: func(c *Context, pkg string) (bool, string, error) {
				res, err := c.Host.Run(c.Ctx, "pacman", "-Q", pkg)
				if err != nil {
					return false, "", err
				}
				if !res.OK() {
					return false, "", nil
				}
				fields := strings.Fields(res.Stdout)
				if len(fields) < 2 {
					return true, "", nil
				}
				return true, fields[1], nil
			},
			install: func(c *Context, p []string) error {
				return runManager(c, "pacman -S --noconfirm --needed "+quoteAll(p))
			},
			remove: func(c *Context, p []string) error { return runManager(c, "pacman -R --noconfirm "+quoteAll(p)) },
			update: func(c *Context) error { return runManager(c, "pacman -Sy --noconfirm") },
			pin:    func(pkg, v string) string { return pkg + "=" + v },
		},
		"apk": {
			name: "apk", binary: "apk",
			query: func(c *Context, pkg string) (bool, string, error) {
				// -v prints "nginx-1.24.0-r7", which is the only way to learn
				// the installed version without parsing `apk list`.
				res, err := c.Host.Run(c.Ctx, "apk", "info", "-e", "-v", pkg)
				if err != nil {
					return false, "", err
				}
				out := strings.TrimSpace(res.Stdout)
				if !res.OK() || out == "" {
					return false, "", nil
				}
				return true, strings.TrimPrefix(firstLine(out), pkg+"-"), nil
			},
			install: func(c *Context, p []string) error { return runManager(c, "apk add --no-cache "+quoteAll(p)) },
			remove:  func(c *Context, p []string) error { return runManager(c, "apk del "+quoteAll(p)) },
			update:  func(c *Context) error { return runManager(c, "apk update") },
			pin:     func(pkg, v string) string { return pkg + "=" + v },
		},
	}
}
