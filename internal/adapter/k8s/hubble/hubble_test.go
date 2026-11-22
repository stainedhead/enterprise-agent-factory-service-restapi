package hubble_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/mem"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/hubble"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collector/traffic"
)

// recorded returns the GetFlowsResponse messages recorded from the generated Cilium types
// (testdata/flows.txt, "name base64" per line).
func recorded(t *testing.T) map[string][]byte {
	t.Helper()
	f, err := os.Open("testdata/flows.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string][]byte{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, b64, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = b
	}
	return out
}

const (
	agtLabel  = "k8s:afs.stainedhead.io/agent-id=agt_01JD0000000000000000000081"
	poolLabel = "k8s:afs.stainedhead.io/pool-id=pool_01JD0000000000000000000091"
)

// M2-H2: the decoder turns recorded Hubble responses into the flows the collector counts.
func TestDecodeRecordedFlows(t *testing.T) {
	at := time.Date(2026, 11, 22, 10, 2, 30, 0, time.UTC)
	for _, tc := range []struct {
		name string
		want traffic.Flow
	}{
		{"allowed_syn", traffic.Flow{Time: at, Egress: true, Forwarded: true, Proto: "tcp", SYN: true, DstPort: 443, DstNames: []string{"api.github.com"},
			SrcNamespace: "afs-t-tr", SrcPod: "agent-alpha-0", SrcLabels: []string{agtLabel, "k8s:app=agent", "k8s:io.kubernetes.pod.namespace=afs-t-tr"}}},
		{"allowed_established", traffic.Flow{Time: at, Egress: true, Forwarded: true, Proto: "tcp", ACK: true, DstPort: 443, DstNames: []string{"api.github.com"},
			SrcNamespace: "afs-t-tr", SrcPod: "agent-alpha-0", SrcLabels: []string{agtLabel, "k8s:app=agent", "k8s:io.kubernetes.pod.namespace=afs-t-tr"}}},
		{"denied", traffic.Flow{Time: at, Egress: true, PolicyDenied: true, Proto: "tcp", SYN: true, DstPort: 443, DstNames: []string{"evil.example.net"},
			SrcNamespace: "afs-t-tr", SrcPod: "agent-alpha-0", SrcLabels: []string{agtLabel, "k8s:app=agent", "k8s:io.kubernetes.pod.namespace=afs-t-tr"}}},
		{"pool_worker_denied", traffic.Flow{Time: at, Egress: true, PolicyDenied: true, Proto: "tcp", SYN: true, DstPort: 8443, DstNames: []string{"slack.com"},
			SrcNamespace: "afs-t-tr", SrcPod: "pool-review-7d9f-x2k4q", SrcLabels: []string{poolLabel, "k8s:io.kubernetes.pod.namespace=afs-t-tr"}}},
		{"udp_denied", traffic.Flow{Time: at, Egress: true, PolicyDenied: true, Proto: "udp", DstPort: 443, DstNames: []string{"quic.example.net"},
			SrcNamespace: "afs-t-tr", SrcPod: "agent-alpha-0", SrcLabels: []string{agtLabel, "k8s:app=agent", "k8s:io.kubernetes.pod.namespace=afs-t-tr"}}},
		{"ingress", traffic.Flow{Time: at, Forwarded: true, Proto: "tcp", SYN: true, DstPort: 8080}},
		{"reply", traffic.Flow{Time: at, Egress: true, Forwarded: true, Reply: true, Proto: "tcp", SYN: true, ACK: true, DstPort: 443, DstNames: []string{"api.github.com"},
			SrcNamespace: "afs-t-tr", SrcPod: "agent-alpha-0", SrcLabels: []string{agtLabel, "k8s:app=agent", "k8s:io.kubernetes.pod.namespace=afs-t-tr"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := recorded(t)[tc.name]
			if raw == nil {
				t.Fatal("fixture missing")
			}
			got, ok, err := hubble.DecodeResponse(raw)
			if err != nil || !ok {
				t.Fatalf("decode: ok=%v err=%v", ok, err)
			}
			if !sameFlow(got, tc.want) {
				t.Errorf("flow = %+v\nwant   %+v", got, tc.want)
			}
		})
	}
}

func sameFlow(a, b traffic.Flow) bool {
	return a.Time.Equal(b.Time) && a.Egress == b.Egress && a.Reply == b.Reply && a.Forwarded == b.Forwarded && a.PolicyDenied == b.PolicyDenied &&
		a.Proto == b.Proto && a.SYN == b.SYN && a.ACK == b.ACK && a.DstPort == b.DstPort && a.SrcNamespace == b.SrcNamespace &&
		a.SrcPod == b.SrcPod && strings.Join(a.DstNames, ",") == strings.Join(b.DstNames, ",") && strings.Join(a.SrcLabels, ",") == strings.Join(b.SrcLabels, ",")
}

func TestDecodeEdgeCases(t *testing.T) {
	// A response without a flow (node status, lost events) is not an error and has no flow.
	var statusOnly []byte
	statusOnly = protowire.AppendTag(statusOnly, 2, protowire.BytesType)
	statusOnly = protowire.AppendBytes(statusOnly, []byte{0x08, 0x01})
	statusOnly = protowire.AppendTag(statusOnly, 1000, protowire.BytesType)
	statusOnly = protowire.AppendBytes(statusOnly, []byte("node-a"))
	if _, ok, err := hubble.DecodeResponse(statusOnly); ok || err != nil {
		t.Errorf("status-only response: ok=%v err=%v", ok, err)
	}
	if _, ok, err := hubble.DecodeResponse(nil); ok || err != nil {
		t.Errorf("empty response: ok=%v err=%v", ok, err)
	}
	// Truncated and garbage input fail without panicking.
	full := recorded(t)["allowed_syn"]
	for i := 1; i < len(full); i += 7 {
		_, _, _ = hubble.DecodeResponse(full[:i])
	}
	for _, bad := range [][]byte{{0xff}, {0x0a, 0x05, 0x01}, {0x08}, {0x0d, 0x01}, {0x0a, 0x02, 0xff, 0xff}} {
		if _, ok, err := hubble.DecodeResponse(bad); ok || err == nil {
			t.Errorf("DecodeResponse(%x): ok=%v err=%v, want an error", bad, ok, err)
		}
	}
	// Fixed-width fields and unknown fields are skipped.
	var withFixed []byte
	withFixed = protowire.AppendTag(withFixed, 5000, protowire.Fixed64Type)
	withFixed = protowire.AppendFixed64(withFixed, 7)
	withFixed = protowire.AppendTag(withFixed, 5001, protowire.Fixed32Type)
	withFixed = protowire.AppendFixed32(withFixed, 7)
	if _, ok, err := hubble.DecodeResponse(withFixed); ok || err != nil {
		t.Errorf("unknown fields: ok=%v err=%v", ok, err)
	}
}

// codec of the test server: bytes in and out.
type rawCodec struct{}

func (rawCodec) Marshal(v any) (mem.BufferSlice, error) {
	return mem.BufferSlice{mem.SliceBuffer(*(v.(*[]byte)))}, nil
}
func (rawCodec) Unmarshal(d mem.BufferSlice, v any) error {
	*(v.(*[]byte)) = d.Materialize()
	return nil
}
func (rawCodec) Name() string { return "proto" }

// hubbleServer serves GetFlows from recorded messages and records the request it was sent.
type hubbleServer struct {
	mu       sync.Mutex
	request  []byte
	method   string
	messages [][]byte
	hold     bool
}

func (h *hubbleServer) start(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.ForceServerCodecV2(rawCodec{}), grpc.UnknownServiceHandler(func(_ any, st grpc.ServerStream) error {
		var req []byte
		if err := st.RecvMsg(&req); err != nil {
			return err
		}
		h.mu.Lock()
		h.request = req
		h.method, _ = grpc.MethodFromServerStream(st)
		msgs, hold := h.messages, h.hold
		h.mu.Unlock()
		for _, m := range msgs {
			m := m
			if err := st.SendMsg(&m); err != nil {
				return err
			}
		}
		if hold {
			<-st.Context().Done()
		}
		return nil
	}))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// M2-H2: the observer asks for new egress flows and delivers what the relay sends; a message that
// cannot be decoded is skipped.
func TestObserverStreams(t *testing.T) {
	rec := recorded(t)
	srv := &hubbleServer{messages: [][]byte{rec["allowed_syn"], {0xff}, rec["denied"]}, hold: true}
	addr := srv.start(t)
	obs, err := hubble.NewObserver(hubble.Config{Address: addr})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var mu sync.Mutex
	var got []traffic.Flow
	done := make(chan error, 1)
	go func() {
		done <- obs.Stream(ctx, func(f traffic.Flow) { mu.Lock(); got = append(got, f); mu.Unlock() })
	}()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Stream = %v, want context.Canceled", err)
	}
	if len(got) != 2 || got[0].DstNames[0] != "api.github.com" || !got[1].PolicyDenied {
		t.Fatalf("flows = %+v", got)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.method != "/observer.Observer/GetFlows" {
		t.Errorf("method = %q", srv.method)
	}
	// The request is byte for byte what the generated Cilium types encode for
	// GetFlowsRequest{Follow: true, Whitelist: [{TrafficDirection: [EGRESS]}]}.
	if want := rec["request"]; string(srv.request) != string(want) {
		t.Errorf("request = %x, want %x", srv.request, want)
	}
}

func TestObserverEnds(t *testing.T) {
	if _, err := hubble.NewObserver(hubble.Config{}); err == nil {
		t.Error("want an error without an address")
	}
	// The relay closes the stream: Stream returns, so the collector reconnects.
	srv := &hubbleServer{messages: [][]byte{recorded(t)["allowed_syn"]}}
	obs, _ := hubble.NewObserver(hubble.Config{Address: srv.start(t)})
	n := 0
	if err := obs.Stream(t.Context(), func(traffic.Flow) { n++ }); err != nil || n != 1 {
		t.Errorf("Stream = %v after %d flows", err, n)
	}
	// An unreachable relay is an error.
	obs, _ = hubble.NewObserver(hubble.Config{Address: "127.0.0.1:1"})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := obs.Stream(ctx, func(traffic.Flow) {}); err == nil {
		t.Error("want an error for an unreachable relay")
	}
}

// M2-H4: the sign-in record is a Fluent Forward message, in the aggregator of the team's namespace.
func TestForwardEncoding(t *testing.T) {
	at := time.Unix(1700000000, 0)
	got := hubble.EncodeForward("afs.signin", at, map[string]string{"b": "2", "a": "1"})
	want := []byte{0x93, 0xaa, 'a', 'f', 's', '.', 's', 'i', 'g', 'n', 'i', 'n', 0xce, 0x65, 0x53, 0xf1, 0x00, 0x82, 0xa1, 'a', 0xa1, '1', 0xa1, 'b', 0xa1, '2'}
	if string(got) != string(want) {
		t.Errorf("bytes = %x\nwant    %x", got, want)
	}
	long := strings.Repeat("x", 300)
	b := hubble.EncodeForward("t", at, map[string]string{"k": long})
	if !strings.Contains(string(b), long) || b[len(b)-303] != 0xda {
		t.Errorf("str16 encoding: % x", b[:20])
	}
	mid := strings.Repeat("y", 40)
	if b := hubble.EncodeForward("t", at, map[string]string{"k": mid}); !strings.Contains(string(b), "\xd9\x28"+mid) {
		t.Error("str8 encoding")
	}
	many := map[string]string{}
	for i := range 20 {
		many[string(rune('a'+i))] = "v"
	}
	if b := hubble.EncodeForward("t", at, many); b[len("t")+1+1+5] != 0xde {
		t.Errorf("map16 header: %x", b[:12])
	}
	huge := strings.Repeat("z", 70000)
	if b := hubble.EncodeForward("t", at, map[string]string{"k": huge}); len(b) > 70000 {
		t.Errorf("an oversized value must be cut, got %d bytes", len(b))
	}
}

func TestSignInRecord(t *testing.T) {
	at := time.Date(2026, 11, 22, 10, 2, 30, 0, time.UTC)
	a := hubble.SignInRecord(traffic.SignIn{At: at, TeamID: "team_1", AgentID: "agt_1", System: "okta", SystemID: "ext_1", Host: "login.example.com"})
	if a["agentId"] != "agt_1" || a["poolId"] != "" || a["event"] != "agent.sign_in" || a["time"] != "2026-11-22T10:02:30Z" || a["system"] != "okta" {
		t.Errorf("agent record = %v", a)
	}
	p := hubble.SignInRecord(traffic.SignIn{At: at, TeamID: "team_1", PoolID: "pool_1", WorkerPod: "w-1", System: "okta"})
	if p["poolId"] != "pool_1" || p["workerPod"] != "w-1" || p["agentId"] != "" {
		t.Errorf("pool record = %v", p)
	}
	if got := hubble.Addr("afs-t-x"); got != "afs-log-aggregator.afs-t-x.svc:24224" {
		t.Errorf("addr = %q", got)
	}
}

// The sink dials the aggregator of the sign-in's own namespace only, sends one message, and drops the
// record when there is no aggregator.
func TestSignInSinkDelivers(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	received := make(chan []byte, 1)
	go func() {
		c, err := lis.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 4096)
		n, _ := c.Read(buf)
		received <- buf[:n]
	}()
	sink := hubble.NewSignInSink(4, nil)
	var dialed []string
	sink.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		if !strings.Contains(addr, "afs-t-a.svc") {
			return nil, errors.New("no aggregator")
		}
		var d net.Dialer
		return d.DialContext(ctx, network, lis.Addr().String())
	}
	at := time.Date(2026, 11, 22, 10, 2, 30, 0, time.UTC)
	for _, si := range []traffic.SignIn{
		{At: at, TeamID: "team_b", Namespace: "afs-t-b", AgentID: "agt_b", System: "okta"},
		{At: at, TeamID: "team_a", Namespace: "afs-t-a", AgentID: "agt_a", System: "okta", Host: "login.example.com"},
		{At: at, TeamID: "team_x", AgentID: "agt_x"},
	} {
		if err := sink.Record(t.Context(), si); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- sink.Run(ctx) }()
	select {
	case msg := <-received:
		if !strings.Contains(string(msg), "agt_a") || strings.Contains(string(msg), "agt_b") || !strings.Contains(string(msg), "afs.signin") {
			t.Errorf("message = %q", msg)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nothing delivered")
	}
	cancel()
	<-done
	for _, a := range dialed {
		if !strings.HasSuffix(a, ".svc:24224") {
			t.Errorf("dialed %q", a)
		}
	}
}

func TestSignInSinkQueueFull(t *testing.T) {
	sink := hubble.NewSignInSink(1, nil)
	if err := sink.Record(t.Context(), traffic.SignIn{}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Record(t.Context(), traffic.SignIn{}); !errors.Is(err, hubble.ErrQueueFull) {
		t.Errorf("err = %v, want ErrQueueFull", err)
	}
	if hubble.NewSignInSink(0, nil) == nil {
		t.Error("default queue size")
	}
}
