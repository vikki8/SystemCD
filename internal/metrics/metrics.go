// Package metrics exports reconcile results as Prometheus textfile metrics.
//
// Drift is only useful if someone finds out about it. Rather than requiring a
// server, an API, or an agent-to-control-plane protocol, systemcd writes a
// file that node_exporter's textfile collector already reads on most fleets.
// Fleet-wide drift alerting then costs one alerting rule and no new
// infrastructure.
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vikki8/systemcd/internal/engine"
	"github.com/vikki8/systemcd/internal/host"
)

// Render turns a report into Prometheus exposition format.
func Render(rep *engine.Report, paused bool) []byte {
	c := rep.Counts()
	var b strings.Builder

	metric := func(name, help, typ string, value any, labels ...string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		label := ""
		if len(labels) > 0 {
			label = "{" + strings.Join(labels, ",") + "}"
		}
		fmt.Fprintf(&b, "%s%s %v\n", name, label, value)
	}

	metric("systemcd_last_reconcile_timestamp_seconds",
		"Unix time of the last completed reconcile.", "gauge",
		rep.Finished.Unix())
	metric("systemcd_reconcile_duration_seconds",
		"Duration of the last reconcile.", "gauge",
		fmt.Sprintf("%.3f", rep.Finished.Sub(rep.Started).Seconds()))
	metric("systemcd_resources_total",
		"Resources evaluated in the last reconcile.", "gauge", c.Total)
	metric("systemcd_resources_in_sync",
		"Resources already matching the repository.", "gauge", c.InSync)
	metric("systemcd_resources_changed",
		"Resources changed by the last reconcile.", "gauge", c.Changed)
	metric("systemcd_resources_failed",
		"Resources that failed to converge.", "gauge", c.Failed)
	metric("systemcd_resources_degraded",
		"Resources whose post-apply health check failed.", "gauge", c.Degraded)
	metric("systemcd_resources_orphaned",
		"Resources systemcd owns that the repository no longer declares.", "gauge", c.Orphaned)

	// The metric worth alerting on. Repository changes are expected; a host
	// edited outside systemcd is not, and the distinction is the reason this
	// series is worth more than a generic "config drift" counter.
	metric("systemcd_external_drift",
		"Resources changed outside systemcd since the last apply.", "gauge", c.ExternalDrift)

	metric("systemcd_paused",
		"1 when reconciliation is paused on this host.", "gauge", boolToInt(paused))
	metric("systemcd_out_of_sync",
		"1 when the host differs from the repository.", "gauge", boolToInt(rep.OutOfSync()))

	if rep.Revision != "" {
		metric("systemcd_revision_info",
			"The git revision last applied, as a label.", "gauge", 1,
			fmt.Sprintf("revision=%q", rep.Revision))
	}

	// Per-resource series, so an alert can name the resource rather than only
	// the host. Sorted for a stable file.
	fmt.Fprintf(&b, "# HELP systemcd_resource_out_of_sync Per-resource sync state.\n# TYPE systemcd_resource_out_of_sync gauge\n")
	results := append([]engine.Result(nil), rep.Results...)
	sort.SliceStable(results, func(i, j int) bool { return results[i].ID.String() < results[j].ID.String() })
	for _, res := range results {
		fmt.Fprintf(&b, "systemcd_resource_out_of_sync{kind=%q,name=%q,origin=%q} %d\n",
			res.ID.Kind, res.ID.Name, string(res.Owner.Origin), boolToInt(res.OutOfSync()))
	}
	return []byte(b.String())
}

// Write publishes the metrics to a textfile-collector directory.
func Write(h host.Host, path string, rep *engine.Report, paused bool) error {
	// node_exporter reads whatever it finds, so a half-written file would be
	// scraped as truth. WriteFile renames into place, which makes the swap
	// atomic from the collector's point of view.
	return h.WriteFile(path, Render(rep, paused), 0o644)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// StaleAfter is a suggested alerting threshold: a host whose last reconcile
// timestamp is older than this has probably stopped reconciling entirely,
// which no per-resource metric would reveal.
const StaleAfter = 30 * time.Minute
