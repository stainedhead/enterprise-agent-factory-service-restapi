package observability

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// FR-R06: an alert or dashboard panel that reads a metric nothing ever writes can never fire. This test
// extracts every afs_* name from prometheusrule.yaml and dashboards/*.json and fails unless the name is
// registered (names.go) and appears on the kept list below with a writer that really exists in the
// non-test source of cmd/ or internal/.
//
// writers maps a metric to the Metrics fields or methods that write it; the test proves at least one is
// used outside metrics.go as a selector. A token ending in "." is a field that is written through
// (.Field.Inc()), one ending in "(" is a method call; a bare token matches either. When an alert starts to use a metric, add it here in the same change.
var writers = map[string][]string{
	MetricAPIRequests:                {"APIRequests"},
	MetricAPIRequestDuration:         {"APIRequestDuration"},
	MetricAPIInFlight:                {"APIInFlight"},
	MetricAPICrossTeamNotFound:       {"APICrossTeamNotFound"},
	MetricAPIOrphanSecrets:           {"APIOrphanSecrets"},
	MetricReconcileDuration:          {"ReconcileDuration."},
	MetricReconcileErrors:            {"ReconcileErrors."},
	MetricReconcileLag:               {"ReconcileLag."},
	MetricDriftCorrected:             {"DriftCorrected."},
	MetricLeader:                     {"SetLeader("},
	MetricGatewayReservationFailures: {"GatewayReservationFailures"},
	MetricGatewayAddedLatency:        {"GatewayAddedLatency"},
	MetricGatewayReservationsExpired: {"GatewayReservationsExpired"},
	MetricGatewaySettleFailures:      {"GatewaySettleFailures.", "SettleFailed("},
	MetricGatewayUsageRowsDropped:    {"GatewayUsageRowsDropped.", "UsageRowDropped("},
	MetricOutboxPending:              {"OutboxLag"},
	MetricOutboxOldestAge:            {"OutboxLag"},
	MetricOutboxPublished:            {"OutboxPublished"},
	MetricOutboxFailed:               {"OutboxFailed"},
	MetricNotificationFailures:       {"NotificationFailures."},
	MetricCURVariance:                {"CURVariance"},
	MetricReportRollupRuns:           {"RollupRun"},
	MetricReportRollupLastSuccess:    {"RollupRun"},
	MetricBuildInfo:                  {"NewMetrics"},
}

// deferredWriters are metrics registered and charted whose writer lands in another stream's commit.
// The writer is exempt from the source check until then; once a writer exists the test demands the
// entry be removed, so the list cannot hide a dead metric for longer than it takes to wire it.
var deferredWriters = map[string]string{}

var suffixRe = regexp.MustCompile(`_(bucket|sum|count)$`)

// referencedMetrics returns the AFS metric names the chart's alerts and dashboards read.
func referencedMetrics(t *testing.T) map[string][]string {
	t.Helper()
	files := []string{chartDir + "templates/prometheusrule.yaml"}
	dash, err := filepath.Glob(chartDir + "dashboards/*.json")
	if err != nil || len(dash) == 0 {
		t.Fatalf("no dashboards found: %v", err)
	}
	files = append(files, dash...)
	refs := map[string][]string{}
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // chart paths fixed by this test
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range metricRef.FindAllString(string(b), -1) {
			if base := suffixRe.ReplaceAllString(m, ""); declaredName(base) {
				m = base
			}
			refs[m] = append(refs[m], filepath.Base(f))
		}
	}
	return refs
}

func declaredName(n string) bool {
	for _, d := range AllMetricNames() {
		if d == n {
			return true
		}
	}
	return false
}

// usedOutsideMetrics reports whether any non-test Go file under cmd/ or internal/ (except metrics.go of
// this package) uses token as a selector, which is how a field or method of Metrics is written to.
func usedOutsideMetrics(t *testing.T, token string) bool {
	t.Helper()
	pat := `\.` + regexp.QuoteMeta(token)
	if last := token[len(token)-1]; last != '.' && last != '(' {
		pat += `\b`
	}
	re := regexp.MustCompile(pat)
	found := false
	for _, root := range []string{"../../../cmd", "../../../internal"} {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || found || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			if filepath.Base(p) == "metrics.go" && strings.Contains(p, "observability") {
				return nil
			}
			b, rerr := os.ReadFile(p) //nolint:gosec // walking the repository's own source
			if rerr == nil && re.Match(b) {
				found = true
			}
			return nil
		})
	}
	return found
}

func TestEveryChartedMetricHasAWriter(t *testing.T) {
	refs := referencedMetrics(t)
	if len(refs) < 10 {
		t.Fatalf("only %d metrics referenced; the extraction is broken", len(refs))
	}
	for name, where := range refs {
		toks, kept := writers[name]
		switch {
		case !declaredName(name):
			t.Errorf("%v read %s, which is not registered (names.go)", where, name)
		case !kept:
			t.Errorf("%v read %s, which is registered but not on the writers list of this test", where, name)
		}
		if !kept || name == MetricBuildInfo {
			continue
		}
		found := false
		for _, tok := range toks {
			if usedOutsideMetrics(t, tok) {
				found = true
				break
			}
		}
		why, deferred := deferredWriters[name]
		switch {
		case !found && !deferred:
			t.Errorf("%v read %s, but nothing in cmd/ or internal/ writes it (looked for %v): the alert or panel is dead", where, name, toks)
		case found && deferred:
			t.Errorf("%s now has a writer; remove it from deferredWriters (%s)", name, why)
		}
	}
	for name := range deferredWriters {
		if _, ok := refs[name]; !ok {
			t.Errorf("deferredWriters lists %s, which no alert or dashboard reads", name)
		}
	}
}

// Every registered metric is on the writers list, so a new metric cannot be added without naming its writer.
func TestWritersListCoversEveryRegisteredMetric(t *testing.T) {
	for _, n := range AllMetricNames() {
		if _, ok := writers[n]; !ok {
			t.Errorf("%s has no entry in the writers list", n)
		}
	}
	for n := range writers {
		if !declaredName(n) {
			t.Errorf("writers lists %s, which names.go does not declare", n)
		}
	}
}
