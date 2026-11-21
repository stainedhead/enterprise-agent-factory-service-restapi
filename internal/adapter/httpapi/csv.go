package httpapi

import (
	"encoding/csv"
	"mime"
	"net/http"
	"strings"
)

// WantsCSV reports whether the client asked for text/csv; only routes flagged CSV honor it.
func WantsCSV(r *http.Request) bool {
	rt, ok := RouteFrom(r)
	if !ok || !rt.CSV {
		return false
	}
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		if mt, _, err := mime.ParseMediaType(strings.TrimSpace(part)); err == nil && mt == "text/csv" {
			return true
		}
	}
	return false
}

// WriteCSV writes a CSV page; the next page's token goes in X-Afs-Next-Token. Cells that a
// spreadsheet would run as a formula are prefixed with a quote.
func WriteCSV(w http.ResponseWriter, header []string, rows [][]string, nextToken string) error {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	if nextToken != "" {
		w.Header().Set("X-Afs-Next-Token", nextToken)
	}
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	if err := cw.Write(header); err != nil {
		return err
	}
	for _, row := range rows {
		safe := make([]string, len(row))
		for i, c := range row {
			safe[i] = csvSafe(c)
		}
		if err := cw.Write(safe); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func csvSafe(c string) string {
	if c == "" {
		return c
	}
	switch c[0] {
	case '=', '+', '@', '\t', '\r':
		return "'" + c
	case '-':
		if len(c) > 1 && (c[1] < '0' || c[1] > '9') && c[1] != '.' {
			return "'" + c
		}
	}
	return c
}
