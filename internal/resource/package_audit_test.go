package resource

import (
	"context"
	"strings"
	"testing"

	"github.com/vikki8/systemcd/internal/host"
)

// scriptedHost wraps MemHost so a test can compute a command's output from
// its arguments, which a static stub cannot. Every command is still recorded
// by the MemHost.
type scriptedHost struct {
	*host.MemHost
	respond func(name string, args []string) (host.Result, bool)
}

func (s *scriptedHost) Run(ctx context.Context, name string, args ...string) (host.Result, error) {
	res, err := s.MemHost.Run(ctx, name, args...)
	if s.respond != nil {
		if scripted, ok := s.respond(name, args); ok {
			return scripted, nil
		}
	}
	return res, err
}

func (s *scriptedHost) RunShell(ctx context.Context, script string) (host.Result, error) {
	return s.Run(ctx, "/bin/sh", "-c", script)
}

// pkgInstance is one installed instance of a package. For rpm an unset epoch
// is "(none)", which is what `rpm -q --queryformat %{EPOCH}` prints.
type pkgInstance struct{ status, epoch, version, release, arch string }

// fakeRPM answers `rpm -q --queryformat FMT pkg` the way rpm does: FMT is
// rendered once per installed instance and nothing is appended, so a format
// without "\n" runs instances together exactly as the real tool does.
func fakeRPM(instances func() []pkgInstance) func(string, []string) (host.Result, bool) {
	return func(name string, args []string) (host.Result, bool) {
		if name != "rpm" || len(args) == 0 || args[0] != "-q" {
			return host.Result{}, false
		}
		format := ""
		for i, a := range args {
			if a == "--queryformat" && i+1 < len(args) {
				format = args[i+1]
			}
		}
		insts := instances()
		if len(insts) == 0 {
			return host.Result{Stdout: "package " + args[len(args)-1] + " is not installed\n", ExitCode: 1}, true
		}
		var b strings.Builder
		for _, in := range insts {
			b.WriteString(strings.NewReplacer(
				"%{EPOCH}", in.epoch, "%{VERSION}", in.version,
				"%{RELEASE}", in.release, "%{ARCH}", in.arch,
			).Replace(format))
		}
		return host.Result{Stdout: b.String()}, true
	}
}

// fakeDpkgQuery answers `dpkg-query -W -f=FMT pkg` the same way.
func fakeDpkgQuery(instances ...pkgInstance) func(string, []string) (host.Result, bool) {
	return func(name string, args []string) (host.Result, bool) {
		if name != "dpkg-query" || len(args) == 0 || args[0] != "-W" {
			return host.Result{}, false
		}
		format := ""
		for _, a := range args {
			if strings.HasPrefix(a, "-f=") {
				format = strings.TrimPrefix(a, "-f=")
			}
		}
		var b strings.Builder
		for _, in := range instances {
			b.WriteString(strings.NewReplacer("${db:Status-Status}", in.status, "${Version}", in.version).Replace(format))
		}
		return host.Result{Stdout: b.String()}, true
	}
}

