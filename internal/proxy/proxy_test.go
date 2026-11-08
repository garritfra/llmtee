package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/garritfra/llmtee/internal/budget"
	"github.com/garritfra/llmtee/internal/config"
	"github.com/garritfra/llmtee/internal/fakeupstream"
	"github.com/garritfra/llmtee/internal/keys"
	"github.com/garritfra/llmtee/internal/pipeline"
	"github.com/garritfra/llmtee/internal/pricing"
	"github.com/garritfra/llmtee/internal/store"
	"github.com/garritfra/llmtee/internal/usage"
)

type sink struct {
	mu  sync.Mutex
	evs []*pipeline.Event
}

func (s *sink) Submit(ev *pipeline.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evs = append(s.evs, ev)
}

func (s *sink) last(t *testing.T) *pipeline.Event {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.evs) == 0 {
		t.Fatal("no event submitted")
	}
	return s.evs[len(s.evs)-1]
}

type env struct {
	fake    *fakeupstream.Server
	proxy   *httptest.Server
	sink    *sink
	keys    *keys.Cache
	budgets *budget.Tracker
	cfg     *config.Config
}

func newEnv(t *testing.T, mutate ...func(*config.Config)) *env {
	t.Helper()
	fake := &fakeupstream.Server{}
	up := httptest.NewServer(fake)
	t.Cleanup(up.Close)
	cfg := config.Defaults()
	cfg.Upstreams = []config.Upstream{
		{Name: "openai", Type: config.TypeOpenAI, BaseURL: up.URL, APIKey: "sk-provider-openai"},
		{Name: "anthropic", Type: config.TypeAnthropic, BaseURL: up.URL, APIKey: "sk-ant-provider"},
		{Name: "openrouter", Type: config.TypeOpenAI, BaseURL: up.URL + "/api"},
		{Name: "ollama", Type: config.TypeOpenAI, BaseURL: up.URL},
	}
	for _, m := range mutate {
		m(cfg)
	}
	e := &env{fake: fake, sink: &sink{}, keys: keys.NewCache(), budgets: budget.New(), cfg: cfg}
	p := New(cfg, e.keys, e.budgets, e.sink, nil)
	e.proxy = httptest.NewServer(p)
	t.Cleanup(e.proxy.Close)
	return e
}

