// Package paths centralizes the on-host locations systemcd owns.
package paths

import "path"

// DataDir holds all mutable state systemcd keeps between runs.
const DataDir = "/var/lib/systemcd"

// ConfigDir holds node-local configuration such as labels.
const ConfigDir = "/etc/systemcd"

// StateFile records the last successfully applied state, which is what makes
// three-way reconciliation (and therefore pruning) possible.
var StateFile = path.Join(DataDir, "state.json")

// BackupDir holds copies of files replaced or removed by a File resource.
var BackupDir = path.Join(DataDir, "backups")

// LockFile serializes reconciles so an operator running `systemcd apply` by
// hand cannot interleave with the agent's loop.
var LockFile = path.Join(DataDir, "lock")

// RepoDir is where `sync` and `agent` clone the desired-state repository.
var RepoDir = path.Join(DataDir, "repo")

// NodeConfigFile optionally supplies node labels used for host targeting.
var NodeConfigFile = path.Join(ConfigDir, "node.yaml")

// UnitFile is the systemd unit installed by `systemcd install`.
const UnitFile = "/etc/systemd/system/systemcd.service"
