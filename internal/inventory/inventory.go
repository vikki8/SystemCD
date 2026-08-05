// Package inventory discovers what is already configured on a host that
// systemcd does not manage.
//
// This is the missing first step for an existing fleet. Every configuration
// tool assumes you know what should be on the machine; nobody who inherited a
// five-year-old server does. Inventory answers the prior question — "what is
// actually here, and which of it did somebody change by hand?" — using the
// package manager's own record of what it shipped as the reference point.
package inventory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/vikki8/systemcd/internal/host"
	"github.com/vikki8/systemcd/internal/resource"
	"github.com/vikki8/systemcd/internal/state"
)

// Category groups findings by why they are interesting.
type Category string

const (
	// CategoryModifiedConfig is a packaged config file whose contents no
	// longer match what the package shipped. This is the highest-value
	// finding on an inherited machine: it is, quite literally, the list of
	// things somebody changed by hand and did not write down.
	CategoryModifiedConfig Category = "modified-config"
	// CategoryUnpackagedConfig is a file under /etc that no package owns —
	// created locally, by an installer, or by an older automation tool.
	CategoryUnpackagedConfig Category = "unpackaged-config"
	// CategoryLocalUnit is a systemd unit installed outside the package
	// manager, which almost always means a first-party service.
	CategoryLocalUnit Category = "local-unit"
	// CategoryEnabledService is a unit enabled at boot.
	CategoryEnabledService Category = "enabled-service"
	// CategoryManualPackage is a package explicitly installed rather than
	// pulled in as a dependency.
	CategoryManualPackage Category = "manual-package"
	// CategoryLocalUser is a non-system account.
	CategoryLocalUser Category = "local-user"
	// CategorySysctlDropIn is a kernel-parameter drop-in.
	CategorySysctlDropIn Category = "sysctl-drop-in"
)

// Item is one discovered piece of host configuration.
type Item struct {
	Category Category `json:"category"`
	// Claim is the ownership key this would occupy, so an item can be matched
	// against what systemcd already owns.
	Claim string `json:"claim"`
	// Detail explains the finding in the operator's terms.
	Detail string `json:"detail,omitempty"`
	// Package names the owning package, where one is known.
	Package string `json:"package,omitempty"`
	// ManagedBy names the systemcd resource that already owns this, if any.
	ManagedBy string `json:"managedBy,omitempty"`
}

// Managed reports whether systemcd already owns the item.
func (i Item) Managed() bool { return i.ManagedBy != "" }

// Report is the outcome of a scan.
type Report struct {
	Hostname string `json:"hostname"`
	Items    []Item `json:"items"`
	// Skipped records scans that could not run, and why. A scan that silently
	// finds nothing because a tool was missing is worse than no scan.
	Skipped []string `json:"skipped,omitempty"`
}

// Counts summarizes a report by category.
type Counts struct {
	Total      int
	Managed    int
	Unmanaged  int
	ByCategory map[Category]int
}

// Counts tallies the report.
func (r *Report) Counts() Counts {
	c := Counts{ByCategory: map[Category]int{}}
	for _, item := range r.Items {
		c.Total++
		if item.Managed() {
			c.Managed++
		} else {
			c.Unmanaged++
		}
		c.ByCategory[item.Category]++
	}
	return c
}

// Unmanaged returns only the items systemcd does not already own — the actual
// remaining surface area of an onboarding.
func (r *Report) Unmanaged() []Item {
	var out []Item
	for _, item := range r.Items {
		if !item.Managed() {
			out = append(out, item)
		}
	}
	return out
}

// Options configures a scan.
type Options struct {
	Host host.Host
	// Snapshot lets the scan mark items systemcd already owns.
	Snapshot *state.Snapshot
	// Categories restricts the scan. Empty means everything.
	Categories []Category
	// Paths restricts filesystem scans to these roots. Defaults to /etc.
	Paths []string
	// Limit caps the number of items per category, so a scan of a machine
	// with 40,000 files in /etc stays usable.
	Limit int
}

