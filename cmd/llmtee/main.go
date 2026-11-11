// Command llmtee is a self-hosted proxy for LLM APIs that logs every request
// with tokens, cost and latency and enforces per-key budgets.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/garritfra/llmtee/internal/budget"
	"github.com/garritfra/llmtee/internal/config"
	"github.com/garritfra/llmtee/internal/keys"
	"github.com/garritfra/llmtee/internal/pricing"
	"github.com/garritfra/llmtee/internal/server"
	"github.com/garritfra/llmtee/internal/store"
	"github.com/garritfra/llmtee/internal/ui"
	"github.com/garritfra/llmtee/internal/usage"
)

// Set by goreleaser.
var (
	version = "dev"
	commit  = "none"
)

const usageText = `llmtee - self-hosted LLM request logger and cost proxy

Usage:
  llmtee [serve]                 run the proxy (default)
  llmtee keys list               list keys
  llmtee keys create NAME [--budget-day USD] [--budget-month USD]
  llmtee keys label NAME PROVIDER_KEY [--budget-day USD] [--budget-month USD]
                                 attribute an existing provider key (passthrough)
  llmtee keys budget ID [--budget-day USD] [--budget-month USD]   (empty = no limit)
  llmtee keys delete ID
  llmtee export [--range 30d] [--model M] [--key ID] [--user U] > requests.csv
  llmtee seed [--days 30] [--requests 3000]   fill the database with demo data
  llmtee healthcheck             exit 0 if the local server answers /healthz
  llmtee version

Global flags:
  --config FILE                  YAML config (or LLMTEE_CONFIG)

Configuration is read from environment variables, see docs/configuration.md.
`

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()})))
	args := os.Args[1:]
	cfgPath := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" || args[i] == "-config" {
			if i+1 < len(args) {
				cfgPath = args[i+1]
				args = append(args[:i], args[i+2:]...)
			}
			break
		}
		if v, ok := strings.CutPrefix(args[i], "--config="); ok {
			cfgPath = v
			args = append(args[:i], args[i+1:]...)
			break
		}
	}
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(cfgPath)
	case "keys":
		err = keysCmd(cfgPath, args)
	case "export":
		err = exportCmd(cfgPath, args)
	case "seed":
		err = seedCmd(cfgPath, args)
	case "healthcheck":
		err = healthcheck(cfgPath)
	case "version", "--version", "-v":
		fmt.Printf("llmtee %s (%s)\n", version, commit)
	case "help", "--help", "-h":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usageText)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func logLevel() slog.Level {
	switch strings.ToLower(os.Getenv("LLMTEE_LOG_LEVEL")) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

func serve(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv, err := server.New(ctx, cfg, version)
	if err != nil {
		return err
	}
	err = srv.ListenAndServe(ctx, 30*time.Second)
	if cerr := srv.Close(); err == nil {
		err = cerr
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func openStore(cfgPath string) (store.Store, *config.Config, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	s, err := store.Open(context.Background(), cfg.DatabaseURL)
	return s, cfg, err
}

// parseFlags separates positional args from --budget-day/--budget-month style flags.
func parseFlags(args []string, names ...string) (pos []string, vals map[string]string, err error) {
	vals = map[string]string{}
	allowed := map[string]bool{}
	for _, n := range names {
		allowed[n] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			pos = append(pos, a)
			continue
		}
		name, val, ok := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if !allowed[name] {
			return nil, nil, fmt.Errorf("unknown flag --%s", name)
		}
		if !ok {
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("--%s needs a value", name)
			}
			i++
			val = args[i]
		}
		vals[name] = val
	}
	return pos, vals, nil
}

func budgetFlag(vals map[string]string, name string) (*float64, error) {
	v, ok := vals[name]
	if !ok || v == "" {
		return nil, nil
	}
	f, err := strconv.ParseFloat(strings.TrimPrefix(v, "$"), 64)
	if err != nil || f < 0 {
		return nil, fmt.Errorf("--%s: invalid amount %q", name, v)
	}
	return &f, nil
}

func fmtBudget(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("$%.2f", *p)
}

func keysCmd(cfgPath string, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	s, _, err := openStore(cfgPath)
	if err != nil {
		return err
	}
	defer s.Close()
	ctx := context.Background()
	sub := args[0]
	pos, vals, err := parseFlags(args[1:], "budget-day", "budget-month")
	if err != nil {
		return err
	}
	day, err := budgetFlag(vals, "budget-day")
	if err != nil {
		return err
	}
	month, err := budgetFlag(vals, "budget-month")
	if err != nil {
		return err
	}
	switch sub {
	case "list":
		ks, err := s.ListKeys(ctx)
		if err != nil {
			return err
		}
		b := budget.New()
		if err := b.Load(ctx, s); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tKIND\tTODAY\tMONTH\tBUDGET/DAY\tBUDGET/MONTH")
		for _, k := range ks {
			sp := b.Spend(k.ID)
			fmt.Fprintf(tw, "%s\t%s\t%s\t$%.4f\t$%.4f\t%s\t%s\n", k.ID, k.Name, k.Kind, sp.Day, sp.Month, fmtBudget(k.BudgetDayUSD), fmtBudget(k.BudgetMonthUSD))
		}
		return tw.Flush()
	case "create":
		if len(pos) != 1 {
			return errors.New("usage: llmtee keys create NAME [--budget-day USD] [--budget-month USD]")
		}
		secret := keys.NewSecret()
		k := keys.New(keys.Managed, pos[0], secret, day, month)
		if err := s.CreateKey(ctx, k); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "created key %s (%s). Store the secret now, it is not shown again:\n", k.Name, k.ID)
		fmt.Println(secret)
		return nil
	case "label":
		if len(pos) != 2 {
			return errors.New("usage: llmtee keys label NAME PROVIDER_KEY [--budget-day USD] [--budget-month USD]")
		}
		k := keys.New(keys.Passthrough, pos[0], pos[1], day, month)
		if err := s.CreateKey(ctx, k); err != nil {
			return err
		}
		fmt.Printf("labeled passthrough key %s as %s\n", k.ID, k.Name)
		return nil
	case "budget":
		if len(pos) != 1 {
			return errors.New("usage: llmtee keys budget ID [--budget-day USD] [--budget-month USD]")
		}
		ks, err := s.ListKeys(ctx)
		if err != nil {
			return err
		}
		for _, k := range ks {
			if k.ID == pos[0] {
				k.BudgetDayUSD, k.BudgetMonthUSD = day, month
				if err := s.UpdateKey(ctx, k); err != nil {
					return err
				}
				fmt.Printf("%s: daily %s, monthly %s (a running proxy picks this up within 30s)\n", k.ID, fmtBudget(day), fmtBudget(month))
				return nil
			}
		}
		return fmt.Errorf("key %s not found", pos[0])
	case "delete":
		if len(pos) != 1 {
			return errors.New("usage: llmtee keys delete ID")
		}
		if err := s.DeleteKey(ctx, pos[0]); err != nil {
			return err
		}
		fmt.Printf("deleted %s\n", pos[0])
		return nil
	}
	return fmt.Errorf("unknown keys command %q", sub)
}