func (e *env) do(t *testing.T, path, body string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.proxy.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

// Golden tests: the client must receive byte-identical streams and the
// parsed usage must match the usage reported in the stream.
func TestGoldenStreams(t *testing.T) {
	cases := []struct {
		name, path, body, fixture string
		want                      usage.Usage
	}{
		{"openai chat", "/openai/v1/chat/completions", `{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			"openai_chat_stream.sse", usage.Usage{Known: true, Model: "gpt-4o-mini-2024-07-18", Input: 219, CacheRead: 1024, Output: 9}},
		{"openai responses", "/openai/v1/responses", `{"model":"gpt-4.1","stream":true,"input":"hi"}`,
			"openai_responses_stream.sse", usage.Usage{Known: true, Model: "gpt-4.1-2025-04-14", Input: 52, CacheRead: 2048, Output: 7}},
		{"anthropic messages", "/anthropic/v1/messages", `{"model":"claude-sonnet-4-5","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`,
			"anthropic_stream.sse", usage.Usage{Known: true, Model: "claude-sonnet-4-5-20250929", Input: 12, CacheRead: 20480, CacheWrite: 1500, Output: 21}},
	}
	for _, random := range []bool{false, true} {
		for _, tc := range cases {
			name := tc.name
			if random {
				name += " random chunks"
			}
			t.Run(name, func(t *testing.T) {
				e := newEnv(t)
				e.fake.RandomChunks = random
				resp, got := e.do(t, tc.path, tc.body, map[string]string{"Authorization": "Bearer sk-test"})
				if resp.StatusCode != 200 {
					t.Fatalf("status %d: %s", resp.StatusCode, got)
				}
				want := fakeupstream.Fixture(tc.fixture)
				if !bytes.Equal(got, want) {
					t.Fatalf("stream not byte-identical\n got: %q\nwant: %q", got, want)
				}
				if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
					t.Errorf("content-type %q", ct)
				}
				if resp.Header.Get(RequestIDHeader) == "" {
					t.Error("missing request id header")
				}
				ev := e.sink.last(t)
				if !ev.Streamed || ev.StreamUsage == nil {
					t.Fatalf("event not marked streamed: %+v", ev)
				}
				if *ev.StreamUsage != tc.want {
					t.Errorf("usage = %+v, want %+v", *ev.StreamUsage, tc.want)
				}
				if ev.TTFTMS == nil {
					t.Error("ttft not recorded")
				}
				if ev.Status != 200 || ev.Error != "" {
					t.Errorf("status=%d error=%q", ev.Status, ev.Error)
				}
			})
		}
	}
}

func TestGoldenNonStreaming(t *testing.T) {
	cases := []struct {
		name, path, body, fixture, format string
		want                              usage.Usage
	}{
		{"openai chat", "/openai/v1/chat/completions", `{"model":"gpt-4o-mini","messages":[]}`, "openai_chat.json", usage.FormatOpenAI,
			usage.Usage{Known: true, Model: "gpt-4o-mini-2024-07-18", Input: 19, Output: 9}},
		{"openai responses", "/openai/v1/responses", `{"model":"gpt-4.1","input":"hi"}`, "openai_responses.json", usage.FormatOpenAI,
			usage.Usage{Known: true, Model: "gpt-4.1-2025-04-14", Input: 36, Output: 7}},
		{"openai embeddings", "/openai/v1/embeddings", `{"model":"text-embedding-3-small","input":"hi"}`, "openai_embeddings.json", usage.FormatOpenAI,
			usage.Usage{Known: true, Model: "text-embedding-3-small", Input: 8}},
		{"anthropic", "/anthropic/v1/messages", `{"model":"claude-sonnet-4-5","max_tokens":10,"messages":[]}`, "anthropic.json", usage.FormatAnthropic,
			usage.Usage{Known: true, Model: "claude-sonnet-4-5-20250929", Input: 25, Output: 11}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			resp, got := e.do(t, tc.path, tc.body, map[string]string{"X-Api-Key": "sk-test"})
			if resp.StatusCode != 200 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			if !bytes.Equal(got, fakeupstream.Fixture(tc.fixture)) {
				t.Fatal("body not byte-identical")
			}
			ev := e.sink.last(t)
			if ev.Streamed || !ev.RespComplete {
				t.Fatalf("streamed=%v complete=%v", ev.Streamed, ev.RespComplete)
			}
			if u := usage.ParseBody(tc.format, ev.Resp); u != tc.want {
				t.Errorf("usage = %+v, want %+v", u, tc.want)
			}
		})
	}
}

func TestStreamOptionsInjection(t *testing.T) {
	e := newEnv(t)
	e.do(t, "/openai/v1/chat/completions", `{"model":"gpt-4o-mini","stream":true,"messages":[]}`, nil)
	var got map[string]any
	if err := json.Unmarshal(e.fake.Last().Body, &got); err != nil {
		t.Fatalf("upstream got invalid JSON: %v (%s)", err, e.fake.Last().Body)
	}
	so, _ := got["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Fatalf("include_usage not injected: %s", e.fake.Last().Body)
	}
	if got["model"] != "gpt-4o-mini" || got["stream"] != true {
		t.Fatalf("body mangled: %s", e.fake.Last().Body)
	}

	// Existing stream_options are merged, not replaced.
	e.do(t, "/openai/v1/chat/completions", `{"model":"m","stream":true,"stream_options":{"foo":1}}`, nil)
	json.Unmarshal(e.fake.Last().Body, &got)
	so, _ = got["stream_options"].(map[string]any)
	if so["include_usage"] != true || so["foo"] != float64(1) {
		t.Fatalf("merge failed: %s", e.fake.Last().Body)
	}

	// Disabled.
	e2 := newEnv(t, func(c *config.Config) { c.InjectStreamUsage = false })
	body := `{"model":"m","stream":true}`
	e2.do(t, "/openai/v1/chat/completions", body, nil)
	if string(e2.fake.Last().Body) != body {
		t.Fatalf("body modified although injection disabled: %s", e2.fake.Last().Body)
	}

	// Non-streaming requests are untouched.
	body = `{"model":"m","messages":[]}`
	e.do(t, "/openai/v1/chat/completions", body, nil)
	if string(e.fake.Last().Body) != body {
		t.Fatalf("non-stream body modified: %s", e.fake.Last().Body)
	}
}

