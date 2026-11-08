package pipeline

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/garritfra/llmtee/internal/budget"
	"github.com/garritfra/llmtee/internal/metrics"
	"github.com/garritfra/llmtee/internal/pricing"
	"github.com/garritfra/llmtee/internal/store"
	"github.com/garritfra/llmtee/internal/usage"
)

// failingStore simulates a read-only or broken database.
type failingStore struct {
	store.Store
	calls atomic.Int32
}

func (f *failingStore) Insert(context.Context, []*store.Request) error {
	f.calls.Add(1)
	return errors.New("attempt to write a readonly database")
}

func TestWriteFailuresAreContained(t *testing.T) {
	m := metrics.New(func() float64 { return 0 })
	fs := &failingStore{}
	b := budget.New()
	pt, _ := pricing.Load("")
	p := New(Options{QueueSize: 10, BatchSize: 2, FlushInterval: 10 * time.Millisecond, MaxBodyBytes: 100}, fs, pt, b, m, nil, nil)
	go p.Run()
	u := usage.Usage{Known: true, Model: "gpt-4o-mini", Input: 1000, Output: 100}
	for i := 0; i < 5; i++ {
		p.Submit(&Event{ID: "x", Start: time.Now(), Upstream: "openai", KeyID: "k", Status: 200, StreamUsage: &u})
	}
	p.Close()
	if fs.calls.Load() == 0 {
		t.Fatal("store never called")
	}
	// Budgets still track spend although the database write failed.
	if b.Spend("k").Day <= 0 {
		t.Error("budget not updated when DB fails")
	}
}

func TestQueueFullDropsInsteadOfBlocking(t *testing.T) {
	m := metrics.New(func() float64 { return 0 })
	p := New(Options{QueueSize: 1, BatchSize: 1, FlushInterval: time.Second}, nil, nil, nil, m, nil, nil)
	// Run is not started: the queue fills up immediately.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			p.Submit(&Event{ID: "x", Start: time.Now()})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Submit blocked on a full queue")
	}
	go p.Run()
	p.Close()
	p.Submit(&Event{}) // after close: no panic
}

func TestProcessBodiesAndErrors(t *testing.T) {
	p := New(Options{QueueSize: 1, BatchSize: 1, FlushInterval: time.Second, MaxBodyBytes: 10}, nil, nil, nil, nil, nil, nil)
	r := p.Process(&Event{ID: "1", Start: time.Now(), Format: usage.FormatOpenAI, Status: 401, RespComplete: true,
		Resp: []byte(`{"error":{"message":"Incorrect API key provided"}}`), Req: []byte("0123456789abcdef"), LogBodies: true})
	if r.Error != "Incorrect API key provided" {
		t.Errorf("error = %q", r.Error)
	}
	if string(r.ReqBody) != "0123456789" {
		t.Errorf("body not truncated: %q", r.ReqBody)
	}
	r = p.Process(&Event{ID: "2", Start: time.Now(), Status: 200, LogBodies: true, OmitReq: true, Req: []byte("secret"), Resp: []byte("{}")})
	if len(r.ReqBody) != 0 || string(r.RespBody) != "{}" {
		t.Errorf("omit request: %q %q", r.ReqBody, r.RespBody)
	}
	r = p.Process(&Event{ID: "3", Start: time.Now(), Status: 200, LogBodies: false, Req: []byte("x")})
	if r.ReqBody != nil || r.RespBody != nil {
		t.Error("bodies stored although logging disabled")
	}
}