// DefaultLimit bounds each category of a scan.
const DefaultLimit = 500

// Scan inspects the host and reports unmanaged configuration.
func Scan(ctx context.Context, opts Options) (*Report, error) {
	if opts.Host == nil {
		return nil, errors.New("inventory: no host provided")
	}
	if opts.Limit <= 0 {
		opts.Limit = DefaultLimit
	}
	if len(opts.Paths) == 0 {
		opts.Paths = []string{"/etc"}
	}

	name, _ := opts.Host.Hostname()
	rep := &Report{Hostname: name}

	s := &scanner{ctx: ctx, opts: opts, report: rep}
	for _, scan := range []struct {
		category Category
		run      func(*scanner) error
	}{
		{CategoryModifiedConfig, (*scanner).modifiedConfigs},
		{CategoryUnpackagedConfig, (*scanner).unpackagedConfigs},
		{CategoryLocalUnit, (*scanner).localUnits},
		{CategoryEnabledService, (*scanner).enabledServices},
		{CategoryManualPackage, (*scanner).manualPackages},
		{CategoryLocalUser, (*scanner).localUsers},
		{CategorySysctlDropIn, (*scanner).sysctlDropIns},
	} {
		if !opts.wants(scan.category) {
			continue
		}
		s.current = scan.category
		if err := scan.run(s); err != nil {
			rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s: %v", scan.category, err))
		}
	}

	sort.SliceStable(rep.Items, func(i, j int) bool {
		if rep.Items[i].Category != rep.Items[j].Category {
			return rep.Items[i].Category < rep.Items[j].Category
		}
		return rep.Items[i].Claim < rep.Items[j].Claim
	})
	return rep, nil
}

func (o Options) wants(c Category) bool {
	if len(o.Categories) == 0 {
		return true
	}
	for _, want := range o.Categories {
		if want == c {
			return true
		}
	}
	return false
}

type scanner struct {
	ctx     context.Context
	opts    Options
	report  *Report
	current Category
	counts  map[Category]int
}

// add records a finding, attributing it to a systemcd resource when one
// already claims it.
func (s *scanner) add(item Item) {
	if s.counts == nil {
		s.counts = map[Category]int{}
	}
	if s.counts[item.Category] >= s.opts.Limit {
		return
	}
	s.counts[item.Category]++

	if s.opts.Snapshot != nil {
		if rec, ok := s.opts.Snapshot.Owner(item.Claim); ok {
			item.ManagedBy = rec.Ref()
		}
	}
	s.report.Items = append(s.report.Items, item)
}

func (s *scanner) pathItem(category Category, path, detail, pkg string) {
	s.add(Item{
		Category: category,
		Claim:    string(resource.ClaimPath) + ":" + path,
		Detail:   detail,
		Package:  pkg,
	})
}

// modifiedConfigs asks the package manager which of its own files have been
// edited since installation. dpkg and rpm both keep checksums for exactly
// this purpose, and almost nobody uses them.
func (s *scanner) modifiedConfigs() error {
	if _, err := s.opts.Host.LookPath("dpkg"); err == nil {
		return s.dpkgVerify()
	}
	if _, err := s.opts.Host.LookPath("rpm"); err == nil {
		return s.rpmVerify()
	}
	return errors.New("needs dpkg or rpm to compare files against what the package shipped")
}

func (s *scanner) dpkgVerify() error {
	// `dpkg -V` prints one line per file that fails verification, in the form
	// "??5??????   c /etc/nginx/nginx.conf"; position 2 is the checksum flag
	// and the letter after the flags marks a conffile.
	res, err := s.opts.Host.Run(s.ctx, "dpkg", "-V")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		flags, path := fields[0], fields[len(fields)-1]
		if len(flags) < 3 || flags[2] != '5' {
			// Only checksum mismatches mean "the contents were edited";
			// mode and ownership differences are noisier and less certain.
			continue
		}
		if !s.underScanRoots(path) {
			continue
		}
		s.pathItem(CategoryModifiedConfig, path, "contents differ from what the package installed", s.owningPackage(path))
	}
	return nil
}