func TestInjectIncludeUsageEdgeCases(t *testing.T) {
	for _, in := range []string{`{}`, ` { "stream" : true }`, `{"stream_options":null,"stream":true}`} {
		out, err := injectIncludeUsage([]byte(in))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("%s -> invalid %s", in, out)
		}
		if m["stream_options"].(map[string]any)["include_usage"] != true {
			t.Fatalf("%s -> %s", in, out)
		}
	}
}

func TestHeadersAndMetadata(t *testing.T) {
	e := newEnv(t)
	e.do(t, "/openai/v1/chat/completions", `{"model":"gpt-4o-mini","messages":[],"user":"body-user"}`, map[string]string{
		"Authorization":             "Bearer sk-passthrough",
		"Helicone-Auth":             "Bearer sk-helicone-old",
		"Helicone-User-Id":          "alice",
		"Helicone-Session-Id":       "sess-1",
		"Helicone-Property-Feature": "summarize",
		"Llmtee-Property-Env":       "prod",
		"Cookie":                    "llmtee_session=secret",
	})
	got := e.fake.Last().Header
	if got.Get("Authorization") != "Bearer sk-passthrough" {
		t.Errorf("provider key not passed through: %q", got.Get("Authorization"))
	}
	for k := range got {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "helicone-") || strings.HasPrefix(lk, "llmtee-") || lk == "cookie" {
			t.Errorf("header %s leaked upstream", k)
		}
	}
	ev := e.sink.last(t)
	if ev.UserID != "alice" || ev.SessionID != "sess-1" {
		t.Errorf("user=%q session=%q", ev.UserID, ev.SessionID)
	}
	if ev.Properties["feature"] != "summarize" || ev.Properties["env"] != "prod" {
		t.Errorf("properties = %v", ev.Properties)
	}
	if want := keys.ID(keys.Passthrough, keys.Hash("sk-passthrough")); ev.KeyID != want {
		t.Errorf("key id = %q, want %q", ev.KeyID, want)
	}
	if ev.ReqModel != "gpt-4o-mini" || ev.Path != "/v1/chat/completions" || ev.Upstream != "openai" {
		t.Errorf("event = %+v", ev)
	}

	// Falls back to the body's user field.
	e.do(t, "/openai/v1/chat/completions", `{"model":"m","user":"body-user"}`, nil)
	if ev := e.sink.last(t); ev.UserID != "body-user" {
		t.Errorf("user fallback = %q", ev.UserID)
	}
	e.do(t, "/anthropic/v1/messages", `{"model":"m","metadata":{"user_id":"u-42"}}`, nil)
	if ev := e.sink.last(t); ev.UserID != "u-42" {
		t.Errorf("anthropic metadata user = %q", ev.UserID)
	}
}

func managedKey(e *env, day *float64) (string, *store.Key) {
	secret := keys.NewSecret()
	k := keys.New(keys.Managed, "test", secret, day, nil)
	e.keys.Set([]*store.Key{k})
	return secret, k
}

