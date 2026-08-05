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

// BaselineDir holds the exact contents a resource had at the moment systemcd
// adopted it. Without this, "give ownership back the way you found it" is a
// promise the tool could not keep: a checksum records that something changed,
// not what it used to be.
var BaselineDir = path.Join(DataDir, "baselines")

// PauseFile, when present, stops the agent from converging. During an
// incident, a self-healing agent reverting an operator's emergency change is
// the last thing anyone needs.
var PauseFile = path.Join(DataDir, "paused")

// MetricsFile is a Prometheus textfile-collector export, so drift becomes a
// time series without deploying anything new.
var MetricsFile = path.Join(DataDir, "systemcd.prom")

// RepoDir is where `sync` and `agent` clone the desired-state repository.
var RepoDir = path.Join(DataDir, "repo")

// NodeConfigFile optionally supplies node labels used for host targeting.
var NodeConfigFile = path.Join(ConfigDir, "node.yaml")

// UnitFile is the systemd unit installed by `systemcd install`.
const UnitFile = "/etc/systemd/system/systemcd.service"
