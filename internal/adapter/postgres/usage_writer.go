package postgres

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

// UsageWriter implements gateway.UsageSink: it queues rows and writes them in batches (one INSERT per batch), at
// most FlushEvery after the first row of a batch or when MaxBatch rows are waiting (gateway contract
// §8, architecture A-9). Record never blocks the call: when the queue is full the row is dropped and
// counted, because a metering gap is better than a stalled model call.
type UsageWriter struct {
	pool       *pgxpool.Pool
	log        *slog.Logger
	flushEvery time.Duration
	maxBatch   int
	attempts   int
	recorder   gateway.UsageDropRecorder
	ctx        context.Context //nolint:containedctx // the writer's own lifetime, set by Run
	ch         chan gateway.Record
	dropped    atomic.Int64
	failed     atomic.Int64
	written    atomic.Int64
	wg         sync.WaitGroup
	closeOnce  sync.Once
	done       chan struct{}
}

// UsageWriterConfig tunes a UsageWriter; zero values take the contract's.
type UsageWriterConfig struct {
	FlushEvery time.Duration // default 500 ms (the contract allows at most 1 s)
	MaxBatch   int           // default 500
	Queue      int           // default 20000
	// Attempts is how often a failing batch is tried before it is dropped. Default 5.
	Attempts int
	// Recorder is told of every dropped row (reason queue_full or write_failed); optional.
	Recorder gateway.UsageDropRecorder
}

// NewUsageWriter returns a writer; call Run before use and Close at shutdown.
func NewUsageWriter(pool *pgxpool.Pool, log *slog.Logger, cfg UsageWriterConfig) *UsageWriter {
	if cfg.FlushEvery <= 0 {
		cfg.FlushEvery = 500 * time.Millisecond
	}
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = 500
	}
	if cfg.Queue <= 0 {
		cfg.Queue = 20000
	}
	if cfg.Attempts <= 0 {
		cfg.Attempts = 5
	}
	if log == nil {
		log = slog.Default()
	}
	return &UsageWriter{pool: pool, log: log, flushEvery: cfg.FlushEvery, maxBatch: cfg.MaxBatch, attempts: cfg.Attempts, recorder: cfg.Recorder,
		ch: make(chan gateway.Record, cfg.Queue), done: make(chan struct{})}
}

var _ gateway.UsageSink = (*UsageWriter)(nil)

// Record implements gateway.UsageSink.
func (w *UsageWriter) Record(r gateway.Record) {
	select {
	case w.ch <- r:
	default:
		w.dropped.Add(1)
		w.report(gateway.UsageQueueFull, 1)
	}
}

func (w *UsageWriter) report(reason gateway.UsageDropReason, rows int) {
	if w.recorder != nil {
		w.recorder.UsageRowsDropped(reason, rows)
	}
}

// Dropped is the number of rows dropped because the queue was full; Failed the rows lost after every
// attempt failed; Written the rows stored.
func (w *UsageWriter) Dropped() int64 { return w.dropped.Load() }

// Failed returns the rows lost after every attempt failed.
func (w *UsageWriter) Failed() int64 { return w.failed.Load() }

// Written returns the rows stored.
func (w *UsageWriter) Written() int64 { return w.written.Load() }

// Run starts the writer; it returns at once. Every batch write derives its timeout from ctx, so a
// cancelled ctx fails the writes in flight; the flush Close triggers derives from ctx with
// context.WithoutCancel and so completes after ctx is gone. Close stops the writer after the queue is
// written. The composition root passes a context that outlives the HTTP drain, so rows recorded by
// in-flight calls are still written.
func (w *UsageWriter) Run(ctx context.Context) {
	w.ctx = ctx
	w.wg.Go(w.loop)
}

// Close writes what is queued and stops the writer.
func (w *UsageWriter) Close() {
	w.closeOnce.Do(func() { close(w.done) })
	w.wg.Wait()
}

