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

// BackupDir holds copies of files replaced by a File resource.
var BackupDir = path.Join(DataDir, "backups")

// RepoDir is where `sync` and `agent` clone the desired-state repository.
var RepoDir = path.Join(DataDir, "repo")

// NodeConfigFile optionally supplies node labels used for host targeting.
var NodeConfigFile = path.Join(ConfigDir, "node.yaml")

// UnitFile is the systemd unit installed by `systemcd install`.
const UnitFile = "/etc/systemd/system/systemcd.service"