func (s *scanner) rpmVerify() error {
	// `rpm -Va` uses the same convention: position 2 of the flag string is
	// the digest check.
	res, err := s.opts.Host.Run(s.ctx, "rpm", "-Va", "--nofiles", "--noscripts")
	if err != nil {
		return err
	}
	if !res.OK() && strings.TrimSpace(res.Stdout) == "" {
		res, err = s.opts.Host.Run(s.ctx, "rpm", "-Va")
		if err != nil {
			return err
		}
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		flags, path := fields[0], fields[len(fields)-1]
		if len(flags) < 3 || flags[2] != '5' {
			continue
		}
		if !s.underScanRoots(path) {
			continue
		}
		s.pathItem(CategoryModifiedConfig, path, "contents differ from what the package installed", "")
	}
	return nil
}

// unpackagedConfigs finds files under the scan roots that no package owns.
func (s *scanner) unpackagedConfigs() error {
	haveDpkg := false
	if _, err := s.opts.Host.LookPath("dpkg-query"); err == nil {
		haveDpkg = true
	} else if _, err := s.opts.Host.LookPath("rpm"); err != nil {
		return errors.New("needs dpkg-query or rpm to tell packaged files from local ones")
	}

	for _, root := range s.opts.Paths {
		files, err := s.walk(root, 0)
		if err != nil {
			return err
		}
		for _, path := range files {
			if skipPath(path) {
				continue
			}
			if s.ownedByPackage(path, haveDpkg) {
				continue
			}
			s.pathItem(CategoryUnpackagedConfig, path, "no package owns this file", "")
		}
	}
	return nil
}

// maxWalkDepth keeps a scan of /etc bounded on machines with deep trees.
const maxWalkDepth = 6

