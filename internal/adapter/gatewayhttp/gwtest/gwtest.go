// Package gwtest builds a complete gateway for tests: the HTTP adapter, the use case over in-memory
// fakes, the real Bedrock runtime signing for real, and the fake Bedrock behind a TLS test server. The
// conformance and load suites use it, so they drive the same wiring the tests of the adapter do.
package gwtest

import (
	"context"
	"crypto/sha1" //nolint:gosec // a fake access key id, not a security use
	"encoding/hex"
	"net/http/httptest"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/bedrock"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/bedrock/fakebedrock"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/gatewayhttp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway/gatewaytest"
)

// Fixed ids of the rig. The ids are valid prefixed ULIDs; the account is the AWS documentation one.
const (
	TeamA   = "team_01JD0000000000000000000A00"
	TeamB   = "team_01JD0000000000000000000B00"
	BudgetA = "bud_01JD0000000000000000000A00"
	Model   = "anthropic.claude-3-5-sonnet-20241022-v2:0"
	ProfA   = "arn:aws:bedrock:us-east-1:111122223333:application-inference-profile/aaa"
	ProfB   = "arn:aws:bedrock:us-east-1:111122223333:application-inference-profile/bbb"
)

// Now is the rig's fixed clock.
var Now = time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

// Rig is one gateway under test. The agent pods are 127.0.0.1 (team A) and ::1 is not registered, so a
// test that needs an unknown caller uses the handler with another RemoteAddr.
type Rig struct {
	Fake    *fakebedrock.Fake
	Dir     *gatewaytest.Directory
	Tokens  *gatewaytest.Tokens
	Store   *gatewaytest.Store
	Spend   *gatewaytest.Spend
	Thr     *gatewaytest.Thresholds
	Usage   *gatewaytest.Usage
	Svc     *gateway.Service
	Server  *gatewayhttp.Server
	HTTP    *httptest.Server // the gateway, plain http
	Upstrm  *httptest.Server // the fake Bedrock, TLS
	Metrics *Observed
}

// Observed collects what the observer saw.
type Observed struct {
	Calls   chan ObservedCall
	Reserve chan string
}

// ObservedCall is one Observer.Done.
type ObservedCall struct {
	Op     string
	Status int
	Added  time.Duration
}

// Done implements gatewayhttp.Observer.
func (o *Observed) Done(op string, status int, added time.Duration) {
	select {
	case o.Calls <- ObservedCall{op, status, added}:
	default:
	}
}

// ReserveFailed implements gatewayhttp.Observer.
func (o *Observed) ReserveFailed(reason string) {
	select {
	case o.Reserve <- reason:
	default:
	}
}

type creds struct{}

func (creds) Credentials(_ context.Context, role string) (awssdk.Credentials, error) {
	h := sha1.Sum([]byte(role)) //nolint:gosec // see import
	return awssdk.Credentials{AccessKeyID: "ASIA" + hex.EncodeToString(h[:6]), SecretAccessKey: "fake-secret", SessionToken: "fake-token"}, nil
}

// KeyFor is the access key id the fake Bedrock sees for a team's role.
func KeyFor(roleARN string) string {
	c, _ := creds{}.Credentials(context.Background(), roleARN)
	return c.AccessKeyID
}

// New builds a rig with a $10 monthly budget on team A, 127.0.0.1 as team A's agent pod, and teams A and
// B each with the model enabled on their own inference profile..
func New(t testing.TB, mutate ...func(*Rig)) *Rig {
	t.Helper()
	r := &Rig{Fake: fakebedrock.New(), Dir: gatewaytest.NewDirectory(), Tokens: &gatewaytest.Tokens{IDs: map[string]gateway.TokenIdentity{}},
		Store: gatewaytest.NewStore(), Spend: gatewaytest.NewSpend(domain.MustMoney("10")), Thr: gatewaytest.NewThresholds(), Usage: &gatewaytest.Usage{},
		Metrics: &Observed{Calls: make(chan ObservedCall, 4096), Reserve: make(chan string, 64)}}
	r.Fake.Record(true)
	r.Dir.Put("127.0.0.1", gateway.PodInfo{Namespace: "afs-t-alpha", Name: "agent-bot-0", UID: "u1", ServiceAccount: "agent-bot", TeamID: TeamA, AgentID: "agt_01"})
	r.Dir.Put("10.0.1.1", gateway.PodInfo{Namespace: "afs-t-beta", Name: "agent-x-0", UID: "u3", ServiceAccount: "agent-x", TeamID: TeamB, AgentID: "agt_02"})
	r.Store.Teams[TeamA] = gateway.TeamRecord{ID: TeamA, Namespace: "afs-t-alpha", State: "active"}
	r.Store.Teams[TeamB] = gateway.TeamRecord{ID: TeamB, Namespace: "afs-t-beta", State: "active"}
	r.Store.Models[TeamA+"\x00"+Model] = gateway.ModelAccess{Enabled: true, ProfileARN: ProfA, MaxOutputTokens: 4096}
	r.Store.Models[TeamB+"\x00"+Model] = gateway.ModelAccess{Enabled: true, ProfileARN: ProfB, MaxOutputTokens: 4096}
	r.Store.PriceRows[Model] = []gateway.Price{{EffectiveFrom: Now.Add(-time.Hour), Input: domain.MustMoney("3"), Output: domain.MustMoney("15"),
		CacheRead: domain.MustMoney("0.3"), CacheWrite: domain.MustMoney("3.75")}}
	r.Store.Budgets[TeamA] = gateway.BudgetRecord{ID: BudgetA, Period: "monthly", StartsAt: Now.Add(-90 * 24 * time.Hour),
		Thresholds: []int{50, 80, 100}, OnExhaust: "block", Amount: domain.MustMoney("10")}
	for _, m := range mutate {
		m(r)
	}

	r.Upstrm = httptest.NewTLSServer(r.Fake.Handler())
	t.Cleanup(r.Upstrm.Close)
	rt, err := bedrock.NewRuntime(bedrock.RuntimeConfig{Endpoint: r.Upstrm.URL, Region: "us-east-1", Credentials: creds{}, HTTPClient: r.Upstrm.Client()})
	if err != nil {
		t.Fatal(err)
	}
	r.Svc = gateway.NewService(gateway.Deps{Pods: r.Dir, Tokens: r.Tokens, Store: r.Store, Spend: r.Spend, Thresholds: r.Thr, Usage: r.Usage},
		gateway.Config{Partition: "aws", AccountID: "111122223333", FailClosedTimeout: 2 * time.Second, Now: func() time.Time { return Now }})
	r.Server = gatewayhttp.New(gatewayhttp.Config{Service: r.Svc, Upstream: rt, Observer: r.Metrics})
	r.HTTP = httptest.NewServer(r.Server.Handler())
	t.Cleanup(func() { r.HTTP.Close(); r.Svc.Wait() })
	return r
}
