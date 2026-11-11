package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/garritfra/llmtee/internal/budget"
	"github.com/garritfra/llmtee/internal/config"
	"github.com/garritfra/llmtee/internal/keys"
	"github.com/garritfra/llmtee/internal/store"
	"github.com/oklog/ulid/v2"
)

func TestSinceForAlignsToBuckets(t *testing.T) {
	now := time.Date(2026, 11, 11, 14, 37, 12, 0, time.UTC)
	for _, tc := range []struct {
		key, rng string
		want     time.Time
		unit     time.Duration
	}{
		{"24h", "24h", time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC), hour},
		{"7d", "7d", time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC), day},
		{"30d", "30d", time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC), day},
		{"", "7d", time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC), day},
		{"bogus", "7d", time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC), day},
	} {
		since, rng, unit := sinceFor(tc.key, now)
		if got := time.UnixMilli(since).UTC(); !got.Equal(tc.want) || rng != tc.rng || unit != tc.unit {
			t.Errorf("sinceFor(%q) = %s %s %s, want %s %s %s", tc.key, got, rng, unit, tc.want, tc.rng, tc.unit)
		}
	}
	if since, rng, _ := sinceFor("all", now); since != 0 || rng != "all" {
		t.Errorf("all = %d %s", since, rng)
	}
}

func TestBuildChart24h(t *testing.T) {
	now := time.Date(2026, 11, 11, 14, 37, 0, 0, time.UTC)
	since, _, unit := sinceFor("24h", now)
	start := time.UnixMilli(since)
	ms := func(t time.Time) int64 { return t.UnixMilli() }
	rows := []store.SeriesRow{
		{Start: ms(start), Group: "gpt", CostUSD: 1, Requests: 2},
		{Start: ms(start), Other: true, CostUSD: 3, Requests: 1},
		{Start: ms(start.Add(23 * hour)), Group: "claude", CostUSD: 2, Requests: 4},
	}
	c := buildChart(rows, []string{"claude", "gpt"}, start, now, unit, func(g string) string { return g },
		func(g string) string { return "/requests?model=" + g })

	if len(c.Cols) != 24 || c.Unit != "hour" {
		t.Fatalf("got %d cols of %s, want 24 hourly", len(c.Cols), c.Unit)
	}
	first, last := c.Cols[0], c.Cols[23]
	if first.Cost != 4 || first.Reqs != 3 || last.Cost != 2 || !last.Partial || first.Partial {
		t.Errorf("first = %+v, last = %+v", first, last)
	}
	if first.Label != "Tue Nov 10, 15:00–16:00 UTC" {
		t.Errorf("label = %q", first.Label)
	}
	// Stack order: kept groups by rank, then Other; shares add up to 100.
	var names []string
	for _, s := range first.Segs {
		names = append(names, fmt.Sprintf("%s:%.0f", s.Name, s.Pct))
	}
	if fmt.Sprint(names) != "[gpt:25 Other:75]" {
		t.Errorf("segments = %v", names)
	}
	var legend []string
	for _, s := range c.Series {
		legend = append(legend, fmt.Sprintf("%s/%s/%.0f/%q", s.Name, s.Color, s.Share, s.URL))
	}
	want := `[claude/s0/33/"/requests?model=claude" gpt/s1/17/"/requests?model=gpt" Other/s-other/50/""]`
	if fmt.Sprint(legend) != want {
		t.Errorf("legend = %v, want %v", legend, want)
	}
	if fmt.Sprint(first.Values) != "[0 1 3]" {
		t.Errorf("values = %v", first.Values)
	}
	// Ticks every 6 hours, the date at midnight.
	var ticks []string
	for _, col := range c.Cols {
		if col.Tick != "" {
			ticks = append(ticks, col.Tick)
		}
	}
	if fmt.Sprint(ticks) != "[18:00 Nov 11 06:00 12:00]" {
		t.Errorf("ticks = %v", ticks)
	}
	// The y axis ends on a round value above the highest column.
	top := c.YTicks[len(c.YTicks)-1]
	if top.Label != "$4" || top.Pos != 100 || first.Height != 100 || last.Height != 50 {
		t.Errorf("y top = %+v, heights %v %v", top, first.Height, last.Height)
	}
}