func (w *UsageWriter) loop() {
	batch := make([]gateway.Record, 0, w.maxBatch)
	timer := time.NewTimer(w.flushEvery)
	defer timer.Stop()
	ctx := w.ctx
	flush := func() {
		if len(batch) > 0 {
			w.write(ctx, batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case r := <-w.ch:
			if len(batch) == 0 {
				timer.Reset(w.flushEvery)
			}
			batch = append(batch, r)
			if len(batch) >= w.maxBatch {
				flush()
			}
		case <-timer.C:
			flush()
			timer.Reset(w.flushEvery)
		case <-w.done:
			ctx = context.WithoutCancel(w.ctx) // the shutdown flush must outlive the parent
			for {
				select {
				case r := <-w.ch:
					batch = append(batch, r)
					if len(batch) >= w.maxBatch {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// nullStr maps an empty string to NULL.
func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// insertUsageSQL writes a whole batch in one statement. COPY would be the natural bulk path, but
// usage_events has row-level security enabled and PostgreSQL refuses COPY FROM on such a table
// ("COPY FROM not supported with row-level security"), so the batch goes through unnest.
const insertUsageSQL = `INSERT INTO afs.usage_events
 (occurred_at, team_id, agent_id, pool_id, worker_pod, model_id, request_id, outcome,
  input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd, latency_ms, tag_set_id,
  endpoint_id, model_name, usage_source, reasoning_tokens, ttft_ms, upstream_ms, gateway_ms, http_status)
SELECT * FROM unnest($1::timestamptz[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::text[],
  $9::int[], $10::int[], $11::int[], $12::int[], $13::numeric[], $14::int[], $15::text[],
  $16::text[], $17::text[], $18::text[], $19::bigint[], $20::int[], $21::int[], $22::int[], $23::smallint[])`

// routeCols are the route columns of one row (gateway contract §12.6); NULL on a Bedrock call.
type routeCols struct {
	endpoint, model, source               []*string
	reasoning                             []*int64
	ttft, upstream, gatewayMS, httpStatus []*int32
}

func i32p(v int) *int32 {
	x := int32(v) //nolint:gosec // timings and statuses are small
	return &x
}

func (rc *routeCols) add(r *gateway.RouteFields) {
	if r == nil {
		rc.endpoint, rc.model, rc.source = append(rc.endpoint, nil), append(rc.model, nil), append(rc.source, nil)
		rc.reasoning = append(rc.reasoning, nil)
		rc.ttft, rc.upstream, rc.gatewayMS, rc.httpStatus = append(rc.ttft, nil), append(rc.upstream, nil), append(rc.gatewayMS, nil), append(rc.httpStatus, nil)
		return
	}
	rs := int64(r.Reasoning)
	var st *int32
	if r.HTTPStatus >= 100 && r.HTTPStatus <= 599 {
		st = i32p(r.HTTPStatus)
	}
	rc.endpoint, rc.model, rc.source = append(rc.endpoint, nullStr(r.EndpointID)), append(rc.model, nullStr(r.Model)), append(rc.source, nullStr(r.Source))
	rc.reasoning = append(rc.reasoning, &rs)
	rc.ttft, rc.upstream, rc.gatewayMS, rc.httpStatus = append(rc.ttft, i32p(r.TTFTMS)), append(rc.upstream, i32p(r.UpstreamMS)), append(rc.gatewayMS, i32p(r.GatewayMS)), append(rc.httpStatus, st)
}

func (w *UsageWriter) write(parent context.Context, batch []gateway.Record) {
	var (
		at                    = make([]time.Time, 0, len(batch))
		team, req, outc       = make([]string, 0, len(batch)), make([]string, 0, len(batch)), make([]string, 0, len(batch))
		model                 = make([]*string, 0, len(batch))
		rc                    routeCols
		agent, pool, pod, tag = make([]*string, 0, len(batch)), make([]*string, 0, len(batch)), make([]*string, 0, len(batch)), make([]*string, 0, len(batch))
		in, out, cr, cw, lat  = make([]int32, 0, len(batch)), make([]int32, 0, len(batch)), make([]int32, 0, len(batch)), make([]int32, 0, len(batch)), make([]int32, 0, len(batch))
		cost                  = make([]pgtype.Numeric, 0, len(batch))
	)
	for _, r := range batch {
		var c pgtype.Numeric
		if err := c.Scan(r.Cost.String()); err != nil {
			w.failed.Add(1)
			w.report(gateway.UsageWriteFailed, 1)
			w.log.Error("usage row dropped: cost", "err", err)
			continue
		}
		at, team, model, req, outc = append(at, r.OccurredAt), append(team, r.TeamID), append(model, nullStr(r.ModelID)), append(req, r.RequestID), append(outc, string(r.Outcome))
		agent, pool, pod, tag = append(agent, nullStr(r.AgentID)), append(pool, nullStr(r.PoolID)), append(pod, nullStr(r.WorkerPod)), append(tag, nullStr(r.TagSetID))
		in, out = append(in, int32(r.Usage.Input)), append(out, int32(r.Usage.Output))       //nolint:gosec // token counts are bounded by the model
		cr, cw = append(cr, int32(r.Usage.CacheRead)), append(cw, int32(r.Usage.CacheWrite)) //nolint:gosec // token counts are bounded by the model
		lat, cost = append(lat, int32(r.LatencyMS)), append(cost, c)                         //nolint:gosec // latency is bounded by the request timeout
		rc.add(r.Route)
	}
	if len(at) == 0 {
		return
	}
	var err error
	for attempt := 1; attempt <= w.attempts; attempt++ {
		ctx, cancel := context.WithTimeout(parent, 5*time.Second)
		_, err = w.pool.Exec(ctx, insertUsageSQL, at, team, agent, pool, pod, model, req, outc, in, out, cr, cw, cost, lat, tag,
			rc.endpoint, rc.model, rc.source, rc.reasoning, rc.ttft, rc.upstream, rc.gatewayMS, rc.httpStatus)
		cancel()
		if err == nil {
			w.written.Add(int64(len(at)))
			return
		}
		time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
	}
	w.failed.Add(int64(len(at)))
	w.report(gateway.UsageWriteFailed, len(at))
	// The error carries no row content (no prompt or completion is ever in a record).
	w.log.Error("usage batch dropped", "rows", len(at), "err", err)
}
