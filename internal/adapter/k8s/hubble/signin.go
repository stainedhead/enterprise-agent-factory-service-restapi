package hubble

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collector/traffic"
)

// SignInTag is the Fluent Bit tag of the sign-in records.
const SignInTag = "afs.signin"

// SignInSink implements traffic.SignIns (M2-H4): it sends each sign-in as one record to the Fluent Bit
// aggregator in the team's own namespace (Fluent Forward protocol, message mode), so the record goes
// through the team's redaction filter to the team's destinations and nowhere else. A team without log
// delivery has no aggregator, the connection fails, and the record is dropped: it is stored nowhere and
// published as no event.
//
// Record never blocks the flow stream: it queues the record, and Run delivers the queue.
type SignInSink struct {
	queue chan traffic.SignIn
	// Dial opens a connection (net.Dialer.DialContext by default).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Timeout bounds one delivery (default 3 s).
	Timeout time.Duration
	Log     *slog.Logger
}

// NewSignInSink returns a SignInSink with a queue of size records.
func NewSignInSink(size int, log *slog.Logger) *SignInSink {
	if size < 1 {
		size = 256
	}
	if log == nil {
		log = slog.Default()
	}
	d := &net.Dialer{}
	return &SignInSink{queue: make(chan traffic.SignIn, size), Dial: d.DialContext, Timeout: 3 * time.Second, Log: log}
}

var _ traffic.SignIns = (*SignInSink)(nil)

// ErrQueueFull is returned by Record when the queue is full and the record was dropped.
var ErrQueueFull = errors.New("hubble: sign-in queue is full")

// Record implements traffic.SignIns.
func (s *SignInSink) Record(_ context.Context, si traffic.SignIn) error {
	select {
	case s.queue <- si:
		return nil
	default:
		return ErrQueueFull
	}
}

// Run delivers queued records until ctx ends.
func (s *SignInSink) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case si := <-s.queue:
			if err := s.deliver(ctx, si); err != nil {
				s.Log.Debug("sign-in not delivered", slog.String("team", si.TeamID), slog.Any("err", err))
			}
		}
	}
}

// Addr is the aggregator's address in a team namespace.
func Addr(namespace string) string {
	return net.JoinHostPort(render.LogAggregatorName+"."+namespace+".svc", strconv.Itoa(render.LogForwardPort))
}

func (s *SignInSink) deliver(ctx context.Context, si traffic.SignIn) error {
	if si.Namespace == "" {
		return errors.New("hubble: sign-in without a namespace")
	}
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	conn, err := s.Dial(ctx, "tcp", Addr(si.Namespace))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetWriteDeadline(time.Now().Add(s.Timeout))
	_, err = conn.Write(EncodeForward(SignInTag, si.At, SignInRecord(si)))
	return err
}

// SignInRecord is the log record of a sign-in: which agent or worker, which system, when.
func SignInRecord(si traffic.SignIn) map[string]string {
	r := map[string]string{
		"event": "agent.sign_in", "teamId": si.TeamID, "system": si.System, "systemId": si.SystemID, "host": si.Host,
		"time": si.At.UTC().Format(time.RFC3339),
	}
	if si.AgentID != "" {
		r["agentId"] = si.AgentID
	}
	if si.PoolID != "" {
		r["poolId"] = si.PoolID
		r["workerPod"] = si.WorkerPod
	}
	return r
}

// EncodeForward encodes a Fluent Forward message-mode event, [tag, time, record], as MessagePack. Keys
// are written in sorted order so the bytes are stable.
func EncodeForward(tag string, at time.Time, record map[string]string) []byte {
	b := []byte{0x93}
	b = appendStr(b, tag)
	b = append(b, 0xce)
	b = binary.BigEndian.AppendUint32(b, uint32(at.Unix())) //nolint:gosec // seconds since 1970 fit until 2106, the width of the protocol field
	keys := make([]string, 0, len(record))
	for k := range record {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	switch n := len(keys); {
	case n < 16:
		b = append(b, 0x80|byte(n))
	default:
		b = append(b, 0xde)
		b = binary.BigEndian.AppendUint16(b, uint16(n)) //nolint:gosec // bounded by the few keys of a record
	}
	for _, k := range keys {
		b = appendStr(b, k)
		b = appendStr(b, record[k])
	}
	return b
}

func appendStr(b []byte, s string) []byte {
	switch n := len(s); {
	case n < 32:
		b = append(b, 0xa0|byte(n))
	case n < 256:
		b = append(b, 0xd9, byte(n))
	case n < 65536:
		b = append(b, 0xda)
		b = binary.BigEndian.AppendUint16(b, uint16(n)) //nolint:gosec // bounded by the few keys of a record
	default:
		// Values are host names and ids, far below this; a longer one is cut rather than refused.
		s = s[:65535]
		b = append(b, 0xda, 0xff, 0xff)
	}
	return append(b, s...)
}