func exportCmd(cfgPath string, args []string) error {
	_, vals, err := parseFlags(args, "range", "model", "key", "user", "session", "upstream", "status")
	if err != nil {
		return err
	}
	s, _, err := openStore(cfgPath)
	if err != nil {
		return err
	}
	defer s.Close()
	q := map[string][]string{}
	for k, v := range vals {
		q[k] = []string{v}
	}
	if _, ok := q["range"]; !ok {
		q["range"] = []string{"all"}
	}
	f, _ := ui.FilterFromQuery(q, time.Now())
	kc := keys.NewCache()
	_ = kc.Reload(context.Background(), s)
	return ui.WriteCSV(context.Background(), os.Stdout, s, f, kc.Name)
}

func healthcheck(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	addr := cfg.Listen
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
	return nil
}

// seedCmd writes realistic demo data (for screenshots and the demo instance).
func seedCmd(cfgPath string, args []string) error {
	_, vals, err := parseFlags(args, "days", "requests")
	if err != nil {
		return err
	}
	days, n := 30, 3000
	if v := vals["days"]; v != "" {
		days, _ = strconv.Atoi(v)
	}
	if v := vals["requests"]; v != "" {
		n, _ = strconv.Atoi(v)
	}
	s, cfg, err := openStore(cfgPath)
	if err != nil {
		return err
	}
	defer s.Close()
	pt, err := pricing.Load(cfg.PricesFile)
	if err != nil {
		return err
	}
	ctx := context.Background()
	type demoKey struct {
		name   string
		day    *float64
		weight int
	}
	f := func(v float64) *float64 { return &v }
	demo := []demoKey{{"hermes-agent", f(5), 5}, {"blog-pipeline", nil, 2}, {"n8n-workflows", f(3), 3}, {"claude-code", nil, 4}}
	var ks []*store.Key
	existing, _ := s.ListKeys(ctx)
	for _, d := range demo {
		var found *store.Key
		for _, e := range existing {
			if e.Name == d.name {
				found = e
			}
		}
		if found == nil {
			found = keys.New(keys.Managed, d.name, keys.NewSecret(), d.day, f(60))
			if err := s.CreateKey(ctx, found); err != nil {
				return err
			}
		}
		for i := 0; i < d.weight; i++ {
			ks = append(ks, found)
		}
	}
	type model struct {
		upstream, name, path string
		in, out              int
		stream               bool
		weight               int
	}
	models := []model{
		{"anthropic", "claude-sonnet-4-5-20250929", "/v1/messages", 9000, 700, true, 6},
		{"anthropic", "claude-haiku-4-5-20251001", "/v1/messages", 3000, 300, true, 4},
		{"openai", "gpt-4o-mini-2024-07-18", "/v1/chat/completions", 1500, 250, true, 6},
		{"openai", "gpt-4.1-2025-04-14", "/v1/responses", 4000, 600, false, 3},
		{"openai", "text-embedding-3-small", "/v1/embeddings", 800, 0, false, 3},
		{"openrouter", "deepseek/deepseek-chat-v3.1", "/v1/chat/completions", 2500, 400, true, 2},
		{"ollama", "llama3.1:8b", "/v1/chat/completions", 1200, 300, true, 3},
	}
	var pool []model
	for _, m := range models {
		for i := 0; i < m.weight; i++ {
			pool = append(pool, m)
		}
	}
	users := []string{"garrit", "alice", "bob", "", ""}
	features := []string{"summarize", "classify", "chat", "extract"}
	now := time.Now()
	var batch []*store.Request
	for i := 0; i < n; i++ {
		age := time.Duration(rand.Float64() * rand.Float64() * float64(days) * 24 * float64(time.Hour))
		ts := now.Add(-age)
		m := pool[rand.IntN(len(pool))]
		k := ks[rand.IntN(len(ks))]
		in := int64(float64(m.in) * (0.3 + rand.Float64()*1.4))
		out := int64(float64(m.out) * (0.2 + rand.Float64()*1.6))
		var cacheRead, cacheWrite int64
		if m.upstream == "anthropic" && rand.IntN(3) > 0 {
			cacheRead = in * 4
			if rand.IntN(4) == 0 {
				cacheWrite = in / 2
			}
		}
		u := usage.Usage{Known: true, Model: m.name, Input: in, Output: out, CacheRead: cacheRead, CacheWrite: cacheWrite}
		if m.upstream == "openrouter" { // OpenRouter reports the cost itself
			c := float64(in)*0.27e-6 + float64(out)*1.1e-6
			u.Cost = &c
		}
		status := 200
		errStr := ""
		if rand.IntN(60) == 0 {
			status, errStr, u.Known = 529, "Overloaded", false
		}
		lat := int64(300 + rand.IntN(2000) + int(out)*12)
		r := &store.Request{ID: ulid.MustNew(ulid.Timestamp(ts), ulid.DefaultEntropy()).String(), TS: ts.UnixMilli(),
			Upstream: m.upstream, Path: m.path, Model: m.name, KeyID: k.ID, UserID: users[rand.IntN(len(users))],
			Status: status, Streamed: m.stream, LatencyMS: lat, Error: errStr,
			Properties: map[string]string{"feature": features[rand.IntN(len(features))]}}
		if rand.IntN(5) == 0 {
			r.SessionID = fmt.Sprintf("sess-%04d", rand.IntN(200))
		}
		if m.stream {
			t := int64(150 + rand.IntN(900))
			r.TTFTMS = &t
		}
		if u.Known {
			r.TokensIn, r.TokensOut, r.TokensCacheRead, r.TokensCacheWrite = &u.Input, &u.Output, &u.CacheRead, &u.CacheWrite
			r.CostUSD = pt.Compute(m.upstream, []string{m.name}, u)
			if m.upstream == "ollama" {
				zero := 0.0
				r.CostUSD = &zero
			}
		}
		batch = append(batch, r)
		if len(batch) == 500 {
			if err := s.Insert(ctx, batch); err != nil {
				return err
			}
			batch = nil
		}
	}
	if err := s.Insert(ctx, batch); err != nil {
		return err
	}
	fmt.Printf("seeded %d requests over %d days\n", n, days)
	return nil
}
