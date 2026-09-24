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
	// ran records that Apply already ran the command in this reconcile
	// (resources are rebuilt for every reconcile), so a notify arriving for
	// the same run does not run it a second time.
	ran bool
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
	if len(e.spec.Argv) > 0 && e.spec.Argv[0] == "" {
		return errors.New("spec.argv[0] must name the program to run")
	}
	// A relative path would be checked against whatever directory systemcd
	// happens to run in, so the guard could pass for the agent and fail for
	// a hand-run apply (or the reverse).
	if e.spec.Creates != "" && !strings.HasPrefix(e.spec.Creates, "/") {
		return fmt.Errorf("spec.creates %q must be an absolute path", e.spec.Creates)
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
	done, why, err := e.satisfied(c)
	if err != nil {
		return nil, err
	}
	if !done {
		// No guard reported the work as done: the command is pending.
		s["run"] = execPending
		return s, nil
	}
	s["run"] = execSatisfied
	if why == execOnlyIfFailed {
		s["_skipped"] = why
	}
	return s, nil
}

const execOnlyIfFailed = "onlyIf did not match"

// satisfied evaluates the guards; why says which one held. Guard commands
// are expected to be read-only: they run during plan too.
func (e *Exec) satisfied(c *Context) (bool, string, error) {
	// onlyIf is a precondition, not a guard on the result: if it fails, there
	// is nothing to do and the resource counts as satisfied.
	if e.spec.OnlyIf != "" {
		ok, err := shellSucceeds(c, e.spec.OnlyIf)
		if err != nil {
			return false, "", err
		}
		if !ok {
			return true, execOnlyIfFailed, nil
		}
	}
	if e.spec.Creates != "" {
		if _, err := c.Host.Stat(e.spec.Creates); err == nil {
			return true, e.spec.Creates + " exists", nil
		} else if !errors.Is(err, host.ErrNotExist) {
			return false, "", err
		}
	}
	if e.spec.Unless != "" {
		ok, err := shellSucceeds(c, e.spec.Unless)
		if err != nil {
			return false, "", err
		}
		if ok {
			return true, "unless succeeded", nil
		}
	}
	return false, "", nil
}

func (e *Exec) Apply(c *Context, d Diff) error {
	err := e.exec(c)
	e.ran = true
	return err
}

// Refresh runs the command in response to a notify. The guards still apply,
// as they do for Puppet's refresh: a refreshOnly Exec with `creates` or
// `onlyIf` must not run just because it was notified. And a notified Exec
// whose own Apply already ran it this reconcile is not run twice.
func (e *Exec) Refresh(c *Context) error {
	if e.ran {
		return nil
	}
	done, why, err := e.satisfied(c)
	if err != nil {
		return err
	}
	if done {
		c.Log("notified, not run: %s", why)
		return nil
	}
	return e.exec(c)
}

func (e *Exec) exec(c *Context) error {
	if c.DryRun {
		// The engine never applies during a plan; this keeps the escape
		// hatch safe even if a caller forgets.
		c.Log("would run %s", e.describe())
		return nil
	}
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
