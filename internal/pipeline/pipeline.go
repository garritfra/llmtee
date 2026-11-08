// Package pipeline is the asynchronous half of llmtee: it receives log
// events from the request path through a buffered channel, computes usage
// and cost, updates budgets, metrics and alerts and writes batches to the
// database. Nothing here can block or fail a proxied request.
package pipeline

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/garritfra/llmtee/internal/alerts"
	"github.com/garritfra/llmtee/internal/budget"
	"github.com/garritfra/llmtee/internal/metrics"
	"github.com/garritfra/llmtee/internal/pricing"
	"github.com/garritfra/llmtee/internal/redact"
	"github.com/garritfra/llmtee/internal/store"
	"github.com/garritfra/llmtee/internal/usage"
)

// Event is emitted by the proxy once a request has finished.
type Event struct {
	ID         string
	Start      time.Time
	Upstream   string
	Format     string // usage.FormatOpenAI / usage.FormatAnthropic
	Path       string
	ReqModel   string
	Key        *store.Key // nil for unknown passthrough keys
	KeyID      string
	UserID     string
	SessionID  string
	Properties map[string]string
	Status     int
	Streamed   bool
	LatencyMS  int64
	TTFTMS     *int64
	Error      string

	// StreamUsage is parsed inline for streams; for non-streamed responses
	// usage is parsed here from Resp.
	StreamUsage *usage.Usage
	// Resp holds the response body copy (complete if RespComplete).
	Resp         []byte
	RespComplete bool

	LogBodies bool
	OmitReq   bool // Llmtee-Omit-Request / Helicone-Omit-Request
	OmitResp  bool // Llmtee-Omit-Response / Helicone-Omit-Response
	Req       []byte
}

// Options configures a pipeline.
type Options struct {
	QueueSize     int
	BatchSize     int
	FlushInterval time.Duration
	MaxBodyBytes  int64
}

// Pipeline processes and persists events.
type Pipeline struct {
	opts    Options
	in      chan *Event
	store   store.Store // may be nil if the database is unavailable
	pricing *pricing.Table
	budgets *budget.Tracker
	metrics *metrics.M
	alerts  *alerts.Notifier
	redact  *redact.Redactor

	mu     sync.RWMutex
	closed bool
	done   chan struct{}

	// OnWrite is called after each batch attempt (tests).
	OnWrite func(n int, err error)
}

// New creates a pipeline. Call Run in a goroutine.
func New(opts Options, s store.Store, pt *pricing.Table, b *budget.Tracker, m *metrics.M, a *alerts.Notifier, r *redact.Redactor) *Pipeline {
	return &Pipeline{opts: opts, in: make(chan *Event, opts.QueueSize), store: s, pricing: pt, budgets: b,
		metrics: m, alerts: a, redact: r, done: make(chan struct{})}
}

// QueueLen returns the number of pending events.
func (p *Pipeline) QueueLen() int { return len(p.in) }

// Submit enqueues an event without blocking. If the queue is full the event
// is dropped and counted.
func (p *Pipeline) Submit(ev *Event) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return
	}
	select {
	case p.in <- ev:
	default:
		if p.metrics != nil {
			p.metrics.EventsDropped.Inc()
		}
	}
}

// Close stops accepting events, drains the queue and waits for the final write.
func (p *Pipeline) Close() {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.in)
	}
	p.mu.Unlock()
	<-p.done
	p.alerts.Wait()
}

