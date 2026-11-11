package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsAndEnv(t *testing.T) {
	c := Defaults()
	err := c.applyEnv([]string{
		"LLMTEE_LISTEN=:9000", "DATABASE_URL=postgres://x/y", "LLMTEE_LOG_BODIES=false",
		"LLMTEE_UPSTREAM_OLLAMA=http://ollama:11434/v1", "LLMTEE_UPSTREAM_GROQ=https://api.groq.com/openai/v1",
		"LLMTEE_UPSTREAM_GROQ_API_KEY=gsk", "LLMTEE_UPSTREAM_CLAUDEPROXY=http://cp", "LLMTEE_UPSTREAM_CLAUDEPROXY_TYPE=anthropic",
		"OPENAI_API_KEY=sk-o", "LLMTEE_REDACT_FIELDS=api_key, password", "LLMTEE_ALERT_THRESHOLDS=1,0.5",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9000" || c.DatabaseURL != "postgres://x/y" || c.LogBodies || len(c.RedactFields) != 2 {
		t.Errorf("cfg = %+v", c)
	}
	if c.Alerts.Thresholds[0] != 0.5 {
		t.Errorf("thresholds not sorted: %v", c.Alerts.Thresholds)
	}
	o, _ := c.Upstream("openai")
	if o.APIKey != "sk-o" || o.BaseURL != "https://api.openai.com" || o.Type != TypeOpenAI {
		t.Errorf("openai = %+v", o)
	}
	ol, ok := c.Upstream("ollama")
	if !ok || ol.BaseURL != "http://ollama:11434" || ol.Type != TypeOpenAI {
		t.Errorf("ollama = %+v", ol)
	}
	g, _ := c.Upstream("groq")
	if g.BaseURL != "https://api.groq.com/openai" || g.APIKey != "gsk" {
		t.Errorf("groq = %+v", g)
	}
	cp, _ := c.Upstream("claudeproxy")
	if cp.Type != TypeAnthropic {
		t.Errorf("claudeproxy = %+v", cp)
	}
}

func TestYAMLAndValidation(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "c.yaml")
	os.Setenv("TEST_VLLM_KEY", "vk")
	defer os.Unsetenv("TEST_VLLM_KEY")
	os.WriteFile(f, []byte(`
listen: ":7000"
log_bodies: false
upstreams:
  - name: vllm
    base_url: http://gpu:8000
    api_key: ${TEST_VLLM_KEY}
    log_bodies: true
  - name: anthropic
    base_url: https://anthropic.example
alerts:
  ntfy_url: https://ntfy.sh/x
`), 0o644)
	c, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := c.Upstream("vllm")
	if v.APIKey != "vk" || v.LogBodies == nil || !*v.LogBodies || c.Listen != ":7000" {
		t.Errorf("vllm = %+v", v)
	}
	a, _ := c.Upstream("anthropic")
	if a.BaseURL != "https://anthropic.example" || a.Type != TypeAnthropic {
		t.Errorf("anthropic override = %+v", a)
	}

	bad := Defaults()
	bad.Upstreams = []Upstream{{Name: "x", Type: "gemini", BaseURL: "http://x"}}
	if err := bad.finish(); err == nil {
		t.Error("unknown type accepted")
	}
	bad = Defaults()
	if err := bad.applyEnv([]string{"LLMTEE_LOG_BODIES=maybe"}); err == nil {
		t.Error("invalid bool accepted")
	}
	for in, want := range map[string]string{"https://llm.example.com/": "https://llm.example.com", "http://box:8080": "http://box:8080", "llm.example.com": "", "ftp://x": ""} {
		c := Defaults()
		c.PublicURL = in
		err := c.finish()
		if want == "" && err == nil {
			t.Errorf("public_url %q accepted", in)
		}
		if want != "" && (err != nil || c.PublicURL != want) {
			t.Errorf("public_url %q = %q, %v", in, c.PublicURL, err)
		}
	}
}
