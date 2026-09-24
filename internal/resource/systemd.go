package resource

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/vikki8/systemcd/internal/manifest"
)

func init() {
	Register("Service", buildService)
	Register("SystemdUnit", buildSystemdUnit)
}

// ServiceSpec declares the desired runtime state of a systemd unit.
type ServiceSpec struct {
	// Unit overrides the unit name; metadata.name is used when empty. A name
	// without a unit type suffix is treated as a .service.
	Unit string `yaml:"unit,omitempty"`
	// Enabled controls start-on-boot. Unset means "don't manage it".
	Enabled *bool `yaml:"enabled,omitempty"`
	// State is started, stopped, or unmanaged (the default when empty is
	// started, matching the usual intent of declaring a Service).
	State string `yaml:"state,omitempty"`
	// Reload makes notify handlers issue `systemctl reload` instead of
	// `restart`, for daemons that support it (nginx, sshd, haproxy).
	Reload bool `yaml:"reload,omitempty"`
	// Scope selects `--user` units; defaults to system.
	Scope string `yaml:"scope,omitempty"`
}

const (
	serviceStarted   = "started"
	serviceStopped   = "stopped"
	serviceUnmanaged = "unmanaged"
)

// Service is the Service resource kind.
type Service struct {
	name string
	spec ServiceSpec
	unit string
}

var (
	_ Resource    = (*Service)(nil)
	_ Refreshable = (*Service)(nil)
	_ Checker     = (*Service)(nil)
)

func buildService(doc *manifest.Document) (Resource, error) {
	s := &Service{name: doc.Metadata.Name}
	if err := doc.DecodeSpec(&s.spec); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) ID() ID { return ID{Kind: "Service", Name: s.name} }

// unitTypes are the suffixes systemd recognises as unit types.
var unitTypes = []string{
	".service", ".socket", ".device", ".mount", ".automount", ".swap",
	".target", ".path", ".timer", ".slice", ".scope",
}

// normalizeUnitName gives a name without a unit type suffix the .service one,
// as systemctl itself does. Checking for any "." is not enough: "php8.2-fpm"
// is a service, and a unit file written under that bare name is never loaded.
func normalizeUnitName(name string) string {
	for _, t := range unitTypes {
		if strings.HasSuffix(name, t) && len(name) > len(t) {
			return name
		}
	}
	return name + ".service"
}

// checkUnitName rejects names that systemctl would read as an option, or
// that would leave the unit directory when used as a file name.
func checkUnitName(name string) error {
	if name == "" {
		return errors.New("unit name is empty")
	}
	if len(name) > 255 {
		return fmt.Errorf("unit name %q is longer than systemd's 255 characters", name)
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("unit name %q starts with '-', which systemctl would read as an option", name)
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(":_.\\@-", r) {
			continue
		}
		return fmt.Errorf("unit name %q contains %q; unit names use only letters, digits and : _ . \\ @ -", name, r)
	}
	return nil
}

func (s *Service) Validate() error {
	unit := s.spec.Unit
	if unit == "" {
		unit = s.name
	}
	if err := checkUnitName(unit); err != nil {
		return err
	}
	s.unit = normalizeUnitName(unit)

	switch s.spec.State {
	case "":
		s.spec.State = serviceStarted
	case serviceStarted, serviceStopped, serviceUnmanaged:
	default:
		return fmt.Errorf("spec.state must be %q, %q or %q, got %q", serviceStarted, serviceStopped, serviceUnmanaged, s.spec.State)
	}
	switch s.spec.Scope {
	case "", "system", "user":
	default:
		return fmt.Errorf("spec.scope must be \"system\" or \"user\", got %q", s.spec.Scope)
	}
	return nil
}

// systemctl builds an argument list honoring the unit scope.
func (s *Service) systemctl(args ...string) []string {
	if s.spec.Scope == "user" {
		return append([]string{"--user"}, args...)
	}
	return args
}

func (s *Service) Desired(c *Context) (State, error) {
	st := State{"_unit": s.unit}
	if s.spec.Enabled != nil {
		st["enabled"] = strconv.FormatBool(*s.spec.Enabled)
	}
	switch s.spec.State {
	case serviceStarted:
		st["active"] = "active"
	case serviceStopped:
		st["active"] = "inactive"
	}
	return st, nil
}

