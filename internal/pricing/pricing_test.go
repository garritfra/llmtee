package pricing

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/garritfra/llmtee/internal/usage"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestLookup(t *testing.T) {
	tb, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if tb.Models() < 1000 {
		t.Fatalf("only %d models embedded", tb.Models())
	}
	for _, tc := range []struct{ upstream, model string }{
		{"openai", "gpt-4o-mini"},
		{"openai", "gpt-4o-mini-2024-07-18"},
		{"openai", "GPT-4o"},
		{"anthropic", "claude-sonnet-4-5-20250929"},
		{"anthropic", "claude-sonnet-4-5"},
		{"openrouter", "anthropic/claude-sonnet-4.5"},
		{"openrouter", "openai/gpt-4o-mini"},
		{"openai", "text-embedding-3-small"},
	} {
		if _, ok := tb.Lookup(tc.upstream, tc.model); !ok {
			t.Errorf("no price for %s %s", tc.upstream, tc.model)
		}
	}
	if _, ok := tb.Lookup("ollama", "llama3.1:8b"); ok {
		t.Error("unexpected price for local model")
	}
}

func TestCostFormula(t *testing.T) {
	p := Price{Input: 3e-6, Output: 15e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6, CacheWrite1h: 6e-6}
	u := usage.Usage{Known: true, Input: 1000, Output: 200, CacheRead: 5000, CacheWrite: 300, CacheWrite1h: 100}
	want := 1000*3e-6 + 200*15e-6 + 5000*0.3e-6 + 200*3.75e-6 + 100*6e-6
	if got := Cost(p, u); !approx(got, want) {
		t.Fatalf("cost %v want %v", got, want)
	}
	// Long-context tier.
	p.InputAbove200k, p.OutputAbove200k = 6e-6, 22.5e-6
	u = usage.Usage{Known: true, Input: 250_000, Output: 1000}
	if got := Cost(p, u); !approx(got, 250_000*6e-6+1000*22.5e-6) {
		t.Fatalf("200k tier cost %v", got)
	}
}

func TestComputeAndOverrides(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "prices.yaml")
	os.WriteFile(f, []byte(`
models:
  "llama3.1:8b": {input: 0, output: 0}
  "my-org/*": {input: 1, output: 2, cache_read: 0.1}
  gpt-4o-mini: {input: 100, output: 100}
upstreams:
  vllm: {input: 0.5, output: 0.5}
`), 0o644)
	tb, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	u := usage.Usage{Known: true, Input: 1_000_000, Output: 1_000_000, CacheRead: 1_000_000}
	cases := map[[2]string]float64{
		{"ollama", "llama3.1:8b"}: 0,
		{"x", "my-org/model-a"}:   1 + 2 + 0.1,
		{"openai", "gpt-4o-mini"}: 300, // local override beats embedded
		{"vllm", "anything"}:      1.5,
	}
	for k, want := range cases {
		c := tb.Compute(k[0], []string{k[1]}, u)
		if c == nil || !approx(*c, want) {
			t.Errorf("%v: got %v want %v", k, c, want)
		}
	}
	if c := tb.Compute("ollama", []string{"unknown-model"}, u); c != nil {
		t.Errorf("unknown model priced: %v", *c)
	}
	if c := tb.Compute("openai", []string{"gpt-4o"}, usage.Usage{}); c != nil {
		t.Error("unknown usage priced")
	}
	pc := 0.25
	if c := tb.Compute("openrouter", []string{"whatever"}, usage.Usage{Known: true, Cost: &pc}); c == nil || *c != 0.25 {
		t.Error("provider cost not used")
	}
	// Response model first, request model as fallback.
	if c := tb.Compute("openai", []string{"nonexistent-xyz", "gpt-4o-mini"}, u); c == nil {
		t.Error("fallback to request model failed")
	}
}

func TestBadOverrideFile(t *testing.T) {
	if _, err := Load("/does/not/exist.yaml"); err == nil {
		t.Error("missing file accepted")
	}
}
