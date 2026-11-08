// Package server assembles llmtee: storage, pipeline, proxy, UI, metrics
// and MCP behind one HTTP listener.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/garritfra/llmtee/internal/alerts"
	"github.com/garritfra/llmtee/internal/budget"
	"github.com/garritfra/llmtee/internal/config"
	"github.com/garritfra/llmtee/internal/keys"
	"github.com/garritfra/llmtee/internal/mcp"
	"github.com/garritfra/llmtee/internal/metrics"
	"github.com/garritfra/llmtee/internal/pipeline"
	"github.com/garritfra/llmtee/internal/pricing"
	"github.com/garritfra/llmtee/internal/proxy"
	"github.com/garritfra/llmtee/internal/redact"
	"github.com/garritfra/llmtee/internal/store"
	"github.com/garritfra/llmtee/internal/ui"
)

// Server is a running llmtee instance.
type Server struct {
	Cfg      *config.Config
	Store    store.Store // nil if the database could not be opened
	Pipeline *pipeline.Pipeline
	Handler  http.Handler
	Keys     *keys.Cache
	Budgets  *budget.Tracker
	Metrics  *metrics.M

	cancel context.CancelFunc
	bg     chan struct{}
}

// New builds a server. A database failure is logged, not fatal: the proxy
// keeps forwarding without logging. The returned server must be closed.
func New(ctx context.Context, cfg *config.Config, version string) (*Server, error) {
	pt, err := pricing.Load(cfg.PricesFile)
	if err != nil {
		return nil, err
	}
	s := &Server{Cfg: cfg, Keys: keys.NewCache(), Budgets: budget.New(), bg: make(chan struct{})}
	if cfg.UIAuth && cfg.AdminPassword == "" {
		b := make([]byte, 12)
		rand.Read(b)
		cfg.AdminPassword = hex.EncodeToString(b)
		slog.Warn("LLMTEE_ADMIN_PASSWORD not set; generated a temporary password for this run", "password", cfg.AdminPassword)
	}

	st, storeErr := store.Open(ctx, cfg.DatabaseURL)
	if storeErr != nil {
		slog.Error("database unavailable; proxying without logging", "err", storeErr)
	} else {
		s.Store = st
		if err := s.Keys.Reload(ctx, st); err != nil {
			slog.Error("loading keys", "err", err)
		}
		if err := s.Budgets.Load(ctx, st); err != nil {
			slog.Error("loading budget counters", "err", err)
		}
	}

	var pipe *pipeline.Pipeline
	s.Metrics = metrics.New(func() float64 {
		if pipe == nil {
			return 0
		}
		return float64(pipe.QueueLen())
	})
	notifier := alerts.New(cfg.Alerts, s.Metrics)
	if s.Store != nil {
		if ks, err := s.Store.ListKeys(ctx); err == nil {
			notifier.Prime(ks, s.Budgets.Spend, s.Budgets.Total())
		}
	}
	pipe = pipeline.New(pipeline.Options{QueueSize: cfg.QueueSize, BatchSize: cfg.BatchSize,
		FlushInterval: cfg.FlushInterval, MaxBodyBytes: cfg.MaxBodyBytes},
		s.Store, pt, s.Budgets, s.Metrics, notifier, redact.New(cfg.RedactFields))
	s.Pipeline = pipe

	px := proxy.New(cfg, s.Keys, s.Budgets, pipe, s.Metrics)
	ui.Version = version
	u, err := ui.New(cfg, s.Store, s.Keys, s.Budgets)
	if err != nil {
		return nil, err
	}
	u.StoreErr = storeErr

	mux := http.NewServeMux()
	for _, p := range proxy.Prefixes() {
		mux.Handle(p, px)
	}
	mux.Handle("/metrics", s.Metrics.Handler())
	mux.HandleFunc("/healthz", s.healthz)
	mux.Handle("/mcp", &mcp.Server{Store: s.Store, Keys: s.Keys, Budgets: s.Budgets, Authorized: u.Authorized, Version: version})
	u.Register(mux)
	s.Handler = mux

	bctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go pipe.Run()
	go func() {
		defer close(s.bg)
		if s.Store == nil {
			<-bctx.Done()
			return
		}
		done := make(chan struct{}, 2)
		go func() {
			pipeline.RunRetention(bctx, s.Store, cfg.BodyRetentionDays, cfg.RetentionDays, time.Hour)
			done <- struct{}{}
		}()
		go func() { s.syncLoop(bctx); done <- struct{}{} }()
		<-done
		<-done
	}()

	slog.Info("llmtee ready", "listen", cfg.Listen, "database", dbKind(s.Store), "prices", pt.Models(),
		"log_bodies", cfg.LogBodies, "body_retention_days", cfg.BodyRetentionDays, "keys", len(mustKeys(s)))
	if cfg.LogBodies {
		slog.Warn("body logging is ON: request and response bodies are stored and may contain sensitive data; set LLMTEE_LOG_BODIES=false to disable",
			"retention_days", cfg.BodyRetentionDays)
	}
	return s, nil
}

func mustKeys(s *Server) []*store.Key {
	if s.Store == nil {
		return nil
	}
	ks, _ := s.Store.ListKeys(context.Background())
	return ks
}

func dbKind(s store.Store) string {
	if s == nil {
		return "unavailable"
	}
	return s.Dialect()
}

// syncLoop reloads keys (changed via CLI or other instances) and, for
// PostgreSQL, budget counters shared between proxy instances.
func (s *Server) syncLoop(ctx context.Context) {
	keyTick := time.NewTicker(30 * time.Second)
	defer keyTick.Stop()
	var budgetC <-chan time.Time
	if s.Store.Dialect() == store.Postgres && s.Cfg.BudgetSync > 0 {
		t := time.NewTicker(s.Cfg.BudgetSync)
		defer t.Stop()
		budgetC = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-keyTick.C:
			if err := s.Keys.Reload(ctx, s.Store); err != nil && ctx.Err() == nil {
				slog.Warn("reloading keys", "err", err)
			}
		case <-budgetC:
			if err := s.Budgets.Load(ctx, s.Store); err != nil && ctx.Err() == nil {
				slog.Warn("syncing budgets", "err", err)
			}
		}
	}
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	res := map[string]any{"status": "ok", "queue": s.Pipeline.QueueLen()}
	if s.Store == nil {
		res["database"] = "unavailable"
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.Store.Ping(ctx); err != nil {
			res["database"] = "error: " + err.Error()
		} else {
			res["database"] = "ok"
		}
	}
	// The proxy works without a database, so health stays 200.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// Close flushes pending events and closes the database.
func (s *Server) Close() error {
	s.Pipeline.Close()
	s.cancel()
	<-s.bg
	if s.Store != nil {
		return s.Store.Close()
	}
	return nil
}

// ListenAndServe runs the HTTP server until ctx is canceled, then shuts down
// gracefully (in-flight requests get up to drain to finish).
func (s *Server) ListenAndServe(ctx context.Context, drain time.Duration) error {
	hs := &http.Server{Addr: s.Cfg.Listen, Handler: s.Handler, ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 120 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down", "drain", drain)
	sctx, cancel := context.WithTimeout(context.Background(), drain)
	defer cancel()
	err := hs.Shutdown(sctx)
	if errors.Is(err, context.DeadlineExceeded) {
		hs.Close()
	}
	return nil
}
