//go:build load

// Package gatewayload drives the gateway the way the load requirement describes: 200 agents, each its own
// pod address, sending 500 requests a second against a budget that only has room for part of them. It
// checks what the hard cap promises (GW-US-1.1, GW-US-1.3): spend never exceeds the limit by more than one
// in-flight request per agent, every refusal is 402, and the gateway adds under 25 ms at p99.
//
// Run with `make load` or `go test -tags load ./test/load/gateway/`. AFS_LOAD_SECONDS (default 6) sets the
// duration. The spend store is in memory, so the latency measured is the gateway's own work; the database
// functions' concurrency is covered by the postgres integration tests.
package gatewayload

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/bedrock/fakebedrock"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/gatewayhttp/gwtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

const (
	agents = 200
	rate   = 500
	// Each call uses 100 input and 200 output tokens: 100*3 + 200*15 = 3300 micro-USD.
	perCall = 3300
	body    = `{"messages":[{"role":"user","content":[{"text":"load"}]}],"inferenceConfig":{"maxTokens":200}}`
)

func TestHardCapUnderLoad(t *testing.T) {
	secs := 6
	if v, err := strconv.Atoi(os.Getenv("AFS_LOAD_SECONDS")); err == nil && v > 0 {
		secs = v
	}
	total := secs * rate
	budgetCalls := total / 3 // room for a third of the calls: the rest must be refused
	limit := domain.MoneyFromMicros(int64(budgetCalls) * perCall)

	r := gwtest.New(t, func(r *gwtest.Rig) {
		r.Spend.Limit = limit
		for i := 0; i < agents; i++ {
			r.Dir.Put(agentIP(i), gateway.PodInfo{Namespace: "afs-t-alpha", Name: fmt.Sprintf("agent-%d-0", i), UID: fmt.Sprintf("u%d", i),
				ServiceAccount: fmt.Sprintf("agent-%d", i), TeamID: gwtest.TeamA, AgentID: fmt.Sprintf("agt_%026d", i)})
		}
	})
	r.Fake.Record(false)
	r.Fake.SetUsage(fakebedrock.Usage{Input: 100, Output: 200})

	clients := make([]*http.Client, agents)
	for i := range clients {
		d := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(agentIP(i))}}
		clients[i] = &http.Client{Transport: &http.Transport{DialContext: d.DialContext, MaxIdleConnsPerHost: 2}}
	}
	url := r.HTTP.URL + "/model/" + gwtest.Model + "/converse"

	var ok, refused, other atomic.Int64
	var mu sync.Mutex
	var otherNotes []string
	work := make([]chan struct{}, agents)
	var wg sync.WaitGroup
	for i := range work {
		work[i] = make(chan struct{}, 8)
		wg.Go(func() {
			for range work[i] {
				req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
				resp, err := clients[i].Do(req)
				switch {
				case err != nil:
					other.Add(1)
					mu.Lock()
					otherNotes = append(otherNotes, err.Error())
					mu.Unlock()
				default:
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					switch resp.StatusCode {
					case 200:
						ok.Add(1)
					case 402:
						refused.Add(1)
					default:
						other.Add(1)
						mu.Lock()
						otherNotes = append(otherNotes, fmt.Sprint("status ", resp.StatusCode))
						mu.Unlock()
					}
				}
			}
		})
	}

	tick := time.NewTicker(time.Second / rate)
	defer tick.Stop()
	start := time.Now()
	for n := 0; n < total; n++ {
		<-tick.C
		work[n%agents] <- struct{}{}
	}
	for _, c := range work {
		close(c)
	}
	wg.Wait()
	r.Svc.Wait()
	elapsed := time.Since(start)

	if other.Load() != 0 {
		t.Fatalf("%d calls were neither 200 nor 402: %v", other.Load(), otherNotes[:min(5, len(otherNotes))])
	}
	snap := r.Spend.Snapshot()
	over := snap.Spent.Micros() - limit.Micros()
	maxOver := int64(agents) * perCall // one in-flight request per agent
	var added []time.Duration
	for _, c := range r.Metrics.Calls() {
		added = append(added, c.Added)
	}
	sort.Slice(added, func(i, j int) bool { return added[i] < added[j] })
	p := func(q float64) time.Duration { return added[int(float64(len(added)-1)*q)] }
	t.Logf("agents=%d rate=%d/s duration=%s sent=%d ok=%d refused(402)=%d", agents, rate, elapsed.Round(time.Millisecond), total, ok.Load(), refused.Load())
	t.Logf("limit=%s spent=%s overshoot=%d micro-USD (allowed %d) reserved-after=%s", limit, snap.Spent, over, maxOver, snap.Reserved)
	t.Logf("added latency p50=%s p95=%s p99=%s max=%s", p(.5), p(.95), p(.99), added[len(added)-1])

	if refused.Load() == 0 || ok.Load() == 0 {
		t.Errorf("expected both successes and refusals, got ok=%d refused=%d", ok.Load(), refused.Load())
	}
	if over > maxOver {
		t.Errorf("spend exceeded the limit by %d micro-USD, more than one in-flight request per agent (%d)", over, maxOver)
	}
	if snap.Reserved.Micros() != 0 || r.Spend.Held() != 0 {
		t.Errorf("reservations left unsettled: %s (%d holds)", snap.Reserved, r.Spend.Held())
	}
	if p(.99) >= 25*time.Millisecond {
		t.Errorf("added latency p99 = %s, want under 25ms", p(.99))
	}
}

// agentIP gives each agent its own loopback address, which is what the gateway sees as the pod IP.
func agentIP(i int) string { return fmt.Sprintf("127.0.%d.%d", 10+i/250, 1+i%250) }
