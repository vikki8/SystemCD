package resource

import (
	"strings"
	"testing"

	"github.com/vikki8/systemcd/internal/host"
)

func TestAccountNamesTheToolsWouldMisreadAreRejected(t *testing.T) {
	// `useradd … -h` prints its help and exits 0, so a User named "-h" would
	// be reported as created without existing; "-D" rewrites useradd's
	// defaults. An all-digit name makes getent look up a uid instead.
	cases := []struct{ name, src string }{
		{"leading dash user", "kind: User\nmetadata:\n  name: \"-h\"\n"},
		{"leading dash spec.user", "kind: User\nmetadata:\n  name: ok\nspec:\n  user: \"-D\"\n"},
		{"numeric user", "kind: User\nmetadata:\n  name: \"1000\"\n"},
		{"nis user", "kind: User\nmetadata:\n  name: \"+alice\"\n"},
		{"space in user", "kind: User\nmetadata:\n  name: ok\nspec:\n  user: \"a b\"\n"},
		{"colon in user", "kind: User\nmetadata:\n  name: ok\nspec:\n  user: \"a:b\"\n"},
		{"leading dash group", "kind: Group\nmetadata:\n  name: \"-g\"\n"},
		{"numeric group", "kind: Group\nmetadata:\n  name: ok\nspec:\n  group: \"27\"\n"},
		{"comma in ensure", "kind: User\nmetadata:\n  name: ok\nspec:\n  groups: [\"docker,sudo\"]\n"},
		{"dash in ensure", "kind: User\nmetadata:\n  name: ok\nspec:\n  groups: [\"-x\"]\n"},
		{"numeric in ensure", "kind: User\nmetadata:\n  name: ok\nspec:\n  groups:\n    ensure: [\"27\"]\n    mode: exact\n"},
		{"empty in ensure", "kind: User\nmetadata:\n  name: ok\nspec:\n  groups: [\"\"]\n"},
		{"dash primary group", "kind: User\nmetadata:\n  name: ok\nspec:\n  group: \"-x\"\n"},
		{"negative uid", "kind: User\nmetadata:\n  name: ok\nspec:\n  uid: -1\n"},
		{"uid -1 as unsigned", "kind: User\nmetadata:\n  name: ok\nspec:\n  uid: 4294967295\n"},
		{"negative gid", "kind: Group\nmetadata:\n  name: ok\nspec:\n  gid: -5\n"},
		{"relative home", "kind: User\nmetadata:\n  name: ok\nspec:\n  home: home/ok\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := buildErr(t, "apiVersion: systemcd.dev/v1\n"+tc.src); err == nil {
				t.Fatal("accepted; want a validation error before any host access")
			}
		})
	}

	// Names the Debian/Ubuntu tools accept, and a numeric primary gid, stay valid.
	for _, src := range []string{
		"kind: User\nmetadata:\n  name: ok\nspec:\n  user: Svc.Account_1$\n",
		"kind: User\nmetadata:\n  name: ok\nspec:\n  group: \"27\"\n",
		"kind: Group\nmetadata:\n  name: systemd-journal\n",
	} {
		if err := buildErr(t, "apiVersion: systemcd.dev/v1\n"+src); err != nil {
			t.Errorf("%q rejected: %v", src, err)
		}
	}
}

func TestUserGroupsMappingRejectsUnknownKeys(t *testing.T) {
	// A typo in the compliance form must not silently fall back to additive.
	err := buildErr(t, `
apiVersion: systemcd.dev/v1
kind: User
metadata:
  name: deploy
spec:
  groups:
    ensure: [docker]
    mod: exact
`)
	if err == nil || !strings.Contains(err.Error(), "mod") {
		t.Fatalf("error = %v, want the unknown key named", err)
	}
}

