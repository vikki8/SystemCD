# SystemCD

**A lifecycle manager for Linux hosts.** Terraform reconciles your cloud API. Argo CD
reconciles the Kubernetes API. `systemcd` reconciles the third thing nobody
gave a controller to: **the host itself** — its systemd units, its files in
`/etc`, its packages, its users, its kernel parameters.

Same loop, same vocabulary:

| | desired state | reconciles against | drift shows up as |
|---|---|---|---|
| Terraform | `.tf` files | cloud provider API | `terraform plan` |
| Argo CD | manifests in git | Kubernetes API | `OutOfSync` in the UI |
| **systemcd** | **manifests in git** | **systemd, `/etc`, dpkg/rpm** | **`systemcd plan`, `OutOfSync`** |

```
git push  →  agent pulls  →  observe host  →  diff  →  converge  →  report
                  ↑                                                    │
                  └──────────────── every 5 minutes ───────────────────┘
```

---

## The gap this fills

Ansible can *put* a machine into a state. It cannot tell you the machine has
since drifted, and it does not run continuously. Its unit of work is a play
you launch, not a controller that holds a machine at a setpoint.

Kubernetes controllers hold things at a setpoint, but only inside a cluster.
The bastion host, the database VM, the appliance at the edge, the CI runner —
they get a bootstrap script and then years of `ssh` and hope.

`systemcd` gives those machines the Argo loop:

- **Continuous reconciliation.** An agent runs under systemd, pulls the repo,
  and converges. Not "when someone remembers to run the playbook".
- **Real drift detection.** Someone `vim`s `/etc/nginx/nginx.conf` at 3am;
  the next reconcile reports it as `OutOfSync` and — if self-heal is on — puts
  it back, with the old version saved in the backup directory.
- **Ownership you can query.** `systemcd owns /etc/nginx/nginx.conf` answers
  which resource is responsible, whether it was created or adopted, and when
  it last changed — the question you actually have at 3am.
- **Drift you can attribute.** "The repo moved ahead" and "someone edited the
  box" produce identical-looking diffs. systemcd tells them apart.
- **Prune.** Delete a resource from git and it's removed from the fleet. This
  needs a record of what was last applied, which is exactly what makes it a
  three-way merge rather than a two-way one.
- **A plan you can read before it happens.** `systemcd plan` exits `2` on
  drift, so it works as a monitoring check and as a CI gate.

## Install

```sh
go build -o systemcd ./cmd/systemcd
sudo install -m0755 systemcd /usr/local/bin/systemcd
```

Point it at a repo and let systemd own it:

```sh
sudo systemcd install --repo https://github.com/you/fleet.git --branch main --interval 5m
journalctl -u systemcd -f
```

That writes `/etc/systemd/system/systemcd.service` and enables it. The tool
that manages your services is itself a managed service.

## A repository

```
fleet/
├── systemcd.yaml           # repo settings: paths, prune, interval, self-heal
├── manifests/
│   ├── base.yaml           # everything every host gets
│   ├── nginx.yaml          # role=web
│   └── app.yaml            # your own service, as a systemd unit
└── files/
    └── nginx.conf          # payloads referenced by File.source
```

A working one lives in [`examples/`](examples/). Try it read-only:

```sh
systemcd plan -C examples --label role=web
```

## A manifest

```yaml
apiVersion: systemcd.dev/v1
kind: Package
metadata:
  name: nginx
  targets:
    labels: { role: web }        # only hosts labelled role=web
---
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: nginx-conf
spec:
  path: /etc/nginx/nginx.conf
  source: files/nginx.conf       # or inline `content:`
  mode: "0644"
  owner: root
dependsOn: [Package/nginx]       # ordering
notify:   [Service/nginx]        # config changed → reload nginx, once
---
apiVersion: systemcd.dev/v1
kind: Service
metadata:
  name: nginx
spec:
  enabled: true
  state: started
  reload: true                   # prefer reload-or-restart over restart
dependsOn: [Package/nginx]
```