func TestManagedKeyInjection(t *testing.T) {
	e := newEnv(t)
	secret, k := managedKey(e, nil)

	e.do(t, "/openai/v1/chat/completions", `{"model":"m"}`, map[string]string{"Authorization": "Bearer " + secret})
	if got := e.fake.Last().Header.Get("Authorization"); got != "Bearer sk-provider-openai" {
		t.Errorf("openai provider key not injected: %q", got)
	}
	if e.sink.last(t).KeyID != k.ID {
		t.Errorf("key id %q", e.sink.last(t).KeyID)
	}

	e.do(t, "/anthropic/v1/messages", `{"model":"m"}`, map[string]string{"X-Api-Key": secret})
	h := e.fake.Last().Header
	if h.Get("X-Api-Key") != "sk-ant-provider" || h.Get("Authorization") != "" {
		t.Errorf("anthropic key not injected: %v", h)
	}

	// Proxy key in a side header + provider key passthrough.
	e.do(t, "/openai/v1/chat/completions", `{"model":"m"}`, map[string]string{"Authorization": "Bearer sk-own", "Llmtee-Key": secret})
	if got := e.fake.Last().Header.Get("Authorization"); got != "Bearer sk-own" {
		t.Errorf("own provider key replaced: %q", got)
	}
	if e.sink.last(t).KeyID != k.ID {
		t.Error("side-header key not attributed")
	}

	// Unknown proxy key.
	resp, body := e.do(t, "/openai/v1/chat/completions", `{}`, map[string]string{"Authorization": "Bearer llmtee-nope"})
	if resp.StatusCode != 401 || !strings.Contains(string(body), "invalid proxy key") {
		t.Errorf("unknown key: %d %s", resp.StatusCode, body)
	}

	// Upstream without provider key.
	resp, body = e.do(t, "/u/ollama/v1/chat/completions", `{}`, map[string]string{"Authorization": "Bearer " + secret})
	if resp.StatusCode != 401 || !strings.Contains(string(body), "no provider API key") {
		t.Errorf("missing provider key: %d %s", resp.StatusCode, body)
	}
}

func TestRequireProxyKey(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.RequireProxyKey = true })
	resp, _ := e.do(t, "/openai/v1/chat/completions", `{}`, map[string]string{"Authorization": "Bearer sk-x"})
	if resp.StatusCode != 401 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	secret, _ := managedKey(e, nil)
	resp, _ = e.do(t, "/openai/v1/chat/completions", `{"model":"m"}`, map[string]string{"Authorization": "Bearer " + secret})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestBudgetExceeded(t *testing.T) {
	e := newEnv(t)
	limit := 1.0
	secret, k := managedKey(e, &limit)
	e.budgets.Add(k.ID, 0.99, time.Now())

	resp, _ := e.do(t, "/openai/v1/chat/completions", `{"model":"m"}`, map[string]string{"Authorization": "Bearer " + secret})
	if resp.StatusCode != 200 {
		t.Fatalf("under budget: %d", resp.StatusCode)
	}
	e.budgets.Add(k.ID, 0.02, time.Now())
	n := len(e.fake.Requests())
	for _, tc := range []struct{ path, hdr string }{{"/openai/v1/chat/completions", "Authorization"}, {"/anthropic/v1/messages", "X-Api-Key"}} {
		resp, body := e.do(t, tc.path, `{"model":"m"}`, map[string]string{tc.hdr: secret})
		if resp.StatusCode != 429 {
			t.Fatalf("over budget: %d", resp.StatusCode)
		}
		if !strings.Contains(string(body), "daily budget") || resp.Header.Get("Retry-After") == "" {
			t.Errorf("bad 429: %s %v", body, resp.Header)
		}
		ev := e.sink.last(t)
		if ev.Status != 429 || ev.KeyID != k.ID {
			t.Errorf("rejection not logged: %+v", ev)
		}
	}
	if strings.Contains(string(mustBody(t, e, "/anthropic/v1/messages", secret)), `"rate_limit_error"`) == false {
		t.Error("anthropic-style error expected")
	}
	if len(e.fake.Requests()) != n {
		t.Error("rejected request reached upstream")
	}
}

