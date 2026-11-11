package ui

import (
	"crypto/tls"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/garritfra/llmtee/internal/config"
)

func TestBaseURL(t *testing.T) {
	u := &UI{cfg: config.Defaults()}
	r := httptest.NewRequest("GET", "http://box:8080/setup", nil)
	if got := u.baseURL(r); got != "http://box:8080" {
		t.Errorf("plain = %q", got)
	}
	r.TLS = &tls.ConnectionState{}
	if got := u.baseURL(r); got != "https://box:8080" {
		t.Errorf("tls = %q", got)
	}
	r = httptest.NewRequest("GET", "http://127.0.0.1:8080/setup", nil)
	r.Header.Set("X-Forwarded-Proto", "https, http")
	r.Header.Set("X-Forwarded-Host", "llm.example.com")
	if got := u.baseURL(r); got != "https://llm.example.com" {
		t.Errorf("forwarded = %q", got)
	}
	r.Header.Set("X-Forwarded-Host", "<script>")
	if got := u.baseURL(r); got != "https://127.0.0.1:8080" {
		t.Errorf("invalid forwarded host = %q", got)
	}
	u.cfg.PublicURL = "https://llm.example.com/tee"
	if got := u.baseURL(r); got != "https://llm.example.com/tee" {
		t.Errorf("public url = %q", got)
	}
}

func TestEndpoints(t *testing.T) {
	cfg := config.Defaults()
	cfg.Upstreams = append(config.BuiltinUpstreams(),
		config.Upstream{Name: "ollama", Type: config.TypeOpenAI, BaseURL: "http://ollama:11434"},
		config.Upstream{Name: "claude", Type: config.TypeAnthropic, BaseURL: "http://cp", APIKey: "k"})
	want := map[string]string{
		"openai":     "http://h/openai/v1",
		"anthropic":  "http://h/anthropic",
		"openrouter": "http://h/openrouter/v1",
		"ollama":     "http://h/u/ollama/v1",
		"claude":     "http://h/u/claude",
	}
	for _, e := range endpoints(cfg, "http://h") {
		if e.URL != want[e.Name] {
			t.Errorf("%s = %q, want %q", e.Name, e.URL, want[e.Name])
		}
		if e.ManagedKey != (e.Name == "claude") {
			t.Errorf("%s managed = %v", e.Name, e.ManagedKey)
		}
	}
}

func TestRenderGuide(t *testing.T) {
	g, err := renderGuide("https://llm.example.com")
	if err != nil {
		t.Fatal(err)
	}
	s := string(g)
	for _, bad := range []string{"localhost:8080", "&lt;llmtee&gt;", "setup-page:skip", "<h1", `href="headers.md`} {
		if strings.Contains(s, bad) {
			t.Errorf("guide contains %q", bad)
		}
	}
	for _, want := range []string{"ANTHROPIC_BASE_URL=https://llm.example.com/anthropic", `id="claude-code"`, `href="` + docsURL + `headers.md`} {
		if !strings.Contains(s, want) {
			t.Errorf("guide misses %q", want)
		}
	}
}