func TestPackageAptPinnedInstallCanDowngrade(t *testing.T) {
	// apt-get -y refuses to downgrade without --allow-downgrades ("E: Packages
	// were downgraded and -y was used without --allow-downgrades"), so a pin
	// older than the installed version could never converge.
	h := host.NewMem()
	h.Binaries["apt-get"] = true
	h.AddStub(host.CommandStub{Match: "dpkg-query", Stdout: "installed 1.26.0-1\n"})
	c := newContext(h)

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: Package
metadata:
  name: nginx
spec:
  version: "1.24.0-2ubuntu7"
`)
	converge(t, r, c)
	if !h.Ran("--allow-downgrades 'nginx=1.24.0-2ubuntu7'") {
		t.Errorf("a pinned apt install must allow the downgrade; commands = %v", h.Commands)
	}
}

func TestPackageAptUnpinnedInstallDoesNotAllowDowngrades(t *testing.T) {
	h := host.NewMem()
	h.Binaries["apt-get"] = true
	h.AddStub(host.CommandStub{Match: "dpkg-query", ExitCode: 1})
	c := newContext(h)

	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Package\nmetadata:\n  name: nginx\n")
	converge(t, r, c)
	if !h.Ran("apt-get install") || h.Ran("--allow-downgrades") {
		t.Errorf("an unpinned install should not permit downgrades; commands = %v", h.Commands)
	}
}

func TestPackageAptInstallKeepsModifiedConffiles(t *testing.T) {
	// systemcd edits the config files that packages ship. Upgrading such a
	// package makes dpkg ask what to do with the conffile, and with no
	// terminal apt fails: "end of file on stdin at conffile prompt".
	h := host.NewMem()
	h.Binaries["apt-get"] = true
	h.AddStub(host.CommandStub{Match: "dpkg-query", ExitCode: 1})
	c := newContext(h)

	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Package\nmetadata:\n  name: nginx\n")
	converge(t, r, c)
	for _, want := range []string{"Dpkg::Options::=--force-confdef", "Dpkg::Options::=--force-confold"} {
		if !h.Ran(want) {
			t.Errorf("apt install should pass %s; commands = %v", want, h.Commands)
		}
	}
}

func TestPackageManagerFailureNamesTheTool(t *testing.T) {
	h := host.NewMem()
	h.Binaries["apt-get"] = true
	h.AddStub(host.CommandStub{Match: "apt-get install", ExitCode: 100, Stderr: "E: Unable to locate package nginx\n"})
	c := newContext(h)

	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Package\nmetadata:\n  name: nginx\n")
	err := r.Apply(c, Diff{{Field: "pkg:nginx", Have: Absent, Want: Present}})
	if err == nil || !strings.HasPrefix(err.Error(), "apt-get: exit 100") {
		t.Fatalf("error = %v, want it to name apt-get rather than the DEBIAN_FRONTEND assignment", err)
	}
}

func TestPackageNamesAreValidated(t *testing.T) {
	for _, bad := range []string{
		"-oAPT::Get::AllowUnauthenticated=true", // read by apt-get as an option
		"--allow-remove-essential",
		"nginx=1.24.0", // a version in the name never reads back as installed
		"nginx>1.2",
		"@development-tools", // a dnf group is not a package
		"nginx*",
		"nginx bash",
		"../etc",
		"",
	} {
		err := buildErr(t, "apiVersion: systemcd.dev/v1\nkind: Package\nmetadata:\n  name: p\nspec:\n  names: [\""+bad+"\"]\n")
		if err == nil || !strings.Contains(err.Error(), "not valid") {
			t.Errorf("name %q: error = %v, want a validation failure", bad, err)
		}
	}
	for _, good := range []string{"nginx", "libc6:i386", "g++", "gcc-c++", "python3.11", "NetworkManager", "perl-Data-Dumper", "libstdc++6"} {
		if err := buildErr(t, "apiVersion: systemcd.dev/v1\nkind: Package\nmetadata:\n  name: p\nspec:\n  names: [\""+good+"\"]\n"); err != nil {
			t.Errorf("name %q should be accepted: %v", good, err)
		}
	}
	// The resource name is the package name when the spec omits one.
	if err := buildErr(t, "apiVersion: systemcd.dev/v1\nkind: Package\nmetadata:\n  name: \"-y\"\n"); err == nil {
		t.Error("a metadata.name used as the package name must be validated too")
	}
}

func TestPackageRepeatedNamesClaimOnce(t *testing.T) {
	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: Package
metadata:
  name: web
spec:
  name: nginx
  names: [nginx, curl, curl]
`)
	claims := ClaimStrings(ClaimsOf(r))
	if strings.Join(claims, " ") != "package:curl package:nginx" {
		t.Errorf("claims = %v, want each package once (a duplicate conflicts with itself)", claims)
	}
}

func TestPackageDpkgMultiArchPinConverges(t *testing.T) {
	// A Multi-Arch: same package installed for two architectures matches
	// twice. Without a newline in the format the two entries run together
	// and the version reads as "2.39-0ubuntu8installed".
	h := &scriptedHost{MemHost: host.NewMem()}
	h.Binaries["apt-get"] = true
	h.respond = fakeDpkgQuery(
		pkgInstance{status: "installed", version: "2.39-0ubuntu8"},
		pkgInstance{status: "installed", version: "2.39-0ubuntu8"},
	)
	c := newContext(h)

	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Package\nmetadata:\n  name: libc6\nspec:\n  version: \"2.39-0ubuntu8\"\n")
	assertConverged(t, r, c)
}