func (s *Service) Observe(c *Context) (State, error) {
	st := State{"_unit": s.unit}

	res, err := c.Host.Run(c.Ctx, "systemctl", s.systemctl("is-enabled", s.unit)...)
	if err != nil {
		return nil, fmt.Errorf("systemctl is-enabled %s: %w", s.unit, err)
	}
	raw := strings.TrimSpace(res.Stdout)
	if raw == "" && !res.OK() {
		// is-enabled answers on stdout even for a missing unit ("not-found",
		// exit 4); silence plus a failure means systemctl could not answer,
		// which must not be reported as "disabled". systemd before v237
		// said "No such file or directory" on stderr for a missing unit.
		if !strings.Contains(res.Stderr, "No such file or directory") {
			return nil, fmt.Errorf("systemctl is-enabled %s: exit %d: %s", s.unit, res.ExitCode, firstLine(res.Stderr))
		}
		raw = "not-found"
	}
	st["enabled"] = strconv.FormatBool(enabledFromOutput(raw))
	st["_enabledRaw"] = raw
	if s.spec.Enabled != nil && enablementNotApplicable(raw) {
		// Neither enable nor disable changes these units: both succeed and
		// is-enabled answers the same afterwards. Reporting a mismatch would
		// run a no-op `systemctl disable` on every reconcile, forever (and
		// for an indirect unit, disable the units its Also= names each time).
		st["enabled"] = strconv.FormatBool(*s.spec.Enabled)
	}

	res, err = c.Host.Run(c.Ctx, "systemctl", s.systemctl("is-active", s.unit)...)
	if err != nil {
		return nil, fmt.Errorf("systemctl is-active %s: %w", s.unit, err)
	}
	active := strings.TrimSpace(res.Stdout)
	if active == "" && !res.OK() {
		// is-active prints a state even for a missing unit. Nothing on
		// stdout and a failure is systemctl unable to reach the manager
		// ("System has not been booted with systemd", "Failed to connect to
		// bus"); planning a start from that would be inventing a state.
		return nil, fmt.Errorf("systemctl is-active %s: exit %d: %s", s.unit, res.ExitCode, firstLine(res.Stderr))
	}
	if active == "" {
		active = "unknown"
	}
	// `activating` and `deactivating` are transient; report the settled value
	// they are heading toward so a reconcile mid-transition does not thrash.
	// `reloading` and `refreshing` are a running unit doing just that.
	switch active {
	case "activating", "reloading", "refreshing":
		active = "active"
	case "deactivating":
		active = "inactive"
	case "failed":
		// A failed unit is not running, and `systemctl stop` leaves it
		// failed (only start or reset-failed clears it). For a unit meant
		// to be stopped that is the settled state, not drift to stop again.
		if s.spec.State == serviceStopped {
			active = "inactive"
		}
	}
	st["active"] = active
	return st, nil
}

// enablementNotApplicable reports is-enabled states that enable and disable
// cannot change: static units have no [Install] section, indirect ones are
// enabled through the units their Also= names, and generated and transient
// units are not enableable at all.
func enablementNotApplicable(state string) bool {
	switch state {
	case "static", "indirect", "generated", "transient":
		return true
	default:
		return false
	}
}

// enabledFromOutput interprets `systemctl is-enabled`. Units that are static,
// indirect, or aliases cannot be enabled and must not be reported as disabled,
// or every reconcile would try (and fail) to enable them forever.
func enabledFromOutput(out string) bool {
	switch strings.TrimSpace(out) {
	case "enabled", "enabled-runtime", "static", "indirect", "alias", "generated", "transient":
		return true
	default:
		return false
	}
}

func (s *Service) Apply(c *Context, d Diff) error {
	if want, ok := d.Want("enabled"); ok {
		verb := "disable"
		if want == "true" {
			verb = "enable"
		}
		if err := s.run(c, verb, s.unit); err != nil {
			return err
		}
		c.Log("systemctl %s %s", verb, s.unit)
	}
	if want, ok := d.Want("active"); ok {
		verb := "stop"
		if want == "active" {
			verb = "start"
		}
		if err := s.run(c, verb, s.unit); err != nil {
			return err
		}
		c.Log("systemctl %s %s", verb, s.unit)
	}
	return nil
}

// Refresh handles `notify`: a config file changed, so bounce the daemon. A
// stopped service is deliberately left stopped.
func (s *Service) Refresh(c *Context) error {
	if s.spec.State == serviceStopped {
		return nil
	}
	verb := "restart"
	if s.spec.Reload {
		verb = "reload-or-restart"
	}
	if s.spec.State == serviceUnmanaged {
		// Whether this unit runs is not systemcd's to decide, so a notify
		// may bounce it if it is running but must not start it: plain
		// restart and reload-or-restart both start a stopped unit.
		verb = "try-" + verb
	}
	if err := s.run(c, verb, s.unit); err != nil {
		return err
	}
	c.Log("systemctl %s %s", verb, s.unit)
	return nil
}

