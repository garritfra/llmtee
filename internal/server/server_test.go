package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/garritfra/llmtee/internal/config"
	"github.com/garritfra/llmtee/internal/fakeupstream"
	"github.com/garritfra/llmtee/internal/store"
)

func setup(t *testing.T, dbURL string) (*Server, *httptest.Server) {
	t.Helper()
	fake := httptest.NewServer(&fakeupstream.Server{})
	t.Cleanup(fake.Close)
	cfg := config.Defaults()
	cfg.DatabaseURL = dbURL
	cfg.AdminPassword = "pw"
	cfg.FlushInterval = 20 * time.Millisecond
	cfg.Upstreams = []config.Upstream{
		{Name: "openai", Type: config.TypeOpenAI, BaseURL: fake.URL, APIKey: "sk-real"},
		{Name: "anthropic", Type: config.TypeAnthropic, BaseURL: fake.URL},
		{Name: "openrouter", Type: config.TypeOpenAI, BaseURL: fake.URL},
	}
	s, err := New(context.Background(), cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler)
	t.Cleanup(func() { hs.Close(); s.Close() })
	return s, hs
}

func post(t *testing.T, c *http.Client, u, body string, hdr ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func get(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func waitRows(t *testing.T, s *Server, n int) []*store.Request {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		rs, err := s.Store.ListRequests(context.Background(), store.Filter{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if len(rs) >= n || time.Now().After(deadline) {
			return rs
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEndToEnd(t *testing.T) {
	s, hs := setup(t, "sqlite://"+filepath.Join(t.TempDir(), "e2e.db"))
	c := hs.Client()

	code, _ := post(t, c, hs.URL+"/openai/v1/chat/completions", `{"model":"gpt-4o-mini","stream":true,"messages":[]}`,
		"Authorization", "Bearer sk-user", "Helicone-Property-Feature", "e2e", "Helicone-User-Id", "garrit")
	if code != 200 {
		t.Fatalf("proxy status %d", code)
	}
	post(t, c, hs.URL+"/anthropic/v1/messages", `{"model":"claude-sonnet-4-5","messages":[]}`, "X-Api-Key", "sk-ant")
	rows := waitRows(t, s, 2)
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	oa := rows[1]
	if oa.CostUSD == nil || *oa.TokensIn != 219 || oa.UserID != "garrit" || oa.Properties["feature"] != "e2e" {
		t.Fatalf("row = %+v", oa)
	}
	full, err := s.Store.GetRequest(context.Background(), oa.ID, true)
	if err != nil || !strings.Contains(string(full.RespBody), "[DONE]") || !strings.Contains(string(full.ReqBody), "include_usage") {
		t.Fatalf("bodies: %v %q", err, full.ReqBody)
	}

	// UI requires login.
	jar, _ := cookiejar.New(nil)
	ui := &http.Client{Jar: jar}
	if code, body := get(t, ui, hs.URL+"/requests"); code != 200 || !strings.Contains(body, "Sign in") {
		t.Fatalf("expected login page, got %d", code)
	}
	resp, err := ui.PostForm(hs.URL+"/login", url.Values{"password": {"wrong"}})
	if err != nil || resp.StatusCode != 401 {
		t.Fatalf("wrong password: %v %v", err, resp.StatusCode)
	}
	resp, _ = ui.PostForm(hs.URL+"/login", url.Values{"password": {"pw"}, "next": {"/requests"}})
	resp.Body.Close()
	for _, p := range []string{"/", "/?range=all", "/requests", "/requests?model=gpt-4o-mini-2024-07-18&status=ok", "/requests/" + oa.ID, "/keys"} {
		code, body := get(t, ui, hs.URL+p)
		if code != 200 || strings.Contains(body, "template error") {
			t.Errorf("GET %s = %d", p, code)
		}
	}
	_, body := get(t, ui, hs.URL+"/requests/"+oa.ID)
	if !strings.Contains(body, "include_usage") || !strings.Contains(body, "feature") {
		t.Error("detail page misses body or properties")
	}
	_, csv := get(t, ui, hs.URL+"/export.csv?range=all")
	if lines := strings.Count(csv, "\n"); lines != 3 || !strings.HasPrefix(csv, "id,time,upstream") {
		t.Errorf("csv:\n%s", csv)
	}

	// Create a managed key with a tiny budget via the UI and use it.
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/keys", strings.NewReader(url.Values{"name": {"agent"}, "budget_day": {"0.0001"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", hs.URL)
	resp, err = ui.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	secret := regexp.MustCompile(`llmtee-[0-9A-Za-z]{32}`).FindString(string(b))
	if secret == "" {
		t.Fatalf("no secret in response")
	}
	// Cross-origin POST is rejected.
	req, _ = http.NewRequest(http.MethodPost, hs.URL+"/keys", strings.NewReader("name=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	if resp, _ := ui.Do(req); resp.StatusCode != 403 {
		t.Errorf("cross-origin POST = %d", resp.StatusCode)
	}

	code, _ = post(t, c, hs.URL+"/openai/v1/chat/completions", `{"model":"gpt-4o-mini","stream":true}`, "Authorization", "Bearer "+secret)
	if code != 200 {
		t.Fatalf("managed key request = %d", code)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		code, body = post(t, c, hs.URL+"/openai/v1/chat/completions", `{"model":"gpt-4o-mini"}`, "Authorization", "Bearer "+secret)
		if code == 429 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code != 429 || !strings.Contains(body, "budget") {
		t.Fatalf("budget not enforced: %d %s", code, body)
	}

	// MCP.
	mcpCall := func(body string) map[string]any {
		t.Helper()
		code, out := post(t, c, hs.URL+"/mcp", body, "Authorization", "Bearer pw")
		if code != 200 {
			t.Fatalf("mcp %d %s", code, out)
		}
		var m map[string]any
		json.Unmarshal([]byte(out), &m)
		return m
	}
	init := mcpCall(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if init["result"].(map[string]any)["protocolVersion"] != "2025-03-26" {
		t.Errorf("init = %v", init)
	}
	if code, _ := post(t, c, hs.URL+"/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, "Authorization", "Bearer pw"); code != 202 {
		t.Errorf("notification = %d", code)
	}
	list := mcpCall(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if n := len(list["result"].(map[string]any)["tools"].([]any)); n != 3 {
		t.Errorf("tools = %d", n)
	}
	spend := mcpCall(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_spend","arguments":{"range":"today","group_by":"model"}}}`)
	sc := spend["result"].(map[string]any)["structuredContent"].(map[string]any)
	if len(sc["rows"].([]any)) == 0 || len(sc["budgets"].([]any)) != 1 {
		t.Errorf("get_spend = %v", sc)
	}
	lr := mcpCall(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"list_requests","arguments":{"limit":2}}}`)
	reqs := lr["result"].(map[string]any)["structuredContent"].(map[string]any)["requests"].([]any)
	if len(reqs) != 2 {
		t.Errorf("list_requests = %v", lr)
	}
	gr := mcpCall(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"get_request","arguments":{"id":"` + oa.ID + `","include_bodies":true}}}`)
	if !strings.Contains(gr["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string), "include_usage") {
		t.Errorf("get_request = %v", gr)
	}
	bad := mcpCall(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"get_request","arguments":{"id":"nope"}}}`)
	if bad["result"].(map[string]any)["isError"] != true {
		t.Errorf("missing request should be a tool error: %v", bad)
	}
	if code, _ := post(t, c, hs.URL+"/mcp", `{}`); code != 401 {
		t.Errorf("mcp without auth = %d", code)
	}

	// Health is public but does not reveal internal state. Metrics need admin auth.
	if code, body := get(t, c, hs.URL+"/healthz"); code != 200 || strings.TrimSpace(body) != `{"status":"ok"}` {
		t.Errorf("healthz %d %s", code, body)
	}
	if code, _ := get(t, c, hs.URL+"/metrics"); code != http.StatusUnauthorized {
		t.Errorf("metrics without auth = %d", code)
	}
	if code, body := get(t, ui, hs.URL+"/metrics"); code != 200 || !strings.Contains(body, "llmtee_cost_usd_total") || !strings.Contains(body, "llmtee_budget_rejections_total") {
		t.Errorf("metrics with auth = %d", code)
	}
}

// The proxy must keep working when the database cannot be opened at all.
func TestProxyWithoutDatabase(t *testing.T) {
	_, hs := setup(t, "sqlite:///proc/llmtee-cannot-exist/x.db")
	code, body := post(t, hs.Client(), hs.URL+"/openai/v1/chat/completions", `{"model":"gpt-4o-mini","stream":true}`, "Authorization", "Bearer sk")
	if code != 200 || !strings.Contains(body, "[DONE]") {
		t.Fatalf("proxy failed without DB: %d", code)
	}
	if code, body := get(t, hs.Client(), hs.URL+"/healthz"); code != 200 || strings.TrimSpace(body) != `{"status":"ok"}` {
		t.Errorf("healthz %d %s", code, body)
	}
}

func TestSecurityHeaders(t *testing.T) {
	_, hs := setup(t, "sqlite://"+filepath.Join(t.TempDir(), "headers.db"))
	resp, err := hs.Client().Get(hs.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for name, want := range map[string]string{
		"Strict-Transport-Security": "max-age=31536000",
		"X-Frame-Options":           "DENY",
		"Content-Security-Policy":   "frame-ancestors 'none'",
		"X-Content-Type-Options":    "nosniff",
		"Cache-Control":             "no-store",
	} {
		if got := resp.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	resp, err = hs.Client().Get(hs.URL + "/static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("static Cache-Control = %q", got)
	}
}

func TestUIAuthDisabled(t *testing.T) {
	cfg := config.Defaults()
	cfg.DatabaseURL = "sqlite://" + filepath.Join(t.TempDir(), "x.db")
	cfg.UIAuth = false
	cfg.ReadOnly = true
	s, err := New(context.Background(), cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	hs := httptest.NewServer(s.Handler)
	defer hs.Close()
	if code, _ := get(t, hs.Client(), hs.URL+"/keys"); code != 200 {
		t.Errorf("keys page = %d", code)
	}
	if code, _ := post(t, hs.Client(), hs.URL+"/keys", "name=x", "Content-Type", "application/x-www-form-urlencoded"); code != 403 {
		t.Errorf("read-only POST = %d", code)
	}
}