`dependsOn` builds a DAG, topologically sorted with cycle detection. `notify`
is the handler mechanism: the notifier is *ordered before* its target, so the
new config is always on disk before the reload fires — and a service the same
run just started is not then also restarted.

## Resource kinds

| Kind | Manages | Notable spec fields |
|---|---|---|
| `File` | file contents, mode, ownership | `content` / `source`, `mode`, `owner`, `group`, `state`, `sensitive` |
| `Directory` | directories | `path`, `mode`, `owner`, `recursive` |
| `Package` | apt · dnf · yum · zypper · pacman · apk (auto-detected) | `name` / `names`, `version`†, `state`, `manager` |
| `Service` | systemd runtime state | `enabled`, `state`, `reload`, `scope` |
| `SystemdUnit` | unit files, with `daemon-reload` | `content` / `source`, `unit`, `directory` |
| `User` / `Group` | accounts | `uid`/`gid`, `home`, `shell`, `groups`, `system` |
| `Sysctl` | kernel parameters + `/etc/sysctl.d` drop-in | `key`/`value`, `values`, `persist` |
| `Exec` | escape hatch, guarded for idempotency | `command` / `argv`, `creates`, `unless`, `onlyIf`, `refreshOnly` |

`systemcd kinds` lists them at runtime.

**† `version` is only accepted where it can be honored.** apt, dnf, yum and
zypper can install an exact version, downgrade to it, and read the installed
version back in the same form the manifest wrote. pacman and apk cannot, so
`spec.version` is *rejected* there rather than silently accepted — a field
that sometimes converges and sometimes reports permanent drift is worse than
no field at all. `systemcd capabilities` tells you which host you're on:

```
$ systemcd capabilities
package manager:  apk
version pinning:  not supported
                  apk pins depend on the version still being present in the
                  configured repository, the installed version string does
                  not round-trip with what a manifest writes...
```

**On `User.groups`:** the shorthand is additive, because "I require docker
membership" is what people mean, not "I own this account's entire group list".
Opt into the stronger assertion explicitly:

```yaml
spec:
  groups: [docker]              # additive: other memberships are left alone
  # or
  groups:
    ensure: [docker, adm]
    mode: exact                 # anything else is drift and gets removed
```

**On `Exec`:** it refuses to build without a guard. Reconciliation only means
something if a resource can report whether it's already satisfied, so
`creates:`, `unless:`, `onlyIf:`, or `refreshOnly:` is mandatory — with
`always: true` as the explicit, visible opt-out.

## Commands

| Command | Does | Exit codes |
|---|---|---|
| `systemcd plan` | show what would change; touches nothing | `2` on drift |
| `systemcd apply` | converge the host | `1` on failure or degraded health |
| `systemcd status` | per-resource `Synced`/`OutOfSync` + health table | `2` on drift |
| `systemcd sync` | git pull, then apply | |
| `systemcd agent` | the reconcile loop, forever | |
| `systemcd rollback` | check out the previous applied revision and apply it | |
| `systemcd validate` | parse and type-check manifests; no host access | `1` on invalid |
| `systemcd inventory` | discover config this host has that systemcd does not manage | |
| `systemcd adopt` | take ownership of existing resources without changing them | |
| `systemcd release <ref>` | hand ownership back, preserving or restoring | |
| `systemcd pause` / `resume` | stop the agent converging, without stopping it | |
| `systemcd show <ref>` | ownership record for one resource | |
| `systemcd owns <path>` | which resource owns this path/unit/package/account | |
| `systemcd capabilities` | what systemcd can guarantee on this host | |
| `systemcd install` | write and enable systemcd's own unit | |

Every command takes `--json` for machine-readable output, `--only` to narrow
the run (`--only 'File/*'`, `--only Service/nginx`), and `--label k=v` to
override node labels.

`plan`'s exit code is the `terraform plan -detailed-exitcode` convention:
`0` in sync, `2` drift, `1` broken. Wire it into Nagios, a CI job, or a
`Restart=no` timer and you have fleet-wide drift alerting for free.

## Onboarding a fleet you inherited

Most tools assume you are starting from zero. Nobody who inherited a
five-year-old server is. The lifecycle here is built for the other case:

```
unmanaged ──inventory──> discovered ──adopt──> owned ──release──> unmanaged
                                                 │
                                                 └──prune──> deleted
```

**1. Find out what is actually there.** The package manager already knows
which of its own files somebody edited — `dpkg -V` and `rpm -Va` keep
checksums for exactly this, and almost nobody uses them:

```
$ systemcd inventory
modified-config (2)
──────────────────────────────────────────────
  path:/etc/nginx/nginx.conf  [nginx-common]
  path:/etc/ssh/sshd_config   [openssh-server]

unpackaged-config (1)
──────────────────────────────────────────────
  path:/etc/local-thing.conf
```

That first list is, literally, the changes nobody wrote down. Secrets
(`/etc/shadow`, host keys, `sudoers.d`) and churn (`.dpkg-old`, cert stores)
are excluded — an inventory that invites you to commit `/etc/shadow` is worse
than none. A scan that cannot run says so rather than reporting a clean
machine.

**2. Turn it into a starting point.**

```sh
systemcd inventory --generate ./fleet --generate-label role=app
```

Writes `manifests/discovered.yaml` plus the extracted file payloads under
`files/`. It is a snapshot of what *is*, not a claim about what *should be* —
turning one into the other is your judgement, not the tool's.

**3. Take ownership without changing anything.**

```
$ systemcd adopt -C ./fleet
@ File/etc-legacyapp-app-conf  adopt
    ownership recorded; the host was not modified
```

This is the move that makes migration politically possible. Day one does not
rewrite the machine. The current state is recorded as the baseline, so the
resource is owned and immediately in sync — and the *next* plan shows exactly
what the repository would change, attributed as a repository change because
the host still holds what systemcd recorded.

**4. Now make the first real change**, and review it as a diff rather than a
pair of hashes:

```
$ systemcd plan -C ./fleet
~ File/etc-legacyapp-app-conf  update
    content: +1 -1
        # edited by someone in 2019
      - workers = 2
      + workers = 16
        debug = true
```

**5. And back out if you need to.** A fleet tool without a migration-out path
is a trap, and teams are right not to adopt one:

```sh
systemcd release File/app-conf --preserve   # forget it; leave the host as-is
systemcd release File/app-conf --restore    # put back what was there in 2019
```

`--restore` works because adoption preserves the actual bytes under
`/var/lib/systemcd/baselines/`, not just a checksum. Kinds that cannot
honestly restore — a removed package cannot be un-removed to a version that
may no longer exist in any repository — say so and offer `--preserve` instead.

Releasing something the repository still declares is refused: the next
reconcile would take it straight back, so reporting it as released would be a
lie. Remove it from git first, or pass `--force`.

## Composition, not templating

Layering without a second programming language. A base declares the resource;
a role or a single host narrows it:

```yaml
kind: Patch
metadata:
  name: web01-hardening
  targets:
    hosts: ["web01"]
spec:
  target: File/nginx-conf
  patch:
    mode: "0600"          # merged over the base spec
    owner: null           # null removes a field — stop managing it
```

Maps merge key by key, so a patch sets one field without restating the
resource. Scalars and sequences **replace**: there is no defensible universal
merge for an unkeyed list, and quietly appending would make `groups: [docker]`
mean something different in a patch than in a base. `dependsOn` and `notify`
append, because a patch that narrows a resource must not silently drop
ordering the base established.

What this deliberately is not:

```yaml
content: "worker_processes {{ .cores }};"   # never
```

That is how every config tool grows a second, worse language, and how
operators stop being able to tell what is actually deployed. **Transform
structured data; do not interpolate strings.**

## Operating it

**Pause instead of stopping the agent.** A self-healing agent reverting an
operator's emergency change mid-incident is the failure mode that makes teams
turn continuous reconciliation off permanently:

```sh
systemcd pause --reason "INC-4471: nginx rolled back by hand" --for 2h
```

Drift is still detected and reported; nothing is converged. `--for` exists
because a pause that outlives its incident is how a fleet quietly stops being
managed. An explicit `systemcd apply` still works — that is human intent — but
warns.