func TestUserPrimaryGroupIsReconciled(t *testing.T) {
	h := host.NewMem()
	h.AddStub(host.CommandStub{Match: "getent passwd deploy", Stdout: "deploy:x:997:100::/var/lib/deploy:/usr/sbin/nologin"})
	h.AddStub(host.CommandStub{Match: "getent group deploy", Stdout: "deploy:x:997:"})
	h.AddStub(host.CommandStub{Match: "getent group 100", Stdout: "users:x:100:"})
	c := newContext(h)

	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: User
metadata:
  name: deploy
spec:
  group: deploy
`)
	d := converge(t, r, c)
	if !d.Has("group") {
		t.Fatalf("a wrong primary group must be drift, got %+v", d)
	}
	if !h.Ran("usermod --gid deploy deploy") {
		t.Errorf("commands = %v", h.Commands)
	}
}

func TestUserPrimaryGroupInSyncByNameOrID(t *testing.T) {
	for _, group := range []string{"deploy", "997"} {
		h := host.NewMem()
		h.AddStub(host.CommandStub{Match: "getent passwd deploy", Stdout: "deploy:x:997:997::/var/lib/deploy:/usr/sbin/nologin"})
		h.AddStub(host.CommandStub{Match: "getent group deploy", Stdout: "deploy:x:997:"})
		h.AddStub(host.CommandStub{Match: "getent group 997", Stdout: "deploy:x:997:"})
		r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: User
metadata:
  name: deploy
spec:
  group: "`+group+`"
`)
		assertConverged(t, r, newContext(h))
	}
}

func TestUserGroupsSurviveAPrimaryGIDWithNoName(t *testing.T) {
	// `id -nG` exits 1 when a gid has no name, yet still prints the list.
	// Treating that as "no groups" meant a usermod on every run, forever.
	for _, groups := range []string{"[docker]", "\n    ensure: [docker]\n    mode: exact"} {
		h := host.NewMem()
		h.AddStub(host.CommandStub{Match: "getent passwd deploy", Stdout: "deploy:x:997:4325::/var/lib/deploy:/usr/sbin/nologin"})
		h.AddStub(host.CommandStub{Match: "id -nG deploy", Stdout: "4325 docker\n", Stderr: "id: cannot find name for group ID 4325\n", ExitCode: 1})
		h.AddStub(host.CommandStub{Match: "getent group 4325", ExitCode: 2})
		r := buildFrom(t, "apiVersion: systemcd.dev/v1\nkind: User\nmetadata:\n  name: deploy\nspec:\n  groups: "+groups+"\n")
		assertConverged(t, r, newContext(h))
	}
}

func TestUserGroupsExactWithPrimaryGroupListed(t *testing.T) {
	// id prints the primary group once even when the account is also a
	// member of it, so listing it in an exact set must not be permanent drift.
	h := host.NewMem()
	h.AddStub(host.CommandStub{Match: "getent passwd deploy", Stdout: "deploy:x:997:997::/var/lib/deploy:/usr/sbin/nologin"})
	h.AddStub(host.CommandStub{Match: "getent group 997", Stdout: "deploy:x:997:deploy"})
	h.AddStub(host.CommandStub{Match: "id -nG deploy", Stdout: "deploy docker"})
	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: User
metadata:
  name: deploy
spec:
  groups:
    ensure: [deploy, docker]
    mode: exact
`)
	assertConverged(t, r, newContext(h))
}

func TestUserGroupsExactIgnoresRepeatedNames(t *testing.T) {
	h := host.NewMem()
	h.AddStub(host.CommandStub{Match: "getent passwd deploy", Stdout: "deploy:x:997:997::/var/lib/deploy:/usr/sbin/nologin"})
	h.AddStub(host.CommandStub{Match: "getent group 997", Stdout: "deploy:x:997:"})
	h.AddStub(host.CommandStub{Match: "id -nG deploy", Stdout: "deploy docker"})
	r := buildFrom(t, `
apiVersion: systemcd.dev/v1
kind: User
metadata:
  name: deploy
spec:
  groups:
    ensure: [docker, docker]
    mode: exact
`)
	assertConverged(t, r, newContext(h))
}
