package resource

import (
	"errors"
	"fmt"
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
	// without a suffix is treated as a .service.
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

func (s *Service) Validate() error {
	unit := s.spec.Unit
	if unit == "" {
		unit = s.name
	}
	if !strings.Contains(unit, ".") {
		unit += ".service"
	}
	s.unit = unit

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
	st["enabled"] = strconv.FormatBool(enabledFromOutput(res.Stdout))
	st["_enabledRaw"] = strings.TrimSpace(res.Stdout)

	res, err = c.Host.Run(c.Ctx, "systemctl", s.systemctl("is-active", s.unit)...)
	if err != nil {
		return nil, fmt.Errorf("systemctl is-active %s: %w", s.unit, err)
	}
	active := strings.TrimSpace(res.Stdout)
	if active == "" {
		active = "unknown"
	}
	// `activating` and `deactivating` are transient; report the settled value
	// they are heading toward so a reconcile mid-transition does not thrash.
	switch active {
	case "activating":
		active = "active"
	case "deactivating":
		active = "inactive"
	}
	st["active"] = active
	return st, nil
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
	case "active", "activating":
		return HealthHealthy, state, nil
	case "":
		return HealthUnknown, "no output from systemctl", nil
	default:
		detail := state
		if log, err := c.Host.Run(c.Ctx, "systemctl", s.systemctl("status", "--no-pager", "--lines=5", s.unit)...); err == nil {
			if trimmed := strings.TrimSpace(log.Stdout); trimmed != "" {
				detail = state + "\n" + trimmed
			}
		}
		return HealthDegraded, detail, nil
	}
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
	// .service suffix if it has none.
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
}

var (
	_ Resource  = (*SystemdUnit)(nil)
	_ Deletable = (*SystemdUnit)(nil)
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
	if !strings.Contains(unit, ".") {
		unit += ".service"
	}
	u.unit = unit

	dir := u.spec.Directory
	if dir == "" {
		if u.spec.Scope == "user" {
			dir = "/etc/systemd/user"
		} else {
			dir = "/etc/systemd/system"
		}
	}
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
			Path:    strings.TrimRight(dir, "/") + "/" + unit,
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

func (u *SystemdUnit) Desired(c *Context) (State, error) { return u.file.Desired(c) }
func (u *SystemdUnit) Observe(c *Context) (State, error) { return u.file.Observe(c) }

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
