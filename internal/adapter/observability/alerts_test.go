package observability

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Alerts and dashboards are written against names.go; a metric they use that no binary declares is a
// silent alert, so this test reads the chart files and checks every afs_* name.
const chartDir = "../../../deploy/helm/afs/"

var metricRef = regexp.MustCompile(`\bafs_[a-z0-9_]+`)

func declared() map[string]bool {
	ok := map[string]bool{}
	for _, n := range AllMetricNames() {
		ok[n] = true
		for _, s := range []string{"_bucket", "_sum", "_count"} {
			ok[n+s] = true
		}
	}
	return ok
}

func checkRefs(t *testing.T, file string, extra ...string) {
	t.Helper()
	b, err := os.ReadFile(chartDir + file) //nolint:gosec // fixed chart paths named in this file
	if err != nil {
		t.Fatal(err)
	}
	ok := declared()
	n := 0
	for _, m := range metricRef.FindAllString(string(b), -1) {
		if ok[m] || slices.Contains(extra, m) {
			n++
			continue
		}
		t.Errorf("%s uses %s, which no AFS binary declares (names.go)", file, m)
	}
	if n == 0 {
		t.Errorf("%s references no AFS metric", file)
	}
}

func TestAlertsUseDeclaredMetrics(t *testing.T) {
	// afs-billing-<job> and afs-slis are object names, not metrics: they contain a dash and do not match.
	checkRefs(t, "templates/prometheusrule.yaml")
}

func TestDashboardUsesDeclaredMetricsAndIsValidJSON(t *testing.T) {
	checkRefs(t, "dashboards/afs-slis.json")
	b, _ := os.ReadFile(chartDir + "dashboards/afs-slis.json")
	var d struct {
		UID    string `json:"uid"`
		Panels []struct {
			ID      int `json:"id"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	ids := map[int]bool{}
	for _, p := range d.Panels {
		if ids[p.ID] {
			t.Errorf("duplicate panel id %d", p.ID)
		}
		ids[p.ID] = true
		for _, tg := range p.Targets {
			if strings.Count(tg.Expr, "(") != strings.Count(tg.Expr, ")") {
				t.Errorf("unbalanced expression %q", tg.Expr)
			}
		}
	}
	if d.UID != "afs-slis" || len(d.Panels) < 10 {
		t.Errorf("dashboard uid=%q panels=%d", d.UID, len(d.Panels))
	}
}

// Every SLI named by M1-L3 has an alert and a dashboard panel.
func TestEverySLIHasAnAlertAndAPanel(t *testing.T) {
	alerts, _ := os.ReadFile(chartDir + "templates/prometheusrule.yaml")
	dash, _ := os.ReadFile(chartDir + "dashboards/afs-slis.json")
	for _, m := range []string{
		MetricAPIRequestDuration, MetricReconcileLag, MetricDriftCorrected, MetricGatewayReservationFailures,
		MetricGatewayAddedLatency, MetricGatewayReservationsExpired, MetricGatewaySettleFailures, MetricOutboxOldestAge, MetricNotificationFailures, MetricCURVariance,
	} {
		if !strings.Contains(string(alerts), m) {
			t.Errorf("no alert on %s", m)
		}
		if !strings.Contains(string(dash), m) {
			t.Errorf("no dashboard panel on %s", m)
		}
	}
}

// C-7: settle failures have an alert, dropped usage rows have a dashboard panel.
func TestGatewayIntegrityCountersAreAlertedAndCharted(t *testing.T) {
	alerts, _ := os.ReadFile(chartDir + "templates/prometheusrule.yaml")
	if !strings.Contains(string(alerts), "alert: AFSGatewaySettleFailures") || !strings.Contains(string(alerts), "increase("+MetricGatewaySettleFailures) {
		t.Error("no AFSGatewaySettleFailures alert on afs_gateway_settle_failures_total")
	}
	dash, _ := os.ReadFile(chartDir + "dashboards/afs-slis.json")
	if !strings.Contains(string(dash), MetricGatewayUsageRowsDropped) || !strings.Contains(string(dash), "by (reason)") {
		t.Error("no dashboard panel on afs_gateway_usage_rows_dropped_total by reason")
	}
}