func TestBuildChartWeeks(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC) // a Wednesday
	start := now.AddDate(-1, 0, 0)
	rows := []store.SeriesRow{
		{Start: start.Truncate(day).UnixMilli(), CostUSD: 1, Requests: 1},
		{Start: now.Truncate(day).UnixMilli(), CostUSD: 2, Requests: 1},
		{Start: now.Truncate(day).Add(-day).UnixMilli(), CostUSD: 3, Requests: 1}, // same week
	}
	c := buildChart(rows, nil, start, now, week, func(g string) string { return g }, func(string) string { return "" })
	if c.Unit != "week" || len(c.Cols) != 53 {
		t.Fatalf("unit %s, %d cols", c.Unit, len(c.Cols))
	}
	if last := c.Cols[len(c.Cols)-1]; last.Cost != 5 || !strings.HasPrefix(last.Label, "Week of Mon Nov 9, 2026") {
		t.Errorf("last week = %+v", last)
	}
	if c.Split || len(c.Series) != 1 {
		t.Errorf("unsplit chart has series %+v", c.Series)
	}
	shown := 0
	for _, col := range c.Cols {
		if col.Tick != "" {
			shown++
		}
	}
	if shown == 0 || shown > maxTicks {
		t.Errorf("%d ticks", shown)
	}
}

func TestBuildChartKeepsNewestColumns(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	c := buildChart(nil, nil, now.AddDate(-3, 0, 0), now, day, func(g string) string { return g }, func(string) string { return "" })
	if len(c.Cols) != maxCols || !c.Cols[len(c.Cols)-1].Partial ||
		c.Cols[len(c.Cols)-1].Label != "Wed Nov 11, 2026" {
		t.Errorf("%d cols, last %+v", len(c.Cols), c.Cols[len(c.Cols)-1])
	}
}

func TestChartKeep(t *testing.T) {
	rows := func(n int) []store.SpendRow {
		out := make([]store.SpendRow, n)
		for i := range out {
			out[i].Group = fmt.Sprint(i)
		}
		return out
	}
	for n, want := range map[int]int{0: 0, 3: 3, 6: 6, 7: 5, 10: 5} {
		if got := len(chartKeep(rows(n))); got != want {
			t.Errorf("chartKeep(%d rows) kept %d, want %d", n, got, want)
		}
	}
}

func TestYScale(t *testing.T) {
	for max, want := range map[float64]string{
		0:      "[$0]",
		0.0042: "[$0 $0.002 $0.004 $0.006]",
		0.48:   "[$0 $0.2 $0.4 $0.6]",
		4.07:   "[$0 $2 $4 $6]",
		1234:   "[$0 $500 $1,000 $1,500]",
	} {
		_, ticks := yScale(max)
		var labels []string
		for _, t := range ticks {
			labels = append(labels, t.Label)
		}
		if fmt.Sprint(labels) != want {
			t.Errorf("yScale(%v) = %v, want %v", max, labels, want)
		}
	}
}

func TestCompare(t *testing.T) {
	at := time.Unix(0, 0)
	for _, tc := range []struct {
		cur, prev float64
		want      string
	}{
		{0, 0, "same as prior period"},
		{1, 0, "▲ no spend in prior period"},
		{1.5, 1, "▲ 50% more than prior period"},
		{0.75, 1, "▼ 25% less than prior period"},
		{1.001, 1, "same as prior period"},
		{3.2, 1, "▲ 3.2× prior period"},
		{500, 1, "▲ over 100× prior period"},
	} {
		d := compare(tc.cur, tc.prev, at)
		if got := strings.TrimSpace(d.Arrow + " " + d.Text); got != tc.want {
			t.Errorf("compare(%v, %v) = %q, want %q", tc.cur, tc.prev, got, tc.want)
		}
	}
}

