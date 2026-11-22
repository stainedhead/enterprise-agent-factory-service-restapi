package httpapi_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// M3-X1: the measures the report code emits are the ones api/openapi.yaml documents for Report rows
// (estimatedUsd and billedUsd for costs), so the code and the contract cannot drift apart silently.
func TestReportMeasuresAreDocumentedInTheOpenAPIDocument(t *testing.T) {
	b, err := os.ReadFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	for _, k := range []domain.ReportKind{domain.ReportKindUsage, domain.ReportKindCosts, domain.ReportKindActivity, domain.ReportKindTraffic} {
		for _, m := range domain.ReportMeasures(k) {
			if !strings.Contains(doc, m.Name) {
				t.Errorf("measure %q of the %s report is not in api/openapi.yaml", m.Name, k)
			}
		}
	}
	if strings.Contains(doc, "costs rows carry costUsd") {
		t.Error("costs rows are estimatedUsd and billedUsd")
	}
}
