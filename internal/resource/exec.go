package resource

import (
	"errors"
	"fmt"
	"strings"

	"github.com/vikki8/systemcd/internal/host"
	"github.com/vikki8/systemcd/internal/manifest"
)

func init() { Register("Exec", buildExec) }

// ExecSpec declares a command to run, with guards that make it idempotent.
//
// Reconciliation is only meaningful if a resource can report whether it is
// already satisfied, so an Exec must carry at least one guard (creates,
// unless, onlyIf), be refreshOnly, or opt into running every time with
// `always: true`.
type ExecSpec struct {
	// Command runs through /bin/sh -c. Argv runs a binary directly, with no
	// shell involved. Exactly one is required.
	Command string   `yaml:"command,omitempty"`
	Argv    []string `yaml:"argv,omitempty"`
	// Creates marks the command satisfied once this path exists.
	Creates string `yaml:"creates,omitempty"`
	// Unless marks the command satisfied when this shell command exits 0.
	Unless string `yaml:"unless,omitempty"`
	// OnlyIf suppresses the command unless this shell command exits 0.
	OnlyIf string `yaml:"onlyIf,omitempty"`
	// RefreshOnly runs the command only when another resource notifies it.
	RefreshOnly bool `yaml:"refreshOnly,omitempty"`
	// Always runs the command on every apply. Use sparingly: it makes every
	// plan show a pending change.
	Always bool `yaml:"always,omitempty"`
}

// Exec is the Exec resource kind.
type Exec struct {
	name string
	spec ExecSpec
}

var (
	_ Resource    = (*Exec)(nil)
	_ Refreshable = (*Exec)(nil)
)

func buildExec(doc *manifest.Document) (Resource, error) {
	e := &Exec{name: doc.Metadata.Name}
	if err := doc.DecodeSpec(&e.spec); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Exec) ID() ID { return ID{Kind: "Exec", Name: e.name} }

func (e *Exec) Validate() error {
	if (e.spec.Command == "") == (len(e.spec.Argv) == 0) {
		return errors.New("exactly one of spec.command or spec.argv is required")
	}
	guarded := e.spec.Creates != "" || e.spec.Unless != "" || e.spec.OnlyIf != "" ||
		e.spec.RefreshOnly || e.spec.Always
	if !guarded {
		return errors.New("an Exec needs a guard: set creates, unless, onlyIf, refreshOnly, or always: true")
	}
	return nil
}

const (
	execSatisfied = "satisfied"
	execPending   = "pending"
)

func (e *Exec) Desired(c *Context) (State, error) {
	if e.spec.RefreshOnly {
		// A refresh-only Exec never drives its own diff; it fires solely
		// through notify, so it always reports itself as satisfied.
		return State{"run": execSatisfied, "_mode": "refreshOnly"}, nil
	}
	return State{"run": execSatisfied, "_command": e.describe()}, nil
}

func (e *Exec) Observe(c *Context) (State, error) {
	if e.spec.RefreshOnly {
		return State{"run": execSatisfied, "_mode": "refreshOnly"}, nil
	}
	s := State{"_command": e.describe()}

	// onlyIf is a precondition, not a guard on the result: if it fails, there
	// is nothing to do and the resource counts as satisfied.
	if e.spec.OnlyIf != "" {
		ok, err := shellSucceeds(c, e.spec.OnlyIf)
		if err != nil {
			return nil, err
		}
		if !ok {
			s["run"] = execSatisfied
			s["_skipped"] = "onlyIf did not match"
			return s, nil
		}
	}
	if e.spec.Creates != "" {
		if _, err := c.Host.Stat(e.spec.Creates); err == nil {
			s["run"] = execSatisfied
			return s, nil
		} else if !errors.Is(err, host.ErrNotExist) {
			return nil, err
		}
	}
	if e.spec.Unless != "" {
		ok, err := shellSucceeds(c, e.spec.Unless)
		if err != nil {
			return nil, err
		}
		if ok {
			s["run"] = execSatisfied
			return s, nil
		}
	}
	// No guard reported the work as done: the command is pending.
	s["run"] = execPending
	return s, nil
}

func (e *Exec) Apply(c *Context, d Diff) error { return e.exec(c) }

// Refresh runs the command in response to a notify.
func (e *Exec) Refresh(c *Context) error { return e.exec(c) }

func (e *Exec) exec(c *Context) error {
	var res host.Result
	var err error
	if len(e.spec.Argv) > 0 {
		res, err = c.Host.Run(c.Ctx, e.spec.Argv[0], e.spec.Argv[1:]...)
	} else {
		res, err = c.Host.RunShell(c.Ctx, e.spec.Command)
	}
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("%s: exit %d: %s", e.describe(), res.ExitCode, firstLine(res.Stderr, res.Stdout))
	}
	c.Log("ran %s", e.describe())
	return nil
}

func (e *Exec) describe() string {
	if len(e.spec.Argv) > 0 {
		return strings.Join(e.spec.Argv, " ")
	}
	return e.spec.Command
}

func shellSucceeds(c *Context, script string) (bool, error) {
	res, err := c.Host.RunShell(c.Ctx, script)
	if err != nil {
		return false, err
	}
	return res.OK(), nil
}