func TestBreakdownOther(t *testing.T) {
	total := store.SpendRow{Requests: 10, CostUSD: 10}
	rows := []store.SpendRow{{Group: "a", Requests: 6, CostUSD: 8}, {Group: "", Requests: 2, CostUSD: 1}}
	name := func(g string) string { return g }
	link := func(g string) string { return "/r?" + g }

	b := newBreakdown("By x", "x", rows, 2, total, name, link, map[string]string{"a": "s0"})
	if b.Other == nil || b.Other.Requests != 2 || b.Other.Cost != 1 || b.Other.Share != 10 {
		t.Errorf("other = %+v", b.Other)
	}
	if b.Rows[0].Color != "s0" || b.Rows[0].Share != 80 || b.Rows[1].URL != "" || !b.Rows[1].Unset {
		t.Errorf("rows = %+v", b.Rows)
	}
	if b := newBreakdown("By x", "x", rows, 10, total, name, link, nil); b.Other != nil {
		t.Error("other row although all groups are listed")
	}
}

func newTestUI(t *testing.T) (store.Store, *http.ServeMux) {
	t.Helper()
	s, err := store.Open(context.Background(), "sqlite://"+filepath.Join(t.TempDir(), "ui.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	cfg := config.Defaults()
	cfg.UIAuth = false
	u, err := New(cfg, s, keys.NewCache(), budget.New())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	u.Register(mux)
	return s, mux
}

func get(mux *http.ServeMux, url string) (int, string) {
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
	return w.Code, w.Body.String()
}

func TestVersionedAssets(t *testing.T) {
	_, mux := newTestUI(t)
	_, body := get(mux, "/")
	css := assetURL("style.css")
	if len(assetVersions["style.css"]) != 12 || !strings.Contains(body, `href="`+css+`"`) {
		t.Fatalf("page does not link %s", css)
	}
	for url, want := range map[string]string{
		css:                         "public, max-age=31536000, immutable",
		"/static/style.css?v=stale": "no-cache",
		"/static/style.css":         "no-cache",
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
		if got := w.Header().Get("Cache-Control"); w.Code != 200 || got != want {
			t.Errorf("%s: %d %q, want %q", url, w.Code, got, want)
		}
	}
}

func TestOverviewEmptyStates(t *testing.T) {
	s, mux := newTestUI(t)
	if code, body := get(mux, "/"); code != 200 || !strings.Contains(body, "No requests yet") {
		t.Fatalf("first run: %d\n%s", code, body)
	}
	old := time.Now().AddDate(0, 0, -60)
	cost := 1.0
	if err := s.Insert(context.Background(), []*store.Request{{ID: ulid.MustNew(ulid.Timestamp(old), ulid.DefaultEntropy()).String(),
		TS: old.UnixMilli(), Upstream: "openai", Model: "gpt-4o", Status: 200, CostUSD: &cost}}); err != nil {
		t.Fatal(err)
	}
	if code, body := get(mux, "/?range=7d&split=model"); code != 200 || !strings.Contains(body, "No requests in the last 7 days") ||
		!strings.Contains(body, `href="/?range=all&amp;split=model"`) {
		t.Fatalf("empty range: %d\n%s", code, body)
	}
}

func TestOverviewRenders(t *testing.T) {
	ctx := context.Background()
	s, mux := newTestUI(t)
	now := time.Now()
	var reqs []*store.Request
	for i, m := range []string{"gpt-4o", "claude", "llama", "gpt-4o", "mistral", "qwen", "phi", "gemma"} {
		ts := now.Add(-time.Duration(i) * 7 * time.Hour)
		cost := float64(i + 1)
		reqs = append(reqs, &store.Request{ID: ulid.MustNew(ulid.Timestamp(ts), ulid.DefaultEntropy()).String(),
			TS: ts.UnixMilli(), Upstream: "openai", Model: m, KeyID: "k1", UserID: fmt.Sprint("u", i%3), Status: 200, CostUSD: &cost})
	}
	if err := s.Insert(ctx, reqs); err != nil {
		t.Fatal(err)
	}
	for _, rng := range []string{"24h", "7d", "30d", "90d", "all"} {
		for _, sp := range splits {
			url := overviewURL(rng, sp.Key)
			code, body := get(mux, url)
			if code != 200 || !strings.Contains(body, `class="cols"`) {
				t.Fatalf("%s: status %d\n%s", url, code, body)
			}
			if sp.Key == store.ByModel && rng != "24h" && !strings.Contains(body, "s-other") {
				t.Errorf("%s: 8 models but no Other series", url)
			}
		}
	}
}
