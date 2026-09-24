package resource

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/vikki8/systemcd/internal/host"
	"github.com/vikki8/systemcd/internal/manifest"
)

func init() { Register("Sysctl", buildSysctl) }

// sysctlDropInDir is where persisted values are written so they survive a
// reboot. The 60- prefix sorts after distro defaults (10-, 50-), so systemcd's
// values win over those at boot. It still sorts before 99-sysctl.conf, the
// admin's /etc/sysctl.conf, which wins at boot until the next reconcile puts
// the runtime value back.
const sysctlDropInDir = "/etc/sysctl.d"

// SysctlSpec declares kernel parameters.
type SysctlSpec struct {
	// Key/Value set a single parameter; Values sets several at once.
	Key    string            `yaml:"key,omitempty"`
	Value  string            `yaml:"value,omitempty"`
	Values map[string]string `yaml:"values,omitempty"`
	// Persist writes a drop-in under /etc/sysctl.d so the value survives a
	// reboot. On by default.
	Persist *bool `yaml:"persist,omitempty"`
}

// Sysctl is the Sysctl resource kind.
type Sysctl struct {
	name   string
	spec   SysctlSpec
	values map[string]string
	keys   []string
}

var _ Resource = (*Sysctl)(nil)
var _ Deletable = (*Sysctl)(nil)

func buildSysctl(doc *manifest.Document) (Resource, error) {
	s := &Sysctl{name: doc.Metadata.Name}
	if err := doc.DecodeSpec(&s.spec); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Sysctl) ID() ID { return ID{Kind: "Sysctl", Name: s.name} }

func (s *Sysctl) Validate() error {
	s.values = map[string]string{}
	if s.spec.Key != "" {
		if s.spec.Value == "" {
			return errors.New("spec.value is required alongside spec.key")
		}
		s.values[s.spec.Key] = s.spec.Value
	}
	for k, v := range s.spec.Values {
		if prev, dup := s.values[k]; dup && prev != v {
			return fmt.Errorf("sysctl key %q is set to both %q (spec.value) and %q (spec.values)", k, prev, v)
		}
		s.values[k] = v
	}
	if len(s.values) == 0 {
		return errors.New("spec.key/spec.value or spec.values is required")
	}
	for k, v := range s.values {
		if k == "" {
			return errors.New("sysctl key is empty")
		}
		if strings.ContainsAny(k, " \t\r\n=") {
			return fmt.Errorf("sysctl key %q contains whitespace or '='", k)
		}
		// The key is an argument to `sysctl -n` during plan; "-p" or
		// "--system" would load and apply every sysctl.d file instead.
		if strings.HasPrefix(k, "-") {
			return fmt.Errorf("sysctl key %q starts with '-', which sysctl would read as an option", k)
		}
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("sysctl key %q has an empty value", k)
		}
		// One line per key in the drop-in: a newline in a value would
		// persist whatever follows it as another setting.
		if strings.ContainsAny(strings.TrimRight(v, "\n"), "\r\n") {
			return fmt.Errorf("sysctl value for %q spans more than one line", k)
		}
		s.keys = append(s.keys, k)
	}
	// The drop-in's file name is built from the resource name.
	if s.persist() && strings.Contains(s.name, "/") {
		return fmt.Errorf("metadata.name %q contains '/', which would place the drop-in outside %s", s.name, sysctlDropInDir)
	}
	normalizeSorted(s.keys)
	return nil
}

func (s *Sysctl) persist() bool { return s.spec.Persist == nil || *s.spec.Persist }

func (s *Sysctl) dropInPath() string {
	return path.Join(sysctlDropInDir, "60-systemcd-"+s.name+".conf")
}

// dropInContent renders the persisted file deterministically.
func (s *Sysctl) dropInContent() []byte {
	var b strings.Builder
	b.WriteString("# Managed by systemcd. Do not edit; change the manifest instead.\n")
	for _, k := range s.keys {
		fmt.Fprintf(&b, "%s = %s\n", k, s.values[k])
	}
	return []byte(b.String())
}

func (s *Sysctl) Desired(c *Context) (State, error) {
	st := State{}
	for _, k := range s.keys {
		st["sysctl:"+k] = normalizeSysctlValue(s.values[k])
	}
	if s.persist() {
		st["persisted"] = checksum(s.dropInContent())
		st["_dropIn"] = s.dropInPath()
	}
	return st, nil
}

func (s *Sysctl) Observe(c *Context) (State, error) {
	st := State{}
	for _, k := range s.keys {
		res, err := c.Host.Run(c.Ctx, "sysctl", "-n", k)
		if err != nil {
			return nil, err
		}
		if !res.OK() {
			st["sysctl:"+k] = "<unreadable>"
			continue
		}
		st["sysctl:"+k] = normalizeSysctlValue(res.Stdout)
	}
	if s.persist() {
		data, err := c.Host.ReadFile(s.dropInPath())
		switch {
		case errors.Is(err, host.ErrNotExist):
			st["persisted"] = "<absent>"
		case err != nil:
			return nil, err
		default:
			st["persisted"] = checksum(data)
		}
		st["_dropIn"] = s.dropInPath()
	}
	return st, nil
}

func (s *Sysctl) Apply(c *Context, d Diff) error {
	for _, f := range d {
		if !strings.HasPrefix(f.Field, "sysctl:") {
			continue
		}
		key := strings.TrimPrefix(f.Field, "sysctl:")
		if err := run(c, "sysctl", "-w", key+"="+s.values[key]); err != nil {
			return err
		}
		c.Log("sysctl %s = %s", key, s.values[key])
	}
	if d.Has("persisted") {
		if err := c.Host.WriteFile(s.dropInPath(), s.dropInContent(), 0o644); err != nil {
			return err
		}
		c.Log("wrote %s", s.dropInPath())
	}
	return nil
}

func (s *Sysctl) Delete(c *Context) error {
	if !s.persist() {
		return nil
	}
	err := c.Host.Remove(s.dropInPath())
	if errors.Is(err, host.ErrNotExist) {
		return nil
	}
	return err
}

// normalizeSysctlValue collapses the tabs the kernel uses between fields of
// multi-value parameters, so "4096\t87380\t6291456" compares equal to the
// space-separated form a manifest would naturally write.
func normalizeSysctlValue(v string) string {
	return strings.Join(strings.Fields(v), " ")
}

func normalizeSorted(items []string) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j] < items[j-1]; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}
