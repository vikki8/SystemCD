package resource

import (
	"strings"
	"testing"

	"github.com/vikki8/systemcd/internal/host"
)

func TestSysctlRejectsKeysThatAreOptions(t *testing.T) {
	// Observe runs `sysctl -n <key>` during plan. A key of "-p" or
	// "--system" makes that load and apply sysctl config files, so a plan
	// would change the kernel.
	for _, key := range []string{"-p", "--system", "-w"} {
		err := buildErr(t, "apiVersion: systemcd.dev/v1\nkind: Sysctl\nmetadata:\n  name: x\nspec:\n  values:\n    \""+key+"\": \"1\"\n")
		if err == nil || !strings.Contains(err.Error(), "option") {
			t.Errorf("key %q: error = %v, want a rejection", key, err)
		}
	}
	// Both separator styles, and interface names with dots written with a
	// slash, are real keys.
	for _, key := range []string{"net.ipv4.ip_forward", "net/ipv4/ip_forward", "net.ipv4.conf.eth0/100.rp_filter", "kernel.core_pattern"} {
		if err := buildErr(t, "apiVersion: systemcd.dev/v1\nkind: Sysctl\nmetadata:\n  name: x\nspec:\n  key: \""+key+"\"\n  value: \"1\"\n"); err != nil {
			t.Errorf("key %q should be accepted: %v", key, err)
		}
	}
}

func TestSysctlPlanRunsOnlyReads(t *testing.T) {
	h := host.NewMem()
	c := newContext(h)
	c.DryRun = true
	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Sysctl\nmetadata:\n  name: x\nspec:\n  key: vm.swappiness\n  value: \"10\"\n")
	if _, err := r.Observe(c); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range h.Commands {
		if cmd != "sysctl -n vm.swappiness" {
			t.Errorf("observe ran %q; only reads are allowed", cmd)
		}
	}
}

func TestSysctlValuesAreValidated(t *testing.T) {
	for name, spec := range map[string]string{
		// A newline would persist whatever follows as another setting.
		"multi-line value": "  values:\n    vm.swappiness: \"10\\nkernel.core_pattern = |/tmp/x\"\n",
		"empty value":      "  values:\n    vm.swappiness: \"\"\n",
		"empty key":        "  values:\n    \"\": \"1\"\n",
		"conflicting key":  "  key: vm.swappiness\n  value: \"10\"\n  values:\n    vm.swappiness: \"60\"\n",
	} {
		if err := buildErr(t, "apiVersion: systemcd.dev/v1\nkind: Sysctl\nmetadata:\n  name: x\nspec:\n"+spec); err == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
	// A block scalar's trailing newline is harmless and still accepted.
	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Sysctl\nmetadata:\n  name: x\nspec:\n  key: vm.swappiness\n  value: |\n    10\n")
	h := host.NewMem()
	h.AddStub(host.CommandStub{Match: "sysctl -n vm.swappiness", Stdout: "10\n"})
	c := newContext(h)
	converge(t, r, c)
	assertConverged(t, r, c)
}

func TestSysctlDropInStaysInSysctlD(t *testing.T) {
	// The drop-in path is built from metadata.name, which the manifest
	// layer does not restrict: "a/../../../etc/cron.d/evil" resolved to
	// /etc/cron.d/evil.conf.
	err := buildErr(t, "apiVersion: systemcd.dev/v1\nkind: Sysctl\nmetadata:\n  name: a/../../../etc/cron.d/evil\nspec:\n  key: vm.swappiness\n  value: \"10\"\n")
	if err == nil || !strings.Contains(err.Error(), "outside /etc/sysctl.d") {
		t.Errorf("error = %v, want the name rejected", err)
	}
	// Without a drop-in the name is never used as a path.
	if err := buildErr(t, "apiVersion: systemcd.dev/v1\nkind: Sysctl\nmetadata:\n  name: a/b\nspec:\n  key: vm.swappiness\n  value: \"10\"\n  persist: false\n"); err != nil {
		t.Errorf("persist: false should not constrain the name: %v", err)
	}
}