func (s *Service) Health(c *Context) (HealthStatus, string, error) {
	if s.spec.State != serviceStarted {
		return HealthHealthy, "", nil
	}
	res, err := c.Host.Run(c.Ctx, "systemctl", s.systemctl("is-active", s.unit)...)
	if err != nil {
		return HealthUnknown, "", err
	}
	switch state := strings.TrimSpace(res.Stdout); state {
	case "active", "reloading", "refreshing":
		return HealthHealthy, state, nil
	case "activating":
		// A crash-looping unit with Restart= spends its RestartSec in
		// "activating (auto-restart)" and, unless it hits the start limit,
		// never reaches "failed". That is not a unit on its way up.
		if s.autoRestarting(c) {
			return HealthDegraded, s.withStatus(c, "activating (auto-restart): the unit keeps exiting and being restarted"), nil
		}
		return HealthHealthy, state, nil
	case "":
		return HealthUnknown, "no output from systemctl", nil
	default:
		return HealthDegraded, s.withStatus(c, state), nil
	}
}

// autoRestarting reports whether the unit is waiting to be restarted after
// exiting. A failed query means "cannot tell", which is not a verdict.
func (s *Service) autoRestarting(c *Context) bool {
	res, err := c.Host.Run(c.Ctx, "systemctl", s.systemctl("show", "--property=SubState", s.unit)...)
	if err != nil || !res.OK() {
		return false
	}
	// "auto-restart", or "auto-restart-queued" on systemd 254 and later.
	return strings.HasPrefix(strings.TrimSpace(res.Stdout), "SubState=auto-restart")
}

// withStatus appends the tail of `systemctl status` to a health detail.
func (s *Service) withStatus(c *Context, detail string) string {
	if log, err := c.Host.Run(c.Ctx, "systemctl", s.systemctl("status", "--no-pager", "--lines=5", s.unit)...); err == nil {
		if trimmed := strings.TrimSpace(log.Stdout); trimmed != "" {
			return detail + "\n" + trimmed
		}
	}
	return detail
}

func (s *Service) run(c *Context, args ...string) error {
	res, err := c.Host.Run(c.Ctx, "systemctl", s.systemctl(args...)...)
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("systemctl %s: exit %d: %s", strings.Join(args, " "), res.ExitCode, firstLine(res.Stderr, res.Stdout))
	}
	return nil
}

// SystemdUnitSpec declares a unit file managed by systemcd.
type SystemdUnitSpec struct {
	// Unit is the file name; metadata.name is used when empty, gaining a
	// .service suffix if it has no unit type suffix. In a drop-in directory
	// (spec.directory ending in .d) it must be a *.conf name.
	Unit    string `yaml:"unit,omitempty"`
	Content string `yaml:"content,omitempty"`
	Source  string `yaml:"source,omitempty"`
	// Directory defaults to /etc/systemd/system.
	Directory string `yaml:"directory,omitempty"`
	State     string `yaml:"state,omitempty"`
	Scope     string `yaml:"scope,omitempty"`
}

// SystemdUnit writes a unit file and keeps systemd's view of it current.
type SystemdUnit struct {
	name string
	spec SystemdUnitSpec
	file *File
	unit string
	// reloadProbe is the unit whose NeedDaemonReload property reflects this
	// file: the unit itself, or the one a drop-in extends. Empty when there
	// is no single such unit (a template, a type-wide drop-in).
	reloadProbe string
}

var (
	_ Resource  = (*SystemdUnit)(nil)
	_ Deletable = (*SystemdUnit)(nil)
)

// Values of the SystemdUnit "loaded" field.
const (
	unitLoadedCurrent = "current"
	unitLoadedStale   = "stale"
)

func buildSystemdUnit(doc *manifest.Document) (Resource, error) {
	u := &SystemdUnit{name: doc.Metadata.Name}
	if err := doc.DecodeSpec(&u.spec); err != nil {
		return nil, err
	}
	return u, nil
}

func (u *SystemdUnit) ID() ID { return ID{Kind: "SystemdUnit", Name: u.name} }