**Drift becomes a Prometheus series with no new infrastructure.** The agent
writes node_exporter textfile metrics:

```
systemcd_external_drift 2
systemcd_out_of_sync 1
systemcd_paused 0
systemcd_last_reconcile_timestamp_seconds 1.7773e+09
systemcd_resource_out_of_sync{kind="File",name="nginx-conf",origin="external"} 1
```

`systemcd_external_drift` is the one worth alerting on: the repository did not
ask for those changes. Alert on a stale `last_reconcile_timestamp` too — a
host that stopped reconciling shows nothing at all in per-resource metrics.

## Host targeting

One repo, a heterogeneous fleet. Any document can be scoped:

```yaml
metadata:
  name: nginx
  targets:
    hosts: ["web-*", "edge-??.eu"]   # glob against hostname
    labels: { role: web, env: prod } # ANDed
```

Labels come from, in increasing precedence: the repo's `Config`,
`/etc/systemcd/node.yaml` on the machine, and `--label` on the command line.

## Ownership

The thing that determines whether you trust this on a production machine is
not the controllers — it's whether it can tell you what it owns. Every apply
writes an ownership record to `/var/lib/systemcd/state.json`.

**Every resource declares what it claims.** A `File` claims `path:/etc/...`,
a `Service` claims `unit:nginx.service`, a `Package` claims `package:nginx`.
Two resources claiming the same thing is refused *before* anything is
observed, let alone applied — last-writer-wins is not a property anyone can
reason about at 3am:

```
$ systemcd plan
systemcd: conflicting ownership:
  - File/nginx-conf and File/nginx-conf-override both claim
    path:/etc/nginx/nginx.conf (declared at base.yaml[2] and web.yaml[0])
```

`Exec` claims nothing, which is the honest description of an escape hatch —
and the reason it needs a guard.

**Adoption is recorded, not assumed.** A file systemcd created and a file it
took over from a distro package are different situations, and the second one
keeps a snapshot of what was there first:

```
$ systemcd show File/app-conf
File/app-conf
  owned by systemcd: yes
  ownership:         adopted (it already existed when systemcd first managed it)
  first applied:     2026-08-04 23:40:26 UTC
  last applied:      2026-08-04 23:44:11 UTC
  last changed:      2026-08-04 23:40:26 UTC
  revision:          a1b2c3d…
  claims:
    path:/opt/svc/app.conf
  state before systemcd took it over:
    checksum:    sha256:ec8b28ac…
```

**The reverse lookup is the one you need mid-incident:**

```
$ systemcd owns /etc/nginx/nginx.conf
path:/etc/nginx/nginx.conf
  owned by:      File/nginx-conf
  ownership:     created by systemcd
  last changed:  2026-08-04 23:40:26 UTC
  revision:      a1b2c3d…
```

**Last-applied and last-changed are different questions.** Confirming a
resource is already correct is not changing it, so `last changed` — and the
revision attached to it — stay truthful across a hundred no-op reconciles.

### Drift attribution

Because state records the *complete* desired state that was last applied, not
just the fields that differed, systemcd can answer a question two-way tools
cannot: **did the repository move, or did someone edit the box?**

```
$ systemcd plan
~ File/app-conf                update
    checksum: sha256:dd6a4f7b… -> sha256:05310cea…
    changed outside systemcd since 4m ago

1 resource(s) were changed outside systemcd since the last apply:
  File/app-conf (last applied 4m ago)
```

The diff looks identical in both cases. Only one of them is an incident. The
rule: if the host still holds every value systemcd wrote, the manifest is what
moved (`repo`); if it does not, something else edited the machine
(`external`). Both are in `--json` as `ownership.driftOrigin`.

### Orphans and prune

A resource systemcd owns that the repository no longer declares does not
vanish from view. It is reported as `Orphaned`:

```
RESOURCE          SYNC        HEALTH    OWNED   LAST CHANGED   DETAIL
File/app-extra    Orphaned    -         yes     3m ago         owned by systemcd but no longer declared…
```

An orphan is deliberately *not* "out of sync" — the repository isn't asking
for a change to it — so it doesn't muddy `plan`'s exit code. It's just no
longer invisible.

