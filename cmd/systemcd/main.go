// Command systemcd reconciles a Linux host against declarative manifests
// kept in git — the same GitOps loop Argo CD runs for Kubernetes, aimed at
// systemd units, config files, packages, users and kernel parameters.
package main

import (
	"os"

	"github.com/vikki8/systemcd/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
