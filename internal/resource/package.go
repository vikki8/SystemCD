package resource

import (
	"errors"
	"fmt"
	"regexp"
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

// packageNamePattern is the union of what apt, rpm, pacman and apk accept as
// a package name, plus dpkg's ":arch" qualifier. Anything else either never
// converges, because the query cannot find what the install was asked for
// ("nginx=1.2", "@group", "nginx*"), or is read as an option by the package
// tool ("-oAPT::..."), which quoting for the shell does not prevent.
var packageNamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._+:@-]*$`)

func (p *Package) Validate() error {
	candidates := append([]string(nil), p.spec.Names...)
	if p.spec.Name != "" {
		candidates = append([]string{p.spec.Name}, candidates...)
	}
	if len(candidates) == 0 {
		// Default to the resource name so the common single-package case can
		// omit the spec entirely.
		candidates = []string{p.name}
	}
	p.names = nil
	seen := map[string]bool{}
	for _, n := range candidates {
		if !packageNamePattern.MatchString(n) {
			return fmt.Errorf("package name %q is not valid: it must start with a letter, digit or underscore "+
				"and contain only letters, digits and . _ + : @ - (a version belongs in spec.version)", n)
		}
		// A repeated name would otherwise claim the same package twice and
		// be reported as conflicting with itself.
		if !seen[n] {
			seen[n] = true
			p.names = append(p.names, n)
		}
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
		installed, version, err := mgr.query(c, n, p.spec.Version)
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
	pinned := p.spec.Version != ""
	var install, installNames, remove []string
	for _, f := range d {
		name := strings.TrimPrefix(f.Field, "pkg:")
		if f.Want == Absent {
			remove = append(remove, name)
			continue
		}
		installNames = append(installNames, name)
		if pinned {
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
		if err := mgr.install(c, install, pinned); err != nil {
			return err
		}
		if pinned && mgr.downgrade != nil {
			if err := p.downgradeIfNeeded(c, mgr, installNames); err != nil {
				return err
			}
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

// downgradeIfNeeded finishes a pinned install on managers whose install
// command will not move a package to an older version.
func (p *Package) downgradeIfNeeded(c *Context, mgr *packageManager, names []string) error {
	for _, n := range names {
		installed, version, err := mgr.query(c, n, p.spec.Version)
		if err != nil {
			return err
		}
		if !installed || version == p.spec.Version {
			continue
		}
		if err := mgr.downgrade(c, []string{mgr.pin(n, p.spec.Version)}); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes every package the resource declared, used when a Package
// resource is pruned from the repository. Packages that are already gone are
// skipped: pacman, zypper and apt (for a name no longer in the index) all fail
// when asked to remove something that is not installed, which would leave
// the prune failing on every run.
func (p *Package) Delete(c *Context) error {
	mgr, err := p.manager(c)
	if err != nil {
		return err
	}
	var installed []string
	for _, n := range p.names {
		ok, _, err := mgr.query(c, n, "")
		if err != nil {
			return err
		}
		if ok {
			installed = append(installed, n)
		}
	}
	if len(installed) == 0 {
		return nil
	}
	if err := mgr.remove(c, installed); err != nil {
		return err
	}
	c.Log("removed %s", strings.Join(installed, ", "))
	return nil
}

// manager resolves the package manager and refuses up front if the manifest
// asked for something this manager cannot deliver.
func (p *Package) manager(c *Context) (*packageManager, error) {
	var mgr *packageManager
	if p.spec.Manager != "" {
		mgr = managers[p.spec.Manager]
	} else {
		detected, err := detectManager(c)
		if err != nil {
			return nil, err
		}
		mgr = detected
	}
	if p.spec.Version != "" && !mgr.pinnable {
		return nil, fmt.Errorf(
			"spec.version is not supported with the %s package manager: %s. "+
				"Declare the package without a version, or pin it in the repository configuration for %s instead",
			mgr.name, mgr.pinNote, mgr.name)
	}
	return mgr, nil
}

// packageManager adapts one distro package tool to a common interface.
type packageManager struct {
	name string
	// binary is what detection looks for on PATH.
	binary string
	// query reports whether pkg is installed and its version. want is the
	// pinned version, if any: the installed version is rendered in the same
	// form (with or without epoch, release, arch) so a pin round-trips, and
	// when several instances are installed side by side (multilib, multiarch,
	// install-only kernels) the one matching want is preferred.
	query func(c *Context, pkg, want string) (installed bool, version string, err error)
	// install is told whether the packages carry a version pin, so it can
	// permit the downgrade an exact pin may need.
	install func(c *Context, pkgs []string, pinned bool) error
	remove  func(c *Context, pkgs []string) error
	update  func(c *Context) error
	pin     func(pkg, version string) string
	// downgrade, when set, runs after a pinned install that left the package
	// at another version, for managers whose install never goes backwards.
	downgrade func(c *Context, pkgs []string) error
	// pinnable records whether systemcd can actually *guarantee* convergence
	// to an exact version with this manager: install a specific version,
	// downgrade to it if a newer one is present, and read back a version
	// string in the same form the manifest wrote.
	//
	// Where it cannot, `spec.version` is rejected rather than silently
	// accepted. A field that sometimes converges and sometimes reports
	// permanent drift is worse than no field at all.
	pinnable bool
	// pinNote explains the refusal.
	pinNote string
}

// Capabilities describes what a host's package manager can guarantee, so the
// answer is knowable before a manifest is written.
type Capabilities struct {
	Manager        string
	VersionPinning bool
	Note           string
}

// DetectCapabilities reports the package-management capabilities of a host.
func DetectCapabilities(c *Context) (Capabilities, error) {
	mgr, err := detectManager(c)
	if err != nil {
		return Capabilities{}, err
	}
	return Capabilities{Manager: mgr.name, VersionPinning: mgr.pinnable, Note: mgr.pinNote}, nil
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
//
// The format ends in a newline because a name without an architecture
// qualifier matches every installed architecture of a Multi-Arch package, and
// without one the lines run together ("installed 2.39-0ubuntu8installed
// 2.39-0ubuntu8").
func dpkgQuery(c *Context, pkg, want string) (bool, string, error) {
	res, err := c.Host.Run(c.Ctx, "dpkg-query", "-W", "-f=${db:Status-Status} ${Version}\n", pkg)
	if err != nil {
		return false, "", err
	}
	if !res.OK() {
		return false, "", nil
	}
	installed := false
	var versions []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "installed", "triggers-awaited", "triggers-pending":
			// The trigger states are a configured package whose triggers
			// have not run yet; reinstalling would not change them.
		default:
			// not-installed, config-files, half-installed, unpacked and
			// half-configured are not usable, so an install is the fix.
			continue
		}
		installed = true
		if len(fields) > 1 {
			versions = append(versions, fields[1])
		}
	}
	return installed, pickVersion(versions, want), nil
}

func rpmQuery(c *Context, pkg, want string) (bool, string, error) {
	res, err := c.Host.Run(c.Ctx, "rpm", "-q", "--queryformat", "%{EPOCH} %{VERSION} %{RELEASE} %{ARCH}\n", pkg)
	if err != nil {
		return false, "", err
	}
	if !res.OK() {
		return false, "", nil
	}
	var versions []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		fields := strings.Fields(line)
		switch len(fields) {
		case 0:
			continue
		case 4:
			versions = append(versions, rpmVersion(fields[0], fields[1], fields[2], fields[3], want))
		default:
			versions = append(versions, strings.TrimSpace(line))
		}
	}
	return true, pickVersion(versions, want), nil
}

// rpmVersion renders an installed package's EVR in the form the manifest
// wrote its pin, so "1.20.1", "1.20.1-14.el9", "1:1.20.1-14.el9" and
// "1.20.1-14.el9.x86_64" each read back exactly as written once installed.
// Without a pin it is the conventional version-release.
func rpmVersion(epoch, version, release, arch, want string) string {
	if want == "" {
		return version + "-" + release
	}
	body := want
	_, afterEpoch, hasEpoch := strings.Cut(want, ":")
	if hasEpoch {
		body = afterEpoch
	}
	out := version
	if strings.Contains(body, "-") {
		out += "-" + release
		if strings.HasSuffix(body, "."+arch) {
			out += "." + arch
		}
	}
	if hasEpoch {
		if epoch == "(none)" {
			epoch = "0"
		}
		out = epoch + ":" + out
	}
	return out
}

// pickVersion chooses which installed instance to report: the pinned version
// if it is among them, otherwise the first.
func pickVersion(versions []string, want string) string {
	for _, v := range versions {
		if want != "" && v == want {
			return v
		}
	}
	if len(versions) == 0 {
		return ""
	}
	return versions[0]
}

func runManager(c *Context, script string) error {
	res, err := c.Host.RunShell(c.Ctx, script)
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("%s: exit %d: %s", commandName(script), res.ExitCode, firstLine(res.Stderr, res.Stdout))
	}
	return nil
}

// commandName is the program a script runs, skipping leading environment
// assignments so an apt failure is reported as apt-get, not DEBIAN_FRONTEND.
func commandName(script string) string {
	fields := strings.Fields(script)
	for _, f := range fields {
		if !strings.Contains(f, "=") {
			return f
		}
	}
	return script
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
			install: func(c *Context, p []string, pinned bool) error {
				// systemcd manages config files itself, so keep the copy on
				// disk: without these, a modified conffile makes dpkg prompt,
				// and with no terminal the install dies with "end of file on
				// stdin at conffile prompt".
				cmd := "DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends " +
					"-o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold"
				if pinned {
					// -y alone refuses downgrades, and a pin may be older
					// than what is installed.
					cmd += " --allow-downgrades"
				}
				return runManager(c, cmd+" "+quoteAll(p))
			},
			remove: func(c *Context, p []string) error {
				return runManager(c, "DEBIAN_FRONTEND=noninteractive apt-get remove -y "+quoteAll(p))
			},
			update:   func(c *Context) error { return runManager(c, "apt-get update") },
			pin:      func(pkg, v string) string { return pkg + "=" + v },
			pinnable: true,
		},
		"dnf": {
			name: "dnf", binary: "dnf", query: rpmQuery,
			install:  func(c *Context, p []string, _ bool) error { return runManager(c, "dnf install -y "+quoteAll(p)) },
			remove:   func(c *Context, p []string) error { return runManager(c, "dnf remove -y "+quoteAll(p)) },
			update:   func(c *Context) error { return runManager(c, "dnf makecache") },
			pin:      func(pkg, v string) string { return pkg + "-" + v },
			pinnable: true,
		},
		"yum": {
			name: "yum", binary: "yum", query: rpmQuery,
			install:  func(c *Context, p []string, _ bool) error { return runManager(c, "yum install -y "+quoteAll(p)) },
			remove:   func(c *Context, p []string) error { return runManager(c, "yum remove -y "+quoteAll(p)) },
			update:   func(c *Context) error { return runManager(c, "yum makecache") },
			pin:      func(pkg, v string) string { return pkg + "-" + v },
			pinnable: true,
			// yum 3 answers an install of an older version with "Nothing to
			// do" and exit 0; only `yum downgrade` goes backwards.
			downgrade: func(c *Context, p []string) error { return runManager(c, "yum downgrade -y "+quoteAll(p)) },
		},
		"zypper": {
			name: "zypper", binary: "zypper", query: rpmQuery,
			install: func(c *Context, p []string, pinned bool) error {
				cmd := "zypper --non-interactive install"
				if pinned {
					// Without it zypper will not replace a newer version
					// with the older one a pin names.
					cmd += " --oldpackage"
				}
				return runManager(c, cmd+" "+quoteAll(p))
			},
			remove: func(c *Context, p []string) error {
				return runManager(c, "zypper --non-interactive remove "+quoteAll(p))
			},
			update:   func(c *Context) error { return runManager(c, "zypper --non-interactive refresh") },
			pin:      func(pkg, v string) string { return pkg + "-" + v },
			pinnable: true,
		},
		"pacman": {
			name: "pacman", binary: "pacman",
			query: func(c *Context, pkg, _ string) (bool, string, error) {
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
			install: func(c *Context, p []string, _ bool) error {
				return runManager(c, "pacman -S --noconfirm --needed "+quoteAll(p))
			},
			remove: func(c *Context, p []string) error { return runManager(c, "pacman -R --noconfirm "+quoteAll(p)) },
			update: func(c *Context) error { return runManager(c, "pacman -Sy --noconfirm") },
			pin:    func(pkg, v string) string { return pkg + "=" + v },
			pinNote: "pacman installs whatever version the repository currently carries and has no supported " +
				"way to request an older one, so a pinned version could never converge",
		},
		"apk": {
			name: "apk", binary: "apk",
			query: func(c *Context, pkg, _ string) (bool, string, error) {
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
			install: func(c *Context, p []string, _ bool) error { return runManager(c, "apk add --no-cache "+quoteAll(p)) },
			remove:  func(c *Context, p []string) error { return runManager(c, "apk del "+quoteAll(p)) },
			update:  func(c *Context) error { return runManager(c, "apk update") },
			pin:     func(pkg, v string) string { return pkg + "=" + v },
			pinNote: "apk pins depend on the version still being present in the configured repository, the " +
				"installed version string does not round-trip with what a manifest writes, and downgrades " +
				"need --allow-untrusted; systemcd cannot promise convergence",
		},
	}
}
