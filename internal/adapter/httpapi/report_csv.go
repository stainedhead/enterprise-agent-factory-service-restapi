package httpapi

import (
	"bytes"
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// ReportCSV renders a whole report as the CSV the report operations return for Accept: text/csv: the
// groupBy columns then the measures, one line per row, spreadsheet formulas neutralized. afs-billing
// stores the same bytes for a scheduled report (BI-US-3.1), so a schedule's CSV equals the on-demand one.
func ReportCSV(rep domain.Report) ([]byte, error) {
	buf := &csvBuffer{header: http.Header{}}
	header, cells := reportCells(rep)
	if err := WriteCSV(buf, header, cells, ""); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// csvBuffer is the in-memory http.ResponseWriter that ReportCSV writes through.
type csvBuffer struct {
	bytes.Buffer
	header http.Header
}

func (b *csvBuffer) Header() http.Header { return b.header }
func (*csvBuffer) WriteHeader(int)       {}
