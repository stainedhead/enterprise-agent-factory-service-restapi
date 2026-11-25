package postgres

import (
	"context"
	"log/slog"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpgateway"
)

// McpLedger writes afs.mcp_events in batches (gateway contract §13.8): Record never blocks, a full queue drops the
// row, and a failed batch is logged and dropped, because telemetry fails open. No argument, result or query text is
// stored except afs_search query text, which the database keeps only while a capture window is open.
type McpLedger struct {
	pool  *pgxpool.Pool
	log   *slog.Logger
	ch    chan mcpgateway.Event
	done  chan struct{}
	once  sync.Once
	wg    sync.WaitGroup
	every time.Duration
	batch int

	dropped, written atomic.Int64
}

// NewMcpLedger returns a ledger with a queue of 20,000 rows, flushed every 500 ms or 500 rows.
func NewMcpLedger(pool *pgxpool.Pool, log *slog.Logger) *McpLedger {
	if log == nil {
		log = slog.Default()
	}
	return &McpLedger{pool: pool, log: log, ch: make(chan mcpgateway.Event, 20000), done: make(chan struct{}), every: 500 * time.Millisecond, batch: 500}
}

var _ mcpgateway.Ledger = (*McpLedger)(nil)

// Record implements mcpgateway.Ledger.
func (l *McpLedger) Record(e mcpgateway.Event) {
	select {
	case l.ch <- e:
	default:
		l.dropped.Add(1)
	}
}

// Dropped and Written count rows lost to a full queue or a failed write, and rows stored.
func (l *McpLedger) Dropped() int64 { return l.dropped.Load() }

// Written returns the rows stored.
func (l *McpLedger) Written() int64 { return l.written.Load() }

// Run starts the writer; it returns at once.
func (l *McpLedger) Run(ctx context.Context) {
	l.wg.Go(func() { l.loop(ctx) })
}

// Close writes what is queued and stops the writer.
func (l *McpLedger) Close() {
	l.once.Do(func() { close(l.done) })
	l.wg.Wait()
}

func (l *McpLedger) loop(ctx context.Context) {
	buf := make([]mcpgateway.Event, 0, l.batch)
	tick := time.NewTicker(l.every)
	defer tick.Stop()
	flush := func(ctx context.Context) {
		if len(buf) > 0 {
			l.write(ctx, buf)
			buf = buf[:0]
		}
	}
	for {
		select {
		case e := <-l.ch:
			buf = append(buf, e)
			if len(buf) >= l.batch {
				flush(ctx)
			}
		case <-tick.C:
			flush(ctx)
		case <-l.done:
			ctx = context.WithoutCancel(ctx)
			for {
				select {
				case e := <-l.ch:
					buf = append(buf, e)
					if len(buf) >= l.batch {
						flush(ctx)
					}
				default:
					flush(ctx)
					return
				}
			}
		}
	}
}

const insertMcpEventsSQL = `INSERT INTO afs.mcp_events (at, request_id, team_id, agent_id, pool_id, worker_pod, server_id, server_kind, method,
  name, digest, outcome, reason, jsonrpc_code, http_status, gateway_ms, upstream_ms, request_bytes, response_bytes,
  protocol_downstream, search_results, search_hit, query_text)
SELECT e.at, e.request_id, e.team_id, e.agent_id, e.pool_id, e.worker_pod, e.server_id, e.server_kind, e.method, e.name, e.digest,
  e.outcome, e.reason, e.jsonrpc_code, e.http_status, e.gateway_ms, e.upstream_ms, e.request_bytes, e.response_bytes,
  e.protocol, CASE WHEN e.search_results = '' THEN NULL ELSE string_to_array(e.search_results, chr(31)) END, e.search_hit, e.query_text
FROM unnest($1::timestamptz[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::text[], $9::text[],
  $10::text[], $11::text[], $12::text[], $13::text[], $14::int[], $15::smallint[], $16::int[], $17::int[], $18::int[], $19::int[],
  $20::text[], $21::text[], $22::bool[], $23::text[])
  AS e(at, request_id, team_id, agent_id, pool_id, worker_pod, server_id, server_kind, method, name, digest, outcome, reason,
       jsonrpc_code, http_status, gateway_ms, upstream_ms, request_bytes, response_bytes, protocol, search_results, search_hit, query_text)`

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func optInt(v int) *int32 {
	if v == 0 {
		return nil
	}
	return i32p(v)
}

func (l *McpLedger) write(ctx context.Context, es []mcpgateway.Event) {
	n := len(es)
	at := make([]time.Time, 0, n)
	var reqID, team, agent, pool, pod, server, kind, method, name, digest, outcome, reason, protocol, results, query []*string
	var code, status, gw, up, reqB, respB []*int32
	var hit []*bool
	str := func(s string, max int) *string { return nullStr(clip(s, max)) }
	for _, e := range es {
		at = append(at, e.At)
		reqID = append(reqID, str(e.RequestID, 128))
		team = append(team, &e.Caller.TeamID)
		var a, p string
		if e.Caller.SubjectKind == "pool" {
			p = e.Caller.SubjectID
		} else {
			a = e.Caller.SubjectID
		}
		agent, pool, pod = append(agent, nullStr(a)), append(pool, nullStr(p)), append(pod, str(e.Caller.WorkerPod, 253))
		server, kind = append(server, nullStr(e.ServerID)), append(kind, nullStr(e.ServerKind))
		m := e.Method
		if m == "" {
			m = "unknown"
		}
		method, name = append(method, str(m, 64)), append(name, str(e.Name, 500))
		d := e.Digest
		if !digestRE.MatchString(d) {
			d = ""
		}
		digest, outcome, reason = append(digest, nullStr(d)), append(outcome, nullStr(e.Outcome)), append(reason, str(e.Reason, 64))
		code = append(code, optInt(e.JSONRPCCode))
		var st *int32
		if e.HTTPStatus >= 100 && e.HTTPStatus <= 599 {
			st = i32p(e.HTTPStatus)
		}
		status, gw, up = append(status, st), append(gw, i32p(max(e.GatewayMS, 0))), append(up, optInt(max(e.UpstreamMS, 0)))
		reqB, respB = append(reqB, i32p(max(e.RequestBytes, 0))), append(respB, i32p(max(e.ResponseBytes, 0)))
		protocol = append(protocol, str(e.Protocol, 20))
		var rs string
		for i, r := range e.SearchResults {
			if i == 50 {
				break
			}
			if i > 0 {
				rs += "\x1f"
			}
			rs += r
		}
		results, hit, query = append(results, &rs), append(hit, e.SearchHit), append(query, str(e.QueryText, 1000))
	}
	for i := range outcome {
		if outcome[i] == nil {
			o := mcpgateway.OutcomeOK
			outcome[i] = &o
		}
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := l.pool.Exec(wctx, insertMcpEventsSQL, at, reqID, team, agent, pool, pod, server, kind, method, name, digest, outcome, reason,
		code, status, gw, up, reqB, respB, protocol, results, hit, query); err != nil {
		l.dropped.Add(int64(n))
		l.log.Warn("mcp ledger batch dropped", "rows", n, "error", MapError(err, OpWrite, "mcp events").Error())
		return
	}
	l.written.Add(int64(n))
}
