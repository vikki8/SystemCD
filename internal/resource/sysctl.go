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
// reboot. A high prefix keeps systemcd's values winning over distro defaults.
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
		s.values[k] = v
	}
	if len(s.values) == 0 {
		return errors.New("spec.key/spec.value or spec.values is required")
	}
	for k := range s.values {
		if strings.ContainsAny(k, " \t\n=") {
			return fmt.Errorf("sysctl key %q contains whitespace or '='", k)
		}
		s.keys = append(s.keys, k)
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