func TestPackageDpkgStatuses(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   string
	}{
		{"installed", Present},
		{"triggers-pending", Present},
		{"triggers-awaited", Present},
		{"config-files", Absent}, // removed, only its conffiles remain
		{"half-installed", Absent},
		{"unpacked", Absent},
		{"half-configured", Absent},
		{"not-installed", Absent},
	} {
		h := &scriptedHost{MemHost: host.NewMem()}
		h.Binaries["apt-get"] = true
		h.respond = fakeDpkgQuery(pkgInstance{status: tc.status, version: "1.0-1"})
		c := newContext(h)
		r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Package\nmetadata:\n  name: nginx\n")
		observed, err := r.Observe(c)
		if err != nil {
			t.Fatal(err)
		}
		if got := observed["pkg:nginx"]; got != tc.want {
			t.Errorf("dpkg status %q observed as %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestPackageRPMPinRoundTripsInTheFormWritten(t *testing.T) {
	// The README promises that dnf/yum/zypper read the installed version back
	// in the same form the manifest wrote. rpm's EVR has an optional epoch,
	// and dnf accepts a pin with or without release and arch.
	installed := []pkgInstance{{epoch: "1", version: "1.20.1", release: "14.el9", arch: "x86_64"}}
	for _, pin := range []string{"1.20.1", "1.20.1-14.el9", "1:1.20.1-14.el9", "1.20.1-14.el9.x86_64", "1:1.20.1-14.el9.x86_64"} {
		t.Run(pin, func(t *testing.T) {
			h := &scriptedHost{MemHost: host.NewMem()}
			h.Binaries["dnf"] = true
			h.respond = fakeRPM(func() []pkgInstance { return installed })
			c := newContext(h)
			r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Package\nmetadata:\n  name: nginx\nspec:\n  version: \""+pin+"\"\n")
			assertConverged(t, r, c)
		})
	}

	t.Run("a different version is still drift", func(t *testing.T) {
		h := &scriptedHost{MemHost: host.NewMem()}
		h.Binaries["dnf"] = true
		h.respond = fakeRPM(func() []pkgInstance { return installed })
		c := newContext(h)
		r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Package\nmetadata:\n  name: nginx\nspec:\n  version: \"1:1.20.1-15.el9\"\n")
		desired, _ := r.Desired(c)
		observed, err := r.Observe(c)
		if err != nil {
			t.Fatal(err)
		}
		if Compare(desired, observed, nil).Empty() {
			t.Error("an installed 1:1.20.1-14.el9 must not satisfy a pin on 1:1.20.1-15.el9")
		}
	})

	t.Run("an explicit zero epoch matches a package without one", func(t *testing.T) {
		h := &scriptedHost{MemHost: host.NewMem()}
		h.Binaries["dnf"] = true
		h.respond = fakeRPM(func() []pkgInstance {
			return []pkgInstance{{epoch: "(none)", version: "2.4", release: "1.el9", arch: "noarch"}}
		})
		c := newContext(h)
		r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Package\nmetadata:\n  name: tzdata\nspec:\n  version: \"0:2.4-1.el9\"\n")
		assertConverged(t, r, c)
	})
}

func TestPackageRPMMultilibPinConverges(t *testing.T) {
	// glibc.x86_64 and glibc.i686 side by side: rpm prints the format once
	// per instance, so without a newline the version reads as
	// "2.34-60.el92.34-60.el9" and never matches.
	h := &scriptedHost{MemHost: host.NewMem()}
	h.Binaries["dnf"] = true
	h.respond = fakeRPM(func() []pkgInstance {
		return []pkgInstance{
			{epoch: "(none)", version: "2.34", release: "60.el9", arch: "x86_64"},
			{epoch: "(none)", version: "2.34", release: "60.el9", arch: "i686"},
		}
	})
	c := newContext(h)
	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Package\nmetadata:\n  name: glibc\nspec:\n  version: \"2.34-60.el9\"\n")
	assertConverged(t, r, c)
}

