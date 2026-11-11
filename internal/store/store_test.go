package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// backends returns the stores under test: always SQLite, plus PostgreSQL
// when LLMTEE_TEST_POSTGRES_URL is set (CI matrix).
func backends(t *testing.T) map[string]Store {
	t.Helper()
	ctx := context.Background()
	out := map[string]Store{}
	s, err := Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	out[SQLite] = s
	if url := os.Getenv("LLMTEE_TEST_POSTGRES_URL"); url != "" {
		// Fresh schema per test.
		schema := fmt.Sprintf("t%d", time.Now().UnixNano())
		admin, err := sql.Open("pgx", url)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			admin.Exec("DROP SCHEMA " + schema + " CASCADE")
			admin.Close()
		})
		sep := "?"
		if containsQ(url) {
			sep = "&"
		}
		p, err := Open(ctx, url+sep+"search_path="+schema)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { p.Close() })
		out[Postgres] = p
	} else if os.Getenv("CI") != "" && os.Getenv("LLMTEE_REQUIRE_POSTGRES") != "" {
		t.Fatal("LLMTEE_TEST_POSTGRES_URL not set")
	}
	return out
}

func containsQ(s string) bool {
	for _, c := range s {
		if c == '?' {
			return true
		}
	}
	return false
}

func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }

func req(ts time.Time, model, key, user string, cost *float64) *Request {
	return &Request{
		ID: ulid.MustNew(ulid.Timestamp(ts), ulid.DefaultEntropy()).String(), TS: ts.UnixMilli(),
		Upstream: "openai", Path: "/v1/chat/completions", Model: model, KeyID: key, UserID: user,
		Status: 200, Streamed: true, TokensIn: i64(100), TokensOut: i64(20), TokensCacheRead: i64(5), TokensCacheWrite: i64(0),
		CostUSD: cost, LatencyMS: 120, TTFTMS: i64(40),
	}
}

func TestStore(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) { testStore(t, s) })
	}
}

