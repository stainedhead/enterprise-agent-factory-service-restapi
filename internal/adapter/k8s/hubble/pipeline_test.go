package hubble_test

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/hubble"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collector/traffic"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/egress"
)

type pipeDir struct{}

func (pipeDir) systems() []egress.System {
	return []egress.System{
		{ID: "ext_gh", Slug: "github", Category: "dev_tools", Endpoints: []string{"api.github.com"}},
		{ID: "ext_slack", Slug: "slack", Category: "collaboration", Endpoints: []string{"slack.com:8443"}, DisabledAt: egress.OffPool},
	}
}
func (d pipeDir) ResolveAgent(_ context.Context, ns, id string) (traffic.Resolved, bool, error) {
	ok := ns == "afs-t-tr" && id == "agt_01JD0000000000000000000081"
	return traffic.Resolved{TeamID: "team_01JD0000000000000000000051", Systems: d.systems()}, ok, nil
}
func (d pipeDir) ResolvePool(_ context.Context, ns, id string) (traffic.Resolved, bool, error) {
	ok := ns == "afs-t-tr" && id == "pool_01JD0000000000000000000091"
	return traffic.Resolved{TeamID: "team_01JD0000000000000000000051", Systems: d.systems()}, ok, nil
}

type pipeStore struct {
	mu   sync.Mutex
	rows []traffic.Row
}

func (s *pipeStore) Add(_ context.Context, rows []traffic.Row) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, rows...)
	return nil
}

type noRedact struct{}

func (noRedact) Redact(s string) string { return s }

// M2-H2: Hubble responses recorded from the Cilium API types, served by a relay and counted by the
// collector, give the rows of external_traffic: connections only at the SYN, denied reasons by system
// and level, pool worker and pod, and nothing for ingress, replies or packets after the first.
func TestRecordedFlowsBecomeRows(t *testing.T) {
	rec := recorded(t)
	srv := &hubbleServer{}
	for _, name := range []string{"allowed_syn", "allowed_established", "allowed_syn", "denied", "pool_worker_denied", "udp_denied", "ingress", "reply"} {
		srv.messages = append(srv.messages, rec[name])
	}
	obs, err := hubble.NewObserver(hubble.Config{Address: srv.start(t)})
	if err != nil {
		t.Fatal(err)
	}
	st := &pipeStore{}
	c := traffic.New(traffic.Config{Source: obs, Directory: pipeDir{}, Store: st, Redactor: noRedact{}, FlushEvery: time.Hour})
	if err := obs.Stream(t.Context(), func(f traffic.Flow) { c.Handle(t.Context(), f) }); err != nil {
		t.Fatal(err)
	}
	if err := c.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	type got struct {
		subject, host, verdict, reason, system string
		port, conns                            int
	}
	var rows []got
	for _, r := range st.rows {
		subject := r.AgentID
		if r.PoolID != "" {
			subject = r.PoolID + "/" + r.WorkerPod
		}
		rows = append(rows, got{subject, r.Host, r.Verdict, r.DeniedReason, r.SystemID, r.Port, r.Connections})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].host < rows[j].host })
	want := []got{
		{"agt_01JD0000000000000000000081", "api.github.com", "allowed", "", "ext_gh", 443, 2},
		{"agt_01JD0000000000000000000081", "evil.example.net", "denied", "not_in_catalog", "", 443, 1},
		{"agt_01JD0000000000000000000081", "quic.example.net", "denied", "not_in_catalog", "", 443, 1},
		{"pool_01JD0000000000000000000091/pool-review-7d9f-x2k4q", "slack.com", "denied", "system_off_pool", "ext_slack", 8443, 1},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v", rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
	for _, r := range st.rows {
		if !r.BucketStart.Equal(time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)) {
			t.Errorf("bucket = %v", r.BucketStart)
		}
	}
}