// Run processes events until Close is called.
func (p *Pipeline) Run() {
	defer close(p.done)
	t := time.NewTicker(p.opts.FlushInterval)
	defer t.Stop()
	batch := make([]*store.Request, 0, p.opts.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		p.write(batch)
		batch = make([]*store.Request, 0, p.opts.BatchSize)
	}
	for {
		select {
		case ev, ok := <-p.in:
			if !ok {
				flush()
				return
			}
			if r := p.safeProcess(ev); r != nil {
				batch = append(batch, r)
			}
			if len(batch) >= p.opts.BatchSize {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}

func (p *Pipeline) write(batch []*store.Request) {
	if p.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := p.store.Insert(ctx, batch)
	if err != nil {
		slog.Error("writing request log batch failed; events lost", "events", len(batch), "err", err)
		if p.metrics != nil {
			p.metrics.DBWriteErrors.Inc()
		}
	} else if p.metrics != nil {
		p.metrics.DBWritten.Add(float64(len(batch)))
	}
	if p.OnWrite != nil {
		p.OnWrite(len(batch), err)
	}
}

func (p *Pipeline) safeProcess(ev *Event) (r *store.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("processing event panicked", "id", ev.ID, "panic", rec)
			r = nil
		}
	}()
	return p.Process(ev)
}

func i64(v int64) *int64 { return &v }

// Process turns an event into a database row and updates budgets, metrics
// and alerts. Exported for tests.
func (p *Pipeline) Process(ev *Event) *store.Request {
	var u usage.Usage
	switch {
	case ev.StreamUsage != nil:
		u = *ev.StreamUsage
	case ev.RespComplete && len(ev.Resp) > 0:
		u = usage.ParseBody(ev.Format, ev.Resp)
	}
	model := u.Model
	if model == "" {
		model = ev.ReqModel
	}
	r := &store.Request{
		ID: ev.ID, TS: ev.Start.UnixMilli(), Upstream: ev.Upstream, Path: ev.Path, Model: model,
		KeyID: ev.KeyID, UserID: ev.UserID, SessionID: ev.SessionID, Status: ev.Status, Streamed: ev.Streamed,
		LatencyMS: ev.LatencyMS, TTFTMS: ev.TTFTMS, Error: ev.Error, Properties: ev.Properties,
	}
	if u.Known {
		r.TokensIn, r.TokensOut, r.TokensCacheRead, r.TokensCacheWrite = i64(u.Input), i64(u.Output), i64(u.CacheRead), i64(u.CacheWrite)
		if p.pricing != nil {
			r.CostUSD = p.pricing.Compute(ev.Upstream, []string{u.Model, ev.ReqModel}, u)
		}
	}

	if r.CostUSD != nil && *r.CostUSD > 0 && p.budgets != nil {
		ks, total := p.budgets.Add(ev.KeyID, *r.CostUSD, ev.Start)
		p.alerts.Check(ev.Key, ks, total)
	}

	if m := p.metrics; m != nil {
		m.Requests.WithLabelValues(ev.Upstream, model, strconv.Itoa(ev.Status)).Inc()
		m.Latency.WithLabelValues(ev.Upstream).Observe(float64(ev.LatencyMS) / 1000)
		if ev.TTFTMS != nil {
			m.TTFT.WithLabelValues(ev.Upstream).Observe(float64(*ev.TTFTMS) / 1000)
		}
		if u.Known {
			m.Tokens.WithLabelValues(ev.Upstream, model, "input").Add(float64(u.Input))
			m.Tokens.WithLabelValues(ev.Upstream, model, "output").Add(float64(u.Output))
			m.Tokens.WithLabelValues(ev.Upstream, model, "cache_read").Add(float64(u.CacheRead))
			m.Tokens.WithLabelValues(ev.Upstream, model, "cache_write").Add(float64(u.CacheWrite))
		} else if ev.Status > 0 && ev.Status < 400 {
			m.UsageUnknown.WithLabelValues(ev.Upstream).Inc()
		}
		if r.CostUSD != nil {
			m.Cost.WithLabelValues(ev.Upstream, model).Add(*r.CostUSD)
		}
	}

	if r.Error == "" && ev.Status >= 400 {
		r.Error = errorMessage(ev.Resp)
	}

	if ev.LogBodies && (!ev.OmitReq || !ev.OmitResp) {
		r.ReqBody, r.RespBody = []byte{}, []byte{}
		if !ev.OmitReq {
			r.ReqBody = p.redact.JSON(p.truncate(ev.Req))
		}
		if !ev.OmitResp {
			resp := p.truncate(ev.Resp)
			if ev.Streamed {
				resp = p.redact.SSE(resp)
			} else {
				resp = p.redact.JSON(resp)
			}
			r.RespBody = resp
		}
	}
	return r
}

// errorMessage extracts error.message (OpenAI and Anthropic) from a body.
func errorMessage(b []byte) string {
	var e struct {
		Error json.RawMessage `json:"error"`
	}
	if len(b) == 0 || json.Unmarshal(b, &e) != nil || len(e.Error) == 0 {
		return ""
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(e.Error, &obj) == nil && obj.Message != "" {
		return truncateString(obj.Message, 500)
	}
	var s string
	if json.Unmarshal(e.Error, &s) == nil {
		return truncateString(s, 500)
	}
	return ""
}

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (p *Pipeline) truncate(b []byte) []byte {
	if max := p.opts.MaxBodyBytes; max > 0 && int64(len(b)) > max {
		return b[:max]
	}
	return b
}

// RunRetention deletes old bodies and requests periodically until ctx ends.
func RunRetention(ctx context.Context, s store.Store, bodyDays, requestDays int, every time.Duration) {
	run := func() {
		now := time.Now()
		var bodies, reqs int64
		if bodyDays > 0 {
			bodies = now.AddDate(0, 0, -bodyDays).UnixMilli()
		}
		if requestDays > 0 {
			reqs = now.AddDate(0, 0, -requestDays).UnixMilli()
		}
		if bodies == 0 && reqs == 0 {
			return
		}
		rctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		n, err := s.Retention(rctx, bodies, reqs)
		if err != nil {
			slog.Error("retention failed", "err", err)
		} else if n > 0 {
			slog.Info("retention deleted old rows", "rows", n)
		}
	}
	run()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}
