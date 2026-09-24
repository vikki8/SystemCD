package host

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func needShell(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no /bin/sh")
	}
}

func TestRunReportsATimeoutAsAnError(t *testing.T) {
	// The Host contract reserves the error for commands that did not run to
	// completion. A killed command reported as an ordinary non-zero exit
	// reads to an Exec guard like `unless` as "condition false, go ahead".
	needShell(t)
	h := NewOS()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	res, err := h.Run(ctx, "sleep", "5")
	if err == nil {
		t.Fatalf("Run returned no error for a command stopped by its deadline (exit %d)", res.ExitCode)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if res.OK() {
		t.Error("an interrupted command must not look successful")
	}
}

func TestRunAppliesCommandTimeout(t *testing.T) {
	needShell(t)
	h := NewOS()
	h.CommandTimeout = 200 * time.Millisecond
	start := time.Now()
	_, err := h.Run(context.Background(), "sleep", "5")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a deadline error", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("Run took %v", time.Since(start))
	}
}

func TestRunDoesNotHangOnABackgroundedChild(t *testing.T) {
	// An Exec that starts a daemon leaves a child holding stdout open. The
	// command itself has finished; Run must not wait for the daemon.
	needShell(t)
	h := NewOS()
	h.CommandTimeout = time.Minute
	start := time.Now()
	res, err := h.RunShell(context.Background(), "sleep 30 & echo started")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.OK() || strings.TrimSpace(res.Stdout) != "started" {
		t.Errorf("res = %+v", res)
	}
	if elapsed > waitDelay+5*time.Second {
		t.Errorf("Run blocked for %v waiting on a backgrounded child", elapsed)
	}
}

func TestRunSendsSIGTERMBeforeKilling(t *testing.T) {
	// git removes its index.lock when terminated but not when killed; a
	// stale lock would wedge every later sync.
	needShell(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "cleaned-up")
	h := NewOS()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	_, err := h.RunShell(ctx, `trap 'echo yes > "`+marker+`"; exit 1' TERM; sleep 30 & wait`)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Error("the command was killed without a chance to clean up")
	}
}

func TestRunCapturesExitCodeAndOutput(t *testing.T) {
	needShell(t)
	h := NewOS()
	res, err := h.RunShell(context.Background(), "echo out; echo err >&2; exit 3")
	if err != nil {
		t.Fatalf("a non-zero exit is not an execution failure: %v", err)
	}
	if res.ExitCode != 3 || strings.TrimSpace(res.Stdout) != "out" || strings.TrimSpace(res.Stderr) != "err" {
		t.Errorf("res = %+v", res)
	}
}

func TestMemRunHonoursACancelledContext(t *testing.T) {
	m := NewMem()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Run(ctx, "systemctl", "restart", "nginx"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if m.Ran("systemctl restart") {
		t.Error("a command with a cancelled context must not run")
	}
}

func TestWriteFileLeavesNoTempFiles(t *testing.T) {
	root := t.TempDir()
	h := NewRootedOS(root)
	must(t, h.WriteFile("/etc/a.conf", []byte("x"), 0o644))
	must(t, h.WriteFileSync("/etc/b.conf", []byte("y"), 0o600))
	_ = h.WriteFile("/etc", []byte("z"), 0o644) // fails: a directory

	entries, err := os.ReadDir(filepath.Join(root, "etc"))
	must(t, err)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".systemcd-") {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}
	info, err := h.Stat("/etc/b.conf")
	must(t, err)
	if info.Mode != 0o600 {
		t.Errorf("mode = %o", info.Mode)
	}
}
