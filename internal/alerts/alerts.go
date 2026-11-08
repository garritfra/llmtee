// Package alerts sends budget threshold notifications via webhook and ntfy.
package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/garritfra/llmtee/internal/config"
	"github.com/garritfra/llmtee/internal/metrics"
	"github.com/garritfra/llmtee/internal/store"
)

// Alert is the webhook payload.
type Alert struct {
	Kind      string    `json:"kind"`  // "budget_threshold"
	Scope     string    `json:"scope"` // "key" or "global"
	KeyID     string    `json:"key_id,omitempty"`
	KeyName   string    `json:"key_name,omitempty"`
	Window    string    `json:"window"` // "daily" or "monthly"
	Threshold float64   `json:"threshold"`
	BudgetUSD float64   `json:"budget_usd"`
	SpentUSD  float64   `json:"spent_usd"`
	Time      time.Time `json:"time"`
	Message   string    `json:"message"`
}

// Notifier fires each (scope, window period, threshold) at most once.
type Notifier struct {
	cfg    config.Alerts
	m      *metrics.M
	client *http.Client
	mu     sync.Mutex
	fired  map[string]bool
	// Send can be replaced in tests.
	Send func(Alert)
	wg   sync.WaitGroup
}

// New returns a notifier, or nil when no channel is configured.
func New(cfg config.Alerts, m *metrics.M) *Notifier {
	if cfg.WebhookURL == "" && cfg.NtfyURL == "" {
		return nil
	}
	n := &Notifier{cfg: cfg, m: m, client: &http.Client{Timeout: 10 * time.Second}, fired: map[string]bool{}}
	n.Send = n.send
	return n
}

func period(window string, t time.Time) string {
	if window == "daily" {
		return t.UTC().Format("2006-01-02")
	}
	return t.UTC().Format("2006-01")
}

// Prime marks thresholds that are already crossed as fired so a restart
// does not re-send alerts.
func (n *Notifier) Prime(keys []*store.Key, spend func(id string) store.KeySpend, total store.KeySpend) {
	if n == nil {
		return
	}
	now := time.Now()
	for _, k := range keys {
		s := spend(k.ID)
		n.evaluate("key", k, s, now, false)
	}
	n.evaluateGlobal(total, now, false)
}

// Check evaluates a key's and the global spend after a request.
func (n *Notifier) Check(k *store.Key, keySpend, total store.KeySpend) {
	if n == nil {
		return
	}
	now := time.Now()
	if k != nil {
		n.evaluate("key", k, keySpend, now, true)
	}
	n.evaluateGlobal(total, now, true)
}

func (n *Notifier) evaluateGlobal(total store.KeySpend, now time.Time, send bool) {
	g := &store.Key{ID: "", Name: "all keys"}
	if n.cfg.GlobalDayUSD > 0 {
		g.BudgetDayUSD = &n.cfg.GlobalDayUSD
	}
	if n.cfg.GlobalMonthUSD > 0 {
		g.BudgetMonthUSD = &n.cfg.GlobalMonthUSD
	}
	n.evaluate("global", g, total, now, send)
}

func (n *Notifier) evaluate(scope string, k *store.Key, s store.KeySpend, now time.Time, send bool) {
	check := func(window string, budget *float64, spent float64) {
		if budget == nil || *budget <= 0 {
			return
		}
		for _, th := range n.cfg.Thresholds {
			if spent < th**budget {
				continue
			}
			id := fmt.Sprintf("%s|%s|%s|%s|%g", scope, k.ID, window, period(window, now), th)
			n.mu.Lock()
			already := n.fired[id]
			n.fired[id] = true
			n.mu.Unlock()
			if already || !send {
				continue
			}
			a := Alert{Kind: "budget_threshold", Scope: scope, KeyID: k.ID, KeyName: k.Name, Window: window,
				Threshold: th, BudgetUSD: *budget, SpentUSD: spent, Time: now.UTC()}
			who := "key " + k.Name
			if scope == "global" {
				who = "all keys"
			}
			a.Message = fmt.Sprintf("llmtee: %s reached %.0f%% of the %s budget ($%.2f of $%.2f)", who, th*100, window, spent, *budget)
			n.wg.Add(1)
			go func() {
				defer n.wg.Done()
				n.Send(a)
			}()
		}
	}
	check("daily", k.BudgetDayUSD, s.Day)
	check("monthly", k.BudgetMonthUSD, s.Month)
}

// Wait blocks until in-flight notifications are done.
func (n *Notifier) Wait() {
	if n != nil {
		n.wg.Wait()
	}
}

func (n *Notifier) send(a Alert) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if n.cfg.WebhookURL != "" {
		body, _ := json.Marshal(a)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, n.cfg.WebhookURL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		n.do("webhook", req)
	}
	if n.cfg.NtfyURL != "" {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, n.cfg.NtfyURL, bytes.NewReader([]byte(a.Message)))
		req.Header.Set("Title", "llmtee budget alert")
		req.Header.Set("Tags", "money_with_wings")
		if a.Threshold >= 1 {
			req.Header.Set("Priority", "high")
		}
		if n.cfg.NtfyToken != "" {
			req.Header.Set("Authorization", "Bearer "+n.cfg.NtfyToken)
		}
		n.do("ntfy", req)
	}
}

func (n *Notifier) do(channel string, req *http.Request) {
	resp, err := n.client.Do(req)
	result := "ok"
	if err != nil {
		result = "error"
		slog.Warn("alert delivery failed", "channel", channel, "err", err)
	} else {
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			result = "error"
			slog.Warn("alert delivery failed", "channel", channel, "status", resp.Status)
		}
	}
	if n.m != nil {
		n.m.AlertsSent.WithLabelValues(channel, result).Inc()
	}
}
