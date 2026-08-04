# SystemCD

**GitOps for Linux servers.** Terraform reconciles your cloud API. Argo CD
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
| `Package` | apt · dnf · yum · zypper · pacman · apk (auto-detected) | `name` / `names`, `version`, `state`, `manager` |
| `Service` | systemd runtime state | `enabled`, `state`, `reload`, `scope` |
| `SystemdUnit` | unit files, with `daemon-reload` | `content` / `source`, `unit`, `directory` |
| `User` / `Group` | accounts | `uid`/`gid`, `home`, `shell`, `groups`, `system` |
| `Sysctl` | kernel parameters + `/etc/sysctl.d` drop-in | `key`/`value`, `values`, `persist` |
| `Exec` | escape hatch, guarded for idempotency | `command` / `argv`, `creates`, `unless`, `onlyIf`, `refreshOnly` |

`systemcd kinds` lists them at runtime.

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
| `systemcd install` | write and enable systemcd's own unit | |

Every command takes `--json` for machine-readable output, `--only` to narrow
the run (`--only 'File/*'`, `--only Service/nginx`), and `--label k=v` to
override node labels.

`plan`'s exit code is the `terraform plan -detailed-exitcode` convention:
`0` in sync, `2` drift, `1` broken. Wire it into Nagios, a CI job, or a
`Restart=no` timer and you have fleet-wide drift alerting for free.

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

## How drift and prune actually work

Every apply records what it applied to `/var/lib/systemcd/state.json`,
including the manifest that produced each resource. That third copy is what
makes the difference:

- **Two-way** (git ⟂ host) tells you a file's contents are wrong.
- **Three-way** (git ⟂ host ⟂ last-applied) *also* tells you a resource used
  to be managed and has since been deleted from git — so `--prune` can remove
  it, in reverse dependency order, by rebuilding it from the stored manifest
  and asking it to delete itself.

Prune is opt-in. Removing a line from a YAML file should not silently
uninstall a package on 400 machines unless you asked for that.

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

The reconcile loop, the nine resource kinds, targeting, prune, rollback, and
the agent all work and are covered by tests (`go test ./...`).

Not there yet: templating in manifests (deliberately deferred — Helm's lesson
is that string-templated YAML gets ugly fast), secret backends, a fleet-wide
dashboard, and `Timer`/`Mount`/`Firewall` kinds.

## License

MIT