func TestPackageZypperPinnedInstallAllowsOlderPackage(t *testing.T) {
	// zypper will not replace a newer installed version with an older one
	// unless told --oldpackage.
	h := &scriptedHost{MemHost: host.NewMem()}
	h.Binaries["zypper"] = true
	h.respond = fakeRPM(func() []pkgInstance {
		return []pkgInstance{{epoch: "(none)", version: "1.26.0", release: "1.1", arch: "x86_64"}}
	})
	c := newContext(h)
	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Package\nmetadata:\n  name: nginx\nspec:\n  version: \"1.24.0-1.1\"\n")
	converge(t, r, c)
	if !h.Ran("zypper --non-interactive install --oldpackage 'nginx-1.24.0-1.1'") {
		t.Errorf("commands = %v", h.Commands)
	}
}

func TestPackageYumPinnedDowngradeConverges(t *testing.T) {
	// yum 3 answers `yum install pkg-<older>` with "Nothing to do" and exit
	// 0, so the pin would report success and drift forever.
	h := &scriptedHost{MemHost: host.NewMem()}
	h.Binaries["yum"] = true
	current := pkgInstance{epoch: "(none)", version: "1.20.1", release: "10.el7", arch: "x86_64"}
	h.respond = fakeRPM(func() []pkgInstance { return []pkgInstance{current} })
	h.AddStub(host.CommandStub{Match: "yum install", Stdout: "Package matching nginx-1.20.1-9.el7.x86_64 already installed. Checking for update.\nNothing to do\n"})
	h.AddStub(host.CommandStub{Match: "yum downgrade -y 'nginx-1.20.1-9.el7'", Do: func(*host.MemHost, []string) {
		current.release = "9.el7"
	}})
	c := newContext(h)

	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Package\nmetadata:\n  name: nginx\nspec:\n  version: \"1.20.1-9.el7\"\n")
	converge(t, r, c)
	if !h.Ran("yum downgrade") {
		t.Fatalf("expected a yum downgrade; commands = %v", h.Commands)
	}
	assertConverged(t, r, c)
}

func TestPackageYumPinnedUpgradeDoesNotDowngrade(t *testing.T) {
	h := &scriptedHost{MemHost: host.NewMem()}
	h.Binaries["yum"] = true
	current := pkgInstance{epoch: "(none)", version: "1.20.1", release: "9.el7", arch: "x86_64"}
	h.respond = fakeRPM(func() []pkgInstance { return []pkgInstance{current} })
	h.AddStub(host.CommandStub{Match: "yum install", Do: func(*host.MemHost, []string) { current.release = "10.el7" }})
	c := newContext(h)

	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Package\nmetadata:\n  name: nginx\nspec:\n  version: \"1.20.1-10.el7\"\n")
	converge(t, r, c)
	if h.Ran("yum downgrade") {
		t.Errorf("install reached the pin; no downgrade should run; commands = %v", h.Commands)
	}
}

func TestPackageDeleteSkipsPackagesAlreadyGone(t *testing.T) {
	// pacman -R and zypper remove fail on a package that is not installed,
	// so pruning a resource whose packages someone already removed would
	// fail on every run.
	h := host.NewMem()
	h.Binaries["pacman"] = true
	h.AddStub(host.CommandStub{Match: "pacman -Q htop", Stdout: "htop 3.3.0-1\n"})
	h.AddStub(host.CommandStub{Match: "pacman -Q", ExitCode: 1, Stderr: "error: package 'strace' was not found\n"})
	// Real pacman: removing only htop works, naming strace fails the lot.
	h.AddStub(host.CommandStub{Match: "pacman -R --noconfirm 'htop'"})
	h.AddStub(host.CommandStub{Match: "pacman -R", ExitCode: 1, Stderr: "error: target not found: strace\n"})
	c := newContext(h)

	r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: Package\nmetadata:\n  name: tools\nspec:\n  names: [htop, strace]\n")
	if err := r.(Deletable).Delete(c); err != nil {
		t.Fatalf("delete: %v (commands = %v)", err, h.Commands)
	}
	if !h.Ran("pacman -R --noconfirm 'htop'") {
		t.Errorf("the installed package should still be removed; commands = %v", h.Commands)
	}

	h2 := host.NewMem()
	h2.Binaries["pacman"] = true
	h2.AddStub(host.CommandStub{Match: "pacman -Q", ExitCode: 1})
	if err := r.(Deletable).Delete(newContext(h2)); err != nil {
		t.Fatalf("delete with nothing installed: %v", err)
	}
	if h2.Ran("pacman -R") {
		t.Errorf("nothing is installed, so nothing should be removed; commands = %v", h2.Commands)
	}
}
