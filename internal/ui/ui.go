// Package ui serves the embedded web UI (Go templates + htmx), the CSV
// export and the login flow. It only reads from the database (plus key
// management).
package ui

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/garritfra/llmtee/internal/budget"
	"github.com/garritfra/llmtee/internal/config"
	"github.com/garritfra/llmtee/internal/keys"
	"github.com/garritfra/llmtee/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Version is shown in the footer.
var Version = "dev"

// UI serves the dashboard.
type UI struct {
	cfg     *config.Config
	store   store.Store
	keys    *keys.Cache
	budgets *budget.Tracker
	pages   map[string]*template.Template
	secret  []byte
	// StoreErr is shown when the database could not be opened.
	StoreErr error
}

// New creates the UI. s may be nil if the database is unavailable.
func New(cfg *config.Config, s store.Store, kc *keys.Cache, b *budget.Tracker) (*UI, error) {
	u := &UI{cfg: cfg, store: s, keys: kc, budgets: b, pages: map[string]*template.Template{}}
	sum := sha256.Sum256([]byte("llmtee-ui-session:" + cfg.AdminPassword))
	u.secret = sum[:]
	funcs := template.FuncMap{
		"usd":  usd,
		"usdp": usdp,
		"num":  num,
		"nump": nump,
		"ms":   msfmt,
		"msp": func(p *int64) string {
			if p == nil {
				return ""
			}
			return msfmt(*p)
		},
		"ts":       func(r *store.Request) string { return r.Time().UTC().Format("2006-01-02 15:04:05") },
		"ago":      ago,
		"keyname":  u.keyName,
		"pct":      pct,
		"pretty":   pretty,
		"query":    func(v url.Values) template.URL { return template.URL(v.Encode()) },
		"with":     withParam,
		"version":  func() string { return Version },
		"readonly": func() bool { return cfg.ReadOnly },
		"uiauth":   func() bool { return cfg.UIAuth },
		"isErr":    func(r *store.Request) bool { return r.Status >= 400 || r.Error != "" },
		"usageUnknown": func(r *store.Request) bool {
			return r.TokensIn == nil && r.Status > 0 && r.Status < 400 && r.Error == ""
		},
		"deref": func(p *float64) float64 {
			if p == nil {
				return 0
			}
			return *p
		},
		"add": func(a, b int) int { return a + b },
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[kv[i].(string)] = kv[i+1]
			}
			return m
		},
	}
	layout, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html")
	if err != nil {
		return nil, err
	}
	for _, page := range []string{"overview", "requests", "request", "keys", "login", "error"} {
		t, err := template.Must(layout.Clone()).ParseFS(templateFS, "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", page, err)
		}
		u.pages[page] = t
	}
	return u, nil
}

// Register adds the UI routes to mux.
func (u *UI) Register(mux *http.ServeMux) {
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServer(http.FS(static)))))
	mux.HandleFunc("GET /login", u.loginPage)
	mux.HandleFunc("POST /login", u.login)
	mux.HandleFunc("POST /logout", u.logout)
	mux.Handle("GET /{$}", u.auth(u.overview))
	mux.Handle("GET /requests", u.auth(u.requests))
	mux.Handle("GET /requests/{id}", u.auth(u.request))
	mux.Handle("GET /keys", u.auth(u.keysPage))
	mux.Handle("POST /keys", u.auth(u.writable(u.createKey)))
	mux.Handle("POST /keys/{id}", u.auth(u.writable(u.updateKey)))
	mux.Handle("POST /keys/{id}/delete", u.auth(u.writable(u.deleteKey)))
	mux.Handle("GET /export.csv", u.auth(u.exportCSV))
}

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}

// ---- auth ----

const cookieName = "llmtee_session"

func (u *UI) sign(exp int64) string {
	m := hmac.New(sha256.New, u.secret)
	fmt.Fprint(m, exp)
	return strconv.FormatInt(exp, 10) + "." + hex.EncodeToString(m.Sum(nil))
}