func (s *scanner) walk(dir string, depth int) ([]string, error) {
	if depth > maxWalkDepth {
		return nil, nil
	}
	names, err := s.opts.Host.ReadDir(dir)
	if err != nil {
		if errors.Is(err, host.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, name := range names {
		full := strings.TrimRight(dir, "/") + "/" + name
		info, err := s.opts.Host.Stat(full)
		if err != nil {
			continue
		}
		if info.IsDir {
			nested, err := s.walk(full, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, nested...)
			continue
		}
		out = append(out, full)
	}
	return out, nil
}

func (s *scanner) ownedByPackage(path string, haveDpkg bool) bool {
	if haveDpkg {
		res, err := s.opts.Host.Run(s.ctx, "dpkg-query", "-S", path)
		return err == nil && res.OK()
	}
	res, err := s.opts.Host.Run(s.ctx, "rpm", "-qf", path)
	return err == nil && res.OK()
}

func (s *scanner) owningPackage(path string) string {
	res, err := s.opts.Host.Run(s.ctx, "dpkg-query", "-S", path)
	if err != nil || !res.OK() {
		return ""
	}
	name, _, found := strings.Cut(strings.TrimSpace(res.Stdout), ":")
	if !found {
		return ""
	}
	return name
}

// localUnits finds systemd units installed outside the package manager,
// which is where first-party services live.
func (s *scanner) localUnits() error {
	const dir = "/etc/systemd/system"
	names, err := s.opts.Host.ReadDir(dir)
	if err != nil {
		if errors.Is(err, host.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, name := range names {
		if !strings.Contains(name, ".") || strings.HasSuffix(name, ".wants") || strings.HasSuffix(name, ".d") {
			continue
		}
		s.pathItem(CategoryLocalUnit, dir+"/"+name, "unit installed locally rather than by a package", "")
	}
	return nil
}

// enabledServices lists what starts at boot.
func (s *scanner) enabledServices() error {
	res, err := s.opts.Host.Run(s.ctx, "systemctl", "list-unit-files", "--state=enabled", "--no-legend", "--no-pager")
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("systemctl list-unit-files failed: %s", strings.TrimSpace(res.Stderr))
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		unit := fields[0]
		s.add(Item{
			Category: CategoryEnabledService,
			Claim:    string(resource.ClaimUnit) + ":" + unit,
			Detail:   "enabled at boot",
		})
	}
	return nil
}

// manualPackages lists packages an operator asked for, as opposed to the
// dependency closure those pulled in.
func (s *scanner) manualPackages() error {
	if _, err := s.opts.Host.LookPath("apt-mark"); err == nil {
		res, err := s.opts.Host.Run(s.ctx, "apt-mark", "showmanual")
		if err != nil {
			return err
		}
		for _, name := range strings.Fields(res.Stdout) {
			s.add(Item{
				Category: CategoryManualPackage,
				Claim:    string(resource.ClaimPackage) + ":" + name,
				Detail:   "explicitly installed",
			})
		}
		return nil
	}
	if _, err := s.opts.Host.LookPath("dnf"); err == nil {
		res, err := s.opts.Host.Run(s.ctx, "dnf", "repoquery", "--userinstalled", "--qf", "%{name}")
		if err != nil {
			return err
		}
		for _, name := range strings.Fields(res.Stdout) {
			s.add(Item{
				Category: CategoryManualPackage,
				Claim:    string(resource.ClaimPackage) + ":" + name,
				Detail:   "explicitly installed",
			})
		}
		return nil
	}
	return errors.New("needs apt-mark or dnf to tell explicit installs from dependencies")
}

// localUsers finds non-system accounts, which are usually service accounts
// somebody created by hand.
func (s *scanner) localUsers() error {
	res, err := s.opts.Host.Run(s.ctx, "getent", "passwd")
	if err != nil {
		return err
	}
	if !res.OK() {
		return errors.New("getent passwd failed")
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		fields := strings.Split(strings.TrimSpace(line), ":")
		if len(fields) < 7 {
			continue
		}
		uid, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		// Below 1000 is the distro's own territory; 65534 is nobody.
		if uid < 1000 || uid == 65534 {
			continue
		}
		s.add(Item{
			Category: CategoryLocalUser,
			Claim:    string(resource.ClaimUser) + ":" + fields[0],
			Detail:   fmt.Sprintf("uid %d, shell %s", uid, fields[6]),
		})
	}
	return nil
}

func (s *scanner) sysctlDropIns() error {
	const dir = "/etc/sysctl.d"
	names, err := s.opts.Host.ReadDir(dir)
	if err != nil {
		if errors.Is(err, host.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, name := range names {
		if !strings.HasSuffix(name, ".conf") {
			continue
		}
		s.pathItem(CategorySysctlDropIn, dir+"/"+name, "kernel parameter drop-in", "")
	}
	return nil
}

func (s *scanner) underScanRoots(path string) bool {
	for _, root := range s.opts.Paths {
		if strings.HasPrefix(path, strings.TrimRight(root, "/")+"/") || path == root {
			return true
		}
	}
	return false
}

// skipPath filters out files that are state rather than configuration, and
// which no sane person would put in git.
func skipPath(path string) bool {
	noisy := []string{
		"/etc/ssl/certs/", "/etc/ca-certificates/", "/etc/pki/",
		"/etc/systemd/system/multi-user.target.wants/",
		"/etc/alternatives/", "/etc/rc0.d/", "/etc/rc1.d/", "/etc/rc2.d/",
		"/etc/rc3.d/", "/etc/rc4.d/", "/etc/rc5.d/", "/etc/rc6.d/", "/etc/rcS.d/",
		"/etc/apparmor.d/cache/", "/etc/ld.so.cache",
	}
	for _, prefix := range noisy {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	// Secrets and live credential stores do not belong in a git repository,
	// so inventory does not invite anyone to put them there.
	secrets := []string{"/etc/shadow", "/etc/gshadow", "/etc/sudoers.d/", "/etc/ssh/ssh_host_"}
	for _, prefix := range secrets {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	suffixes := []string{".dpkg-old", ".dpkg-dist", ".rpmnew", ".rpmsave", ".bak", "~", ".swp"}
	for _, suffix := range suffixes {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}