func mustBody(t *testing.T, e *env, path, secret string) []byte {
	_, b := e.do(t, path, `{}`, map[string]string{"X-Api-Key": secret})
	return b
}

func TestUpstreamErrors(t *testing.T) {
	e := newEnv(t, func(c *config.Config) {
		c.Upstreams = append(c.Upstreams, config.Upstream{Name: "dead", Type: config.TypeOpenAI, BaseURL: "http://127.0.0.1:1"})
	})
	resp, body := e.do(t, "/u/dead/v1/chat/completions", `{"model":"m"}`, nil)
	if resp.StatusCode != 502 || !strings.Contains(string(body), "upstream request failed") {
		t.Fatalf("dead upstream: %d %s", resp.StatusCode, body)
	}
	if ev := e.sink.last(t); ev.Status != 502 || ev.Error == "" {
		t.Errorf("event %+v", ev)
	}

	resp, body = e.do(t, "/openai/v1/chat/completions", `{"model":"m"}`, map[string]string{"X-Fake-Status": "500"})
	if resp.StatusCode != 500 || !strings.Contains(string(body), "server had an error") {
		t.Fatalf("upstream 500 not passed through: %d %s", resp.StatusCode, body)
	}

	resp, _ = e.do(t, "/nope/v1/chat/completions", `{}`, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown route: %d", resp.StatusCode)
	}
	resp, _ = e.do(t, "/u/missing/v1/chat/completions", `{}`, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown upstream: %d", resp.StatusCode)
	}
}

func TestRouting(t *testing.T) {
	e := newEnv(t)
	e.do(t, "/openrouter/v1/chat/completions?x=1", `{"model":"openai/gpt-4o"}`, nil)
	if got := e.fake.Last().Path; got != "/api/v1/chat/completions" {
		t.Errorf("openrouter path = %q", got)
	}
	e.do(t, "/u/ollama/v1/embeddings", `{"model":"nomic"}`, nil)
	if got := e.fake.Last().Path; got != "/v1/embeddings" {
		t.Errorf("custom path = %q", got)
	}
	n := len(e.sink.evs)
	req, _ := http.NewRequest(http.MethodGet, e.proxy.URL+"/openai/v1/models", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET models: %v %v", err, resp)
	}
	resp.Body.Close()
	if len(e.sink.evs) != n {
		t.Error("GET requests should not be logged")
	}
}

// The full path from proxy event to priced database row.
func TestEventToRow(t *testing.T) {
	e := newEnv(t)
	e.do(t, "/anthropic/v1/messages", `{"model":"claude-sonnet-4-5","stream":true}`, map[string]string{"X-Api-Key": "k"})
	pt, err := pricing.Load("")
	if err != nil {
		t.Fatal(err)
	}
	p := pipeline.New(pipeline.Options{QueueSize: 1, BatchSize: 1, FlushInterval: time.Second, MaxBodyBytes: 1 << 20}, nil, pt, budget.New(), nil, nil, nil)
	r := p.Process(e.sink.last(t))
	if r.CostUSD == nil {
		t.Fatal("no cost")
	}
	// 12*3e-6 + 21*15e-6 + 20480*0.3e-6 + 1500*3.75e-6
	want := 12*3e-6 + 21*15e-6 + 20480*0.3e-6 + 1500*3.75e-6
	if d := *r.CostUSD - want; d > 1e-9 || d < -1e-9 {
		t.Errorf("cost = %v, want %v", *r.CostUSD, want)
	}
	if *r.TokensIn != 12 || *r.TokensOut != 21 || *r.TokensCacheRead != 20480 || *r.TokensCacheWrite != 1500 {
		t.Errorf("tokens %d %d %d %d", *r.TokensIn, *r.TokensOut, *r.TokensCacheRead, *r.TokensCacheWrite)
	}
}