// Authorized reports whether the request carries a valid session cookie or
// the admin password as bearer token.
func (u *UI) Authorized(r *http.Request) bool {
	if !u.cfg.UIAuth {
		return true
	}
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return subtle.ConstantTimeCompare([]byte(tok), []byte(u.cfg.AdminPassword)) == 1
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	expS, _, ok := strings.Cut(c.Value, ".")
	exp, err := strconv.ParseInt(expS, 10, 64)
	if !ok || err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(u.sign(exp)))
}

func (u *UI) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !u.Authorized(r) {
			if r.Header.Get("HX-Request") != "" {
				w.Header().Set("HX-Redirect", "/login")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost && !sameOrigin(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		h(w, r)
	})
}

func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" || o == "null" {
		return r.Header.Get("Sec-Fetch-Site") != "cross-site"
	}
	ou, err := url.Parse(o)
	return err == nil && ou.Host == r.Host
}

func (u *UI) writable(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if u.cfg.ReadOnly {
			http.Error(w, "read-only demo instance", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

func (u *UI) loginPage(w http.ResponseWriter, r *http.Request) {
	if !u.cfg.UIAuth {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	u.render(w, r, "login", map[string]any{"Next": r.URL.Query().Get("next")})
}

func (u *UI) login(w http.ResponseWriter, r *http.Request) {
	pw := r.FormValue("password")
	next := r.FormValue("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	if subtle.ConstantTimeCompare([]byte(pw), []byte(u.cfg.AdminPassword)) != 1 {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusUnauthorized)
		u.render(w, r, "login", map[string]any{"Next": next, "Error": "Wrong password."})
		return
	}
	exp := time.Now().Add(30 * 24 * time.Hour).Unix()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: u.sign(exp), Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		Expires: time.Unix(exp, 0)})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (u *UI) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- rendering ----

func (u *UI) render(w http.ResponseWriter, r *http.Request, page string, data map[string]any) {
	t := u.pages[page]
	if data == nil {
		data = map[string]any{}
	}
	data["Page"] = page
	data["Path"] = r.URL.Path
	data["LogBodies"] = u.cfg.LogBodies
	data["BodyDays"] = u.cfg.BodyRetentionDays
	if u.StoreErr != nil {
		data["StoreErr"] = u.StoreErr.Error()
	}
	name := "layout.html"
	if b, ok := data["Block"].(string); ok {
		name = b
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		slog.Error("render", "page", page, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

func (u *UI) fail(w http.ResponseWriter, r *http.Request, status int, err error) {
	slog.Warn("ui error", "path", r.URL.Path, "err", err)
	w.WriteHeader(status)
	u.render(w, r, "error", map[string]any{"Error": err.Error(), "Status": status})
}

func (u *UI) keyName(id string) string {
	if id == "" {
		return ""
	}
	if n := u.keys.Name(id); n != "" {
		return n
	}
	return id
}

// ---- overview ----

var ranges = []struct {
	Key, Label string
	Dur        time.Duration
}{
	{"24h", "24 hours", 24 * time.Hour},
	{"7d", "7 days", 7 * 24 * time.Hour},
	{"30d", "30 days", 30 * 24 * time.Hour},
	{"90d", "90 days", 90 * 24 * time.Hour},
}

func sinceFor(key string, now time.Time) (int64, string) {
	for _, rg := range ranges {
		if rg.Key == key {
			return now.Add(-rg.Dur).UnixMilli(), key
		}
	}
	if key == "all" {
		return 0, "all"
	}
	return now.Add(-7 * 24 * time.Hour).UnixMilli(), "7d"
}

type bar struct {
	Label  string
	Cost   float64
	Height float64
	Reqs   int64
}

func (u *UI) overview(w http.ResponseWriter, r *http.Request) {
	if u.store == nil {
		u.fail(w, r, http.StatusServiceUnavailable, errors.New("database unavailable: "+errString(u.StoreErr)))
		return
	}
	ctx := r.Context()
	now := time.Now()
	since, rng := sinceFor(r.URL.Query().Get("range"), now)
	f := store.Filter{Since: since}

	dayStart, monthStart := budget.Starts(now)
	today, err := u.store.Spend(ctx, store.Filter{Since: dayStart}, store.ByNone)
	if err != nil {
		u.fail(w, r, 500, err)
		return
	}
	month, err := u.store.Spend(ctx, store.Filter{Since: monthStart}, store.ByNone)
	if err != nil {
		u.fail(w, r, 500, err)
		return
	}
	total, err := u.store.Spend(ctx, f, store.ByNone)
	if err != nil {
		u.fail(w, r, 500, err)
		return
	}
	days, err := u.store.Spend(ctx, f, store.ByDay)
	if err != nil {
		u.fail(w, r, 500, err)
		return
	}
	group := func(g string) []store.SpendRow {
		rows, err := u.store.Spend(ctx, store.Filter{Since: since, Limit: 10}, g)
		if err != nil {
			slog.Warn("spend query", "group", g, "err", err)
		}
		return rows
	}

	// Fill missing days so the chart has a continuous axis.
	var bars []bar
	byDay := map[string]store.SpendRow{}
	for _, d := range days {
		byDay[d.Group] = d
	}
	start := time.UnixMilli(since).UTC()
	if since == 0 && len(days) > 0 {
		start, _ = time.Parse("2006-01-02", days[0].Group)
	}
	maxCost := 0.0
	for d := start.Truncate(24 * time.Hour); !d.After(now.UTC()) && len(bars) < 400; d = d.Add(24 * time.Hour) {
		key := d.Format("2006-01-02")
		row := byDay[key]
		bars = append(bars, bar{Label: key, Cost: row.CostUSD, Reqs: row.Requests})
		maxCost = math.Max(maxCost, row.CostUSD)
	}
	for i := range bars {
		if maxCost > 0 {
			bars[i].Height = math.Max(bars[i].Cost/maxCost*100, 0.5)
		}
	}

	type budgetRow struct {
		Key      *store.Key
		Spend    store.KeySpend
		DayPct   float64
		MonthPct float64
	}
	var budgets []budgetRow
	ks, _ := u.store.ListKeys(ctx)
	for _, k := range ks {
		if k.BudgetDayUSD == nil && k.BudgetMonthUSD == nil {
			continue
		}
		s := u.budgets.Spend(k.ID)
		br := budgetRow{Key: k, Spend: s}
		if k.BudgetDayUSD != nil && *k.BudgetDayUSD > 0 {
			br.DayPct = math.Min(s.Day / *k.BudgetDayUSD * 100, 100)
		}
		if k.BudgetMonthUSD != nil && *k.BudgetMonthUSD > 0 {
			br.MonthPct = math.Min(s.Month / *k.BudgetMonthUSD * 100, 100)
		}
		budgets = append(budgets, br)
	}

	u.render(w, r, "overview", map[string]any{
		"Range": rng, "Ranges": ranges, "Since": since,
		"Today": first(today), "Month": first(month), "Total": first(total),
		"Bars": bars, "MaxCost": maxCost,
		"ByModel": group(store.ByModel), "ByKey": group(store.ByKey), "ByUser": group(store.ByUser),
		"ByUpstream": group(store.ByUpstream), "Budgets": budgets,
	})
}

func first(rows []store.SpendRow) store.SpendRow {
	if len(rows) == 0 {
		return store.SpendRow{}
	}
	return rows[0]
}

func errString(err error) string {
	if err == nil {
		return "not configured"
	}
	return err.Error()
}

// ---- requests ----

// FilterFromQuery parses list filters from URL parameters.
func FilterFromQuery(q url.Values, now time.Time) (store.Filter, string) {
	since, rng := sinceFor(q.Get("range"), now)
	if q.Get("range") == "" && q.Get("session") != "" {
		since, rng = 0, "all"
	}
	f := store.Filter{
		Since: since, Upstream: q.Get("upstream"), Model: q.Get("model"), KeyID: q.Get("key"),
		UserID: q.Get("user"), SessionID: q.Get("session"), Status: q.Get("status"), Before: q.Get("before"),
	}
	if p := q.Get("prop"); p != "" {
		f.PropName, f.PropValue, _ = strings.Cut(p, "=")
		f.PropName = strings.ToLower(strings.TrimSpace(f.PropName))
		f.PropValue = strings.TrimSpace(f.PropValue)
	}
	return f, rng
}

const pageSize = 100

func (u *UI) requests(w http.ResponseWriter, r *http.Request) {
	if u.store == nil {
		u.fail(w, r, http.StatusServiceUnavailable, errors.New("database unavailable: "+errString(u.StoreErr)))
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	f, rng := FilterFromQuery(q, time.Now())
	f.Limit = pageSize + 1
	reqs, err := u.store.ListRequests(ctx, f)
	if err != nil {
		u.fail(w, r, 500, err)
		return
	}
	more := len(reqs) > pageSize
	if more {
		reqs = reqs[:pageSize]
	}
	next := url.Values{}
	for k, v := range q {
		next[k] = v
	}
	if more {
		next.Set("before", reqs[len(reqs)-1].ID)
	}
	base := url.Values{}
	for k, v := range q {
		if k != "before" {
			base[k] = v
		}
	}
	data := map[string]any{"Requests": reqs, "More": more, "Next": next, "Query": base, "Range": rng, "Ranges": ranges, "Q": q}

	sum, err := u.store.Spend(ctx, store.Filter{Since: f.Since, Upstream: f.Upstream, Model: f.Model, KeyID: f.KeyID,
		UserID: f.UserID, SessionID: f.SessionID, Status: f.Status, PropName: f.PropName, PropValue: f.PropValue}, store.ByNone)
	if err == nil {
		data["Sum"] = first(sum)
	}
	// htmx: "load more" returns only rows, a filter change only the results.
	if r.Header.Get("HX-Request") != "" {
		if q.Get("before") != "" {
			data["Block"] = "rows"
		} else if r.Header.Get("HX-Target") == "results" {
			data["Block"] = "results"
		}
	}
	if data["Block"] == nil {
		data["Models"], _ = u.store.Distinct(ctx, "model", f.Since)
		data["Upstreams"], _ = u.store.Distinct(ctx, "upstream", f.Since)
		keyIDs, _ := u.store.Distinct(ctx, "key_id", f.Since)
		type opt struct{ ID, Name string }
		var ko []opt
		for _, id := range keyIDs {
			ko = append(ko, opt{id, u.keyName(id)})
		}
		sort.Slice(ko, func(i, j int) bool { return ko[i].Name < ko[j].Name })
		data["Keys"] = ko
	}
	u.render(w, r, "requests", data)
}

func (u *UI) request(w http.ResponseWriter, r *http.Request) {
	if u.store == nil {
		u.fail(w, r, http.StatusServiceUnavailable, errors.New("database unavailable"))
		return
	}
	req, err := u.store.GetRequest(r.Context(), r.PathValue("id"), true)
	if errors.Is(err, store.ErrNotFound) {
		u.fail(w, r, http.StatusNotFound, errors.New("request not found"))
		return
	}
	if err != nil {
		u.fail(w, r, 500, err)
		return
	}
	var props []kv
	for k, v := range req.Properties {
		props = append(props, kv{k, v})
	}
	sort.Slice(props, func(i, j int) bool { return props[i].K < props[j].K })
	u.render(w, r, "request", map[string]any{"R": req, "Props": props,
		"ReqBody": pretty(req.ReqBody), "RespBody": pretty(req.RespBody)})
}

type kv struct{ K, V string }

// ---- keys ----

type keyView struct {
	*store.Key
	Spend store.KeySpend
}

func (u *UI) keysPage(w http.ResponseWriter, r *http.Request) {
	u.renderKeys(w, r, map[string]any{})
}

func (u *UI) renderKeys(w http.ResponseWriter, r *http.Request, data map[string]any) {
	if u.store == nil {
		u.fail(w, r, http.StatusServiceUnavailable, errors.New("database unavailable"))
		return
	}
	ks, err := u.store.ListKeys(r.Context())
	if err != nil {
		u.fail(w, r, 500, err)
		return
	}
	var views []keyView
	for _, k := range ks {
		views = append(views, keyView{k, u.budgets.Spend(k.ID)})
	}
	data["Keys"] = views
	data["Upstreams"] = u.cfg.Upstreams
	u.render(w, r, "keys", data)
}

func parseBudget(s string) (*float64, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "$"))
	if s == "" {
		return nil, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return nil, fmt.Errorf("invalid budget %q", s)
	}
	return &f, nil
}

func (u *UI) createKey(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	day, err1 := parseBudget(r.FormValue("budget_day"))
	month, err2 := parseBudget(r.FormValue("budget_month"))
	if err := errors.Join(err1, err2); err != nil {
		u.renderKeys(w, r, map[string]any{"Error": err.Error()})
		return
	}
	if name == "" {
		u.renderKeys(w, r, map[string]any{"Error": "name is required"})
		return
	}
	var k *store.Key
	var secret string
	if r.FormValue("kind") == keys.Passthrough {
		pk := strings.TrimSpace(r.FormValue("provider_key"))
		if pk == "" {
			u.renderKeys(w, r, map[string]any{"Error": "paste the provider key to label (only its hash is stored)"})
			return
		}
		k = keys.New(keys.Passthrough, name, pk, day, month)
	} else {
		secret = keys.NewSecret()
		k = keys.New(keys.Managed, name, secret, day, month)
	}
	if err := u.store.CreateKey(r.Context(), k); err != nil {
		u.renderKeys(w, r, map[string]any{"Error": "create key: " + err.Error()})
		return
	}
	u.reloadKeys(r.Context())
	u.renderKeys(w, r, map[string]any{"Created": k, "Secret": secret})
}

func (u *UI) updateKey(w http.ResponseWriter, r *http.Request) {
	k, ok := u.keys.ByID(r.PathValue("id"))
	if !ok {
		u.fail(w, r, 404, errors.New("key not found"))
		return
	}
	day, err1 := parseBudget(r.FormValue("budget_day"))
	month, err2 := parseBudget(r.FormValue("budget_month"))
	if err := errors.Join(err1, err2); err != nil {
		u.renderKeys(w, r, map[string]any{"Error": err.Error()})
		return
	}
	nk := *k
	if n := strings.TrimSpace(r.FormValue("name")); n != "" {
		nk.Name = n
	}
	nk.BudgetDayUSD, nk.BudgetMonthUSD = day, month
	if err := u.store.UpdateKey(r.Context(), &nk); err != nil {
		u.renderKeys(w, r, map[string]any{"Error": err.Error()})
		return
	}
	u.reloadKeys(r.Context())
	http.Redirect(w, r, "/keys", http.StatusSeeOther)
}

func (u *UI) deleteKey(w http.ResponseWriter, r *http.Request) {
	if err := u.store.DeleteKey(r.Context(), r.PathValue("id")); err != nil {
		u.renderKeys(w, r, map[string]any{"Error": err.Error()})
		return
	}
	u.reloadKeys(r.Context())
	http.Redirect(w, r, "/keys", http.StatusSeeOther)
}

func (u *UI) reloadKeys(ctx context.Context) {
	if err := u.keys.Reload(ctx, u.store); err != nil {
		slog.Error("reload keys", "err", err)
	}
}

// ---- CSV export ----

// CSVHeader is the column list of the CSV export.
var CSVHeader = []string{"id", "time", "upstream", "path", "model", "key_id", "key_name", "user_id", "session_id",
	"status", "streamed", "tokens_in", "tokens_out", "tokens_cache_read", "tokens_cache_write", "cost_usd",
	"latency_ms", "ttft_ms", "error", "properties"}

func (u *UI) exportCSV(w http.ResponseWriter, r *http.Request) {
	if u.store == nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	f, _ := FilterFromQuery(r.URL.Query(), time.Now())
	f.Before = ""
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="llmtee-%s.csv"`, time.Now().UTC().Format("20060102-150405")))
	if err := WriteCSV(r.Context(), w, u.store, f, u.keys.Name); err != nil {
		slog.Error("csv export", "err", err)
	}
}

// WriteCSV streams all requests matching f as CSV.
func WriteCSV(ctx context.Context, w interface{ Write([]byte) (int, error) }, s store.Store, f store.Filter, keyName func(string) string) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(CSVHeader); err != nil {
		return err
	}
	i := func(p *int64) string {
		if p == nil {
			return ""
		}
		return strconv.FormatInt(*p, 10)
	}
	f.Limit = 1000
	for n := 0; n < 1_000_000; {
		reqs, err := s.ListRequests(ctx, f)
		if err != nil {
			return err
		}
		for _, r := range reqs {
			cost := ""
			if r.CostUSD != nil {
				cost = strconv.FormatFloat(*r.CostUSD, 'f', -1, 64)
			}
			var props []string
			for k, v := range r.Properties {
				props = append(props, k+"="+v)
			}
			sort.Strings(props)
			if err := cw.Write([]string{r.ID, r.Time().UTC().Format(time.RFC3339Nano), r.Upstream, r.Path, r.Model,
				r.KeyID, keyName(r.KeyID), r.UserID, r.SessionID, strconv.Itoa(r.Status), strconv.FormatBool(r.Streamed),
				i(r.TokensIn), i(r.TokensOut), i(r.TokensCacheRead), i(r.TokensCacheWrite), cost,
				strconv.FormatInt(r.LatencyMS, 10), i(r.TTFTMS), r.Error, strings.Join(props, ";")}); err != nil {
				return err
			}
		}
		n += len(reqs)
		if len(reqs) < f.Limit {
			break
		}
		f.Before = reqs[len(reqs)-1].ID
	}
	cw.Flush()
	return cw.Error()
}

// ---- formatting helpers ----

func usd(v float64) string {
	switch {
	case v == 0:
		return "$0.00"
	case math.Abs(v) < 0.01:
		return "$" + strconv.FormatFloat(v, 'f', 4, 64)
	default:
		return "$" + commas(strconv.FormatFloat(v, 'f', 2, 64))
	}
}

func usdp(p *float64) string {
	if p == nil {
		return "—"
	}
	if *p != 0 && math.Abs(*p) < 0.0001 {
		return "$" + strconv.FormatFloat(*p, 'f', 6, 64)
	}
	if math.Abs(*p) < 1 {
		return "$" + strconv.FormatFloat(*p, 'f', 4, 64)
	}
	return usd(*p)
}

func num(v int64) string { return commas(strconv.FormatInt(v, 10)) }

func nump(p *int64) string {
	if p == nil {
		return "—"
	}
	return num(*p)
}

func commas(s string) string {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	intPart, frac, hasFrac := strings.Cut(s, ".")
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	out := b.String()
	if hasFrac {
		out += "." + frac
	}
	if neg {
		out = "-" + out
	}
	return out
}

func msfmt(v int64) string {
	if v >= 10000 {
		return strconv.FormatFloat(float64(v)/1000, 'f', 1, 64) + " s"
	}
	return num(v) + " ms"
}

func ago(r *store.Request) string {
	d := time.Since(r.Time())
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s ago"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	}
	return r.Time().UTC().Format("Jan 2")
}

func pct(part, whole float64) string {
	if whole <= 0 {
		return "0"
	}
	return strconv.FormatFloat(math.Min(part/whole*100, 100), 'f', 1, 64)
}

// pretty indents JSON bodies; SSE and other text is returned unchanged.
func pretty(b []byte) string {
	t := bytes.TrimSpace(b)
	if len(t) > 0 && (t[0] == '{' || t[0] == '[') {
		var out bytes.Buffer
		if json.Indent(&out, t, "", "  ") == nil {
			return out.String()
		}
	}
	return string(b)
}

func withParam(v url.Values, key, value string) template.URL {
	n := url.Values{}
	for k, vs := range v {
		if k != "before" {
			n[k] = vs
		}
	}
	if value == "" {
		n.Del(key)
	} else {
		n.Set(key, value)
	}
	return template.URL(n.Encode())
}