func (u *SystemdUnit) Validate() error {
	unit := u.spec.Unit
	if unit == "" {
		unit = u.name
	}
	// The name becomes a file name under the unit directory, so it must not
	// be able to leave it ("../../cron.d/x" would also slip past claims).
	if err := checkUnitName(unit); err != nil {
		return err
	}
	switch u.spec.Scope {
	case "", "system", "user":
	default:
		return fmt.Errorf("spec.scope must be \"system\" or \"user\", got %q", u.spec.Scope)
	}

	dir := u.spec.Directory
	if dir == "" {
		if u.spec.Scope == "user" {
			dir = "/etc/systemd/user"
		} else {
			dir = "/etc/systemd/system"
		}
	}
	dir = strings.TrimRight(dir, "/")
	// The path is also the ownership claim, so it must be canonical: a
	// directory spelled with ".." would claim one path and write another.
	if !strings.HasPrefix(dir, "/") || path.Clean(dir) != dir {
		return fmt.Errorf("spec.directory %q must be a clean absolute path", u.spec.Directory)
	}
	if parent, dropIn := strings.CutSuffix(dir, ".d"); dropIn {
		// systemd reads only *.conf in a drop-in directory.
		if !strings.HasSuffix(unit, ".conf") {
			return fmt.Errorf("%s is a drop-in directory, where systemd only reads *.conf files; got %q", dir, unit)
		}
		if base := parent[strings.LastIndex(parent, "/")+1:]; normalizeUnitName(base) == base && !strings.Contains(base, "@.") {
			u.reloadProbe = base
		}
	} else {
		if strings.HasSuffix(unit, ".conf") {
			return fmt.Errorf("%q is a drop-in name; set spec.directory to the unit's .d directory", unit)
		}
		unit = normalizeUnitName(unit)
		if !strings.Contains(unit, "@.") {
			u.reloadProbe = unit
		}
	}
	u.unit = unit

	state, err := normalizePresence(u.spec.State)
	if err != nil {
		return err
	}
	u.spec.State = state

	// A unit file is a file; reuse the File provider for the heavy lifting so
	// mode/ownership/backup semantics stay identical.
	u.file = &File{
		name: u.name,
		spec: FileSpec{
			Path:    dir + "/" + unit,
			Content: u.spec.Content,
			Source:  u.spec.Source,
			Mode:    "0644",
			State:   state,
		},
	}
	if err := u.file.Validate(); err != nil {
		return err
	}
	if state == Present && u.spec.Content == "" && u.spec.Source == "" {
		return errors.New("spec.content or spec.source is required")
	}
	return nil
}

// Desired adds "loaded" to the file's state: systemd must be running the
// file that is on disk. Without it, a daemon-reload that failed once (or a
// unit file changed behind systemd's back) is never retried, because the
// file itself is in sync on every later run.
func (u *SystemdUnit) Desired(c *Context) (State, error) {
	st, err := u.file.Desired(c)
	if err != nil || u.spec.State != Present || u.reloadProbe == "" {
		return st, err
	}
	st["loaded"] = unitLoadedCurrent
	return st, nil
}

func (u *SystemdUnit) Observe(c *Context) (State, error) {
	st, err := u.file.Observe(c)
	if err != nil || u.spec.State != Present || u.reloadProbe == "" || st["state"] != Present {
		return st, err
	}
	stale, err := u.needsDaemonReload(c)
	if err != nil {
		return nil, err
	}
	st["loaded"] = unitLoadedCurrent
	if stale {
		st["loaded"] = unitLoadedStale
	}
	return st, nil
}

// needsDaemonReload asks systemd whether its loaded copy of the unit is older
// than the files on disk. A query that runs but fails (no systemd manager, as
// in a chroot or image build) has nothing loaded to be stale, so it is not
// drift.
func (u *SystemdUnit) needsDaemonReload(c *Context) (bool, error) {
	args := []string{"show", "--property=NeedDaemonReload", u.reloadProbe}
	if u.spec.Scope == "user" {
		args = append([]string{"--user"}, args...)
	}
	res, err := c.Host.Run(c.Ctx, "systemctl", args...)
	if err != nil {
		return false, fmt.Errorf("systemctl show %s: %w", u.reloadProbe, err)
	}
	return res.OK() && strings.TrimSpace(res.Stdout) == "NeedDaemonReload=yes", nil
}

func (u *SystemdUnit) Apply(c *Context, d Diff) error {
	if err := u.file.Apply(c, d); err != nil {
		return err
	}
	return u.daemonReload(c)
}

func (u *SystemdUnit) Delete(c *Context) error {
	if err := u.file.Delete(c); err != nil {
		return err
	}
	return u.daemonReload(c)
}

func (u *SystemdUnit) daemonReload(c *Context) error {
	args := []string{"daemon-reload"}
	if u.spec.Scope == "user" {
		args = append([]string{"--user"}, args...)
	}
	res, err := c.Host.Run(c.Ctx, "systemctl", args...)
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("systemctl daemon-reload: exit %d: %s", res.ExitCode, firstLine(res.Stderr, res.Stdout))
	}
	c.Log("systemctl daemon-reload")
	return nil
}

// firstLine picks the first non-empty line from the given candidates, for
// compact error messages.
func firstLine(candidates ...string) string {
	for _, cand := range candidates {
		for _, line := range strings.Split(cand, "\n") {
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				return trimmed
			}
		}
	}
	return "no output"
}