`--prune` acts on them, in reverse dependency order, by rebuilding each from
its stored manifest and asking it to delete itself. Two guard rails:

- **Irreversible prunes need `--confirm`.** Removing a package, deleting an
  account, or wiping a directory tree is not recoverable from systemcd's
  backups, so `--prune` alone withholds them and says so. Files are pruned
  freely because `Delete` backs them up first.
- **`--only` disables pruning and orphan reporting entirely.** Ownership
  cannot be judged from a partial view; if prune ran under `--only` it would
  delete most of the machine.

### State storage

`/var/lib/systemcd/state.json`, mode `0600`, is the trust anchor, so it is
treated like one:

- **Written atomically with a directory fsync**, so the rename that publishes
  it survives a power loss — not just the file contents.
- **The previous copy is rotated to `.bak` first.** A corrupt or truncated
  state file recovers from the backup with a loud warning rather than either
  failing the run or silently forgetting every ownership record on the box.
- **Schema-versioned with forward refusal.** State written by a newer
  systemcd is refused, not misinterpreted; older state migrates in place.
- **A `flock` serializes reconciles.** A hand-run `systemcd apply` and the
  agent converging the same machine at once would interleave writes; the
  second one is turned away with an explanation. Read-only `plan` and
  `status` never take the lock, so you can always inspect a busy box.

## Safety properties

- **Plans never mutate.** `DryRun` is threaded to every provider; the tests
  assert the host is untouched and no state is written.
- **Files are replaced atomically** — write to a temp file in the same
  directory, `fsync`, `rename` — so no reader ever sees a half-written config.
- **Overwrites are backed up** to `/var/lib/systemcd/backups/` first.
- **A failure blocks its dependents, not the whole run.** If the package
  install fails, its config and service are marked `skipped` with the reason,
  while unrelated branches still converge.
- **Post-apply health checks.** A service that comes back `failed` is reported
  `Degraded` with the journal tail, and the command exits non-zero.
- **`sensitive: true`** keeps a file's hash and diff out of plan output.

## Design notes

**Why Go, one static binary.** The machines that need this most are the ones
you don't want to install a Python or Ruby runtime on. `scp` one file, done.

**Why shell out to `git` and `systemctl`.** The host already has git
configured with the operator's credentials, SSH agent, and proxy settings;
inheriting that is a feature, not a shortcut. Same for `systemctl` — no
reimplementation of the D-Bus API to drift out of date.

**Why everything goes through a `Host` interface.** Providers never touch
`os.*` directly. That's what lets the whole resource layer be tested against
an in-memory host — real filesystem semantics, scripted command responses,
every executed command recorded — with no root and no container.

```go
type Host interface {
    Run(ctx context.Context, name string, args ...string) (Result, error)
    ReadFile(path string) ([]byte, error)
    WriteFile(path string, data []byte, mode fs.FileMode) error
    Stat(path string) (FileInfo, error)
    // ...
}
```

**Adding a kind** is one file: implement `Desired`, `Observe`, and `Apply`
over a flat `map[string]string`, then `Register("Kind", build)`. The diffing,
ordering, planning, reporting, state recording, and pruning all come for free.
Optional interfaces opt into more: `Refreshable` for `notify` handlers,
`Deletable` for prune, `Checker` for health.

## Status

Working and covered by tests (`go test -race ./...`): the reconcile loop, nine
resource kinds, host targeting, ownership and claims, drift attribution,
orphans, prune, adopt/release, inventory and manifest generation, structural
patches, content diffs, pause, metrics, rollback, locking, and the agent.

Not there yet:

- **Secret backends.** `sensitive: true` keeps values out of output and state,
  but the value still lives in git. Real secret references are the next gap.
- **A fleet-wide view.** Everything here is per-host by design; correlating
  400 machines currently means scraping the metrics or the `--json`.
- **`Timer`, `Mount`, `Firewall` kinds**, and `Exec` health verification.
- **Constraint-based package versions** (`>=1.24,<1.25`) rather than exact
  pins, on the managers that can honour them.

## License

MIT