func testStore(t *testing.T, s Store) {
	ctx := context.Background()
	now := time.Now()
	day := 24 * time.Hour

	a := req(now.Add(-time.Minute), "gpt-4o", "mk-a", "alice", f64(0.5))
	a.Properties = map[string]string{"feature": "summarize", "env": "prod"}
	a.ReqBody, a.RespBody = []byte(`{"model":"gpt-4o"}`), []byte("data: {}\n\n")
	b := req(now.Add(-2*time.Minute), "claude", "mk-b", "bob", f64(1.25))
	c := req(now.Add(-40*day), "gpt-4o", "mk-a", "", nil) // old
	c.Status, c.Error = 500, "boom"
	d := req(now.Add(-3*time.Minute), "llama", "", "", nil) // usage known, price unknown
	if err := s.Insert(ctx, []*Request{a, b, c, d}); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(ctx, nil); err != nil {
		t.Fatal(err)
	}

	// List, newest first, with properties.
	got, err := s.ListRequests(ctx, Filter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0].ID != a.ID || got[3].ID != c.ID {
		t.Fatalf("list order wrong: %v", ids(got))
	}
	if got[0].Properties["feature"] != "summarize" || *got[0].CostUSD != 0.5 || !got[0].Streamed || *got[0].TTFTMS != 40 {
		t.Errorf("row = %+v", got[0])
	}
	if got[3].CostUSD != nil || got[3].Error != "boom" {
		t.Errorf("null cost / error not preserved: %+v", got[3])
	}

	// Filters.
	check := func(f Filter, want ...string) {
		t.Helper()
		f.Limit = 100
		got, err := s.ListRequests(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(ids(got)) != fmt.Sprint(want) {
			t.Errorf("filter %+v = %v, want %v", f, ids(got), want)
		}
	}
	check(Filter{Model: "gpt-4o"}, a.ID, c.ID)
	check(Filter{KeyID: "mk-b"}, b.ID)
	check(Filter{UserID: "alice"}, a.ID)
	check(Filter{Status: "error"}, c.ID)
	check(Filter{Status: "ok"}, a.ID, b.ID, d.ID)
	check(Filter{Since: now.Add(-day).UnixMilli()}, a.ID, b.ID, d.ID)
	check(Filter{PropName: "feature", PropValue: "summarize"}, a.ID)
	check(Filter{PropName: "env"}, a.ID)
	check(Filter{Before: b.ID}, d.ID, c.ID)

	// Detail with bodies.
	r, err := s.GetRequest(ctx, a.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !r.HasBody || string(r.ReqBody) != `{"model":"gpt-4o"}` || string(r.RespBody) != "data: {}\n\n" {
		t.Errorf("bodies = %q %q", r.ReqBody, r.RespBody)
	}
	r, _ = s.GetRequest(ctx, a.ID, false)
	if !r.HasBody || r.ReqBody != nil {
		t.Error("withBodies=false should not load bodies")
	}
	if _, err := s.GetRequest(ctx, "nope", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing = %v", err)
	}

	// Aggregations.
	tot, err := s.Spend(ctx, Filter{}, ByNone)
	if err != nil {
		t.Fatal(err)
	}
	if len(tot) != 1 || tot[0].Requests != 4 || tot[0].CostUSD != 1.75 || tot[0].TokensIn != 400 || tot[0].Errors != 1 || tot[0].Unpriced != 2 {
		t.Errorf("total = %+v", tot)
	}
	byModel, err := s.Spend(ctx, Filter{Since: now.Add(-day).UnixMilli()}, ByModel)
	if err != nil {
		t.Fatal(err)
	}
	if len(byModel) != 3 || byModel[0].Group != "claude" || byModel[0].CostUSD != 1.25 {
		t.Errorf("by model = %+v", byModel)
	}
	byDay, err := s.Spend(ctx, Filter{}, ByDay)
	if err != nil {
		t.Fatal(err)
	}
	if len(byDay) < 2 || byDay[len(byDay)-1].Group != now.UTC().Format("2006-01-02") && byDay[len(byDay)-1].Group != now.Add(-3*time.Minute).UTC().Format("2006-01-02") {
		t.Errorf("by day = %+v", byDay)
	}
	for _, g := range []string{ByKey, ByUser, ByUpstream, BySession} {
		if _, err := s.Spend(ctx, Filter{}, g); err != nil {
			t.Errorf("group %s: %v", g, err)
		}
	}
	if _, err := s.Spend(ctx, Filter{}, "drop table"); err == nil {
		t.Error("invalid group accepted")
	}

	// Budget spend.
	dayStart := now.Add(-time.Hour).UnixMilli()
	per, total, err := s.BudgetSpend(ctx, dayStart, now.Add(-day).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if per["mk-a"].Day != 0.5 || per["mk-b"].Month != 1.25 || total.Day != 1.75 {
		t.Errorf("budget spend = %+v total %+v", per, total)
	}

	models, err := s.Distinct(ctx, "model", 0)
	if err != nil || len(models) != 3 {
		t.Errorf("distinct = %v %v", models, err)
	}
	if _, err := s.Distinct(ctx, "1; drop", 0); err == nil {
		t.Error("invalid distinct column accepted")
	}

	// Keys.
	k := &Key{ID: "mk-1", Name: "agent", Hash: "h1", Kind: "managed", BudgetDayUSD: f64(5), Created: now.UnixMilli()}
	if err := s.CreateKey(ctx, k); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateKey(ctx, &Key{ID: "mk-2", Name: "dup", Hash: "h1", Kind: "managed", Created: 1}); err == nil {
		t.Error("duplicate hash accepted")
	}
	k.Name, k.BudgetDayUSD, k.BudgetMonthUSD = "agent2", nil, f64(100)
	if err := s.UpdateKey(ctx, k); err != nil {
		t.Fatal(err)
	}
	ks, err := s.ListKeys(ctx)
	if err != nil || len(ks) != 1 || ks[0].Name != "agent2" || ks[0].BudgetDayUSD != nil || *ks[0].BudgetMonthUSD != 100 {
		t.Fatalf("keys = %+v %v", ks, err)
	}
	if err := s.UpdateKey(ctx, &Key{ID: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update missing = %v", err)
	}
	if err := s.DeleteKey(ctx, "mk-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteKey(ctx, "mk-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete missing = %v", err)
	}

	// Retention: bodies older than 30s go (a is older), requests older than 30 days go (c).
	if _, err := s.Retention(ctx, now.Add(-30*time.Second).UnixMilli(), now.Add(-30*day).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	r, _ = s.GetRequest(ctx, a.ID, true)
	if r.HasBody {
		t.Error("body not deleted by retention")
	}
	if _, err := s.GetRequest(ctx, c.ID, false); !errors.Is(err, ErrNotFound) {
		t.Error("old request not deleted by retention")
	}
	if err := s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSeries(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) { testSeries(t, s) })
	}
}

func testSeries(t *testing.T, s Store) {
	ctx := context.Background()
	h0 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	if err := s.Insert(ctx, []*Request{
		req(h0.Add(5*time.Minute), "gpt-4o", "k1", "alice", f64(1)),
		req(h0.Add(50*time.Minute), "claude", "k1", "bob", f64(2)),
		req(h0.Add(70*time.Minute), "llama", "k2", "", f64(4)),
		req(h0.Add(80*time.Minute), "gpt-4o", "k2", "", nil),
	}); err != nil {
		t.Fatal(err)
	}
	hour := int64(time.Hour / time.Millisecond)
	f := Filter{Since: h0.UnixMilli()}

	total, err := s.Series(ctx, f, hour, ByNone, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprint([]SeriesRow{
		{Start: h0.UnixMilli(), Requests: 2, CostUSD: 3},
		{Start: h0.Add(time.Hour).UnixMilli(), Requests: 2, CostUSD: 4},
	})
	if fmt.Sprint(total) != want {
		t.Errorf("total series = %v, want %v", total, want)
	}

	byModel, err := s.Series(ctx, f, hour, ByModel, []string{"gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, r := range byModel {
		g := r.Group
		if r.Other {
			g = "*"
		}
		got[fmt.Sprintf("%d/%s", (r.Start-h0.UnixMilli())/hour, g)] += r.CostUSD
	}
	if fmt.Sprint(got) != "map[0/*:2 0/gpt-4o:1 1/*:4 1/gpt-4o:0]" {
		t.Errorf("by model = %v", got)
	}

	all, err := s.Series(ctx, f, hour, ByUser, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("by user without keep = %+v", all)
	}
	if _, err := s.Series(ctx, f, hour, ByDay, nil); err == nil {
		t.Error("ByDay accepted as series group")
	}
	if _, err := s.Series(ctx, f, 0, ByNone, nil); err == nil {
		t.Error("zero bucket accepted")
	}
}

func ids(rs []*Request) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

func TestParseURL(t *testing.T) {
	for in, want := range map[string][2]string{
		"":                         {SQLite, "llmtee.db"},
		"sqlite:///data/llmtee.db": {SQLite, "/data/llmtee.db"},
		"sqlite://rel.db":          {SQLite, "rel.db"},
		"file:x.db":                {SQLite, "x.db"},
		"./x.db":                   {SQLite, "./x.db"},
		"postgres://u:p@h/db":      {Postgres, "postgres://u:p@h/db"},
		"postgresql://h/db":        {Postgres, "postgresql://h/db"},
	} {
		d, dsn, err := parseURL(in)
		if err != nil || d != want[0] || dsn != want[1] {
			t.Errorf("parseURL(%q) = %s %s %v", in, d, dsn, err)
		}
	}
	if _, _, err := parseURL("mysql://x"); err == nil {
		t.Error("mysql accepted")
	}
}

func TestMigrationsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	for i := 0; i < 2; i++ {
		s, err := Open(context.Background(), "sqlite://"+path)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
}
