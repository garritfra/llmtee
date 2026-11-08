// Package pricing computes request costs from an embedded copy of LiteLLM's
// price table (MIT) plus optional local overrides from a prices.yaml file.
package pricing

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/garritfra/llmtee/internal/usage"
)

//go:embed prices.json
var embedded []byte

// Price holds USD per token rates.
type Price struct {
	Input              float64 `json:"input_cost_per_token"`
	Output             float64 `json:"output_cost_per_token"`
	CacheRead          float64 `json:"cache_read_input_token_cost"`
	CacheWrite         float64 `json:"cache_creation_input_token_cost"`
	CacheWrite1h       float64 `json:"cache_creation_input_token_cost_above_1hr"`
	InputAbove200k     float64 `json:"input_cost_per_token_above_200k_tokens"`
	OutputAbove200k    float64 `json:"output_cost_per_token_above_200k_tokens"`
	CacheReadAbove200k float64 `json:"cache_read_input_token_cost_above_200k_tokens"`
	CacheWriteAbove200 float64 `json:"cache_creation_input_token_cost_above_200k_tokens"`
	Provider           string  `json:"litellm_provider"`
}

// Override is a user-supplied price in USD per million tokens.
type Override struct {
	Input      float64  `yaml:"input"`
	Output     float64  `yaml:"output"`
	CacheRead  *float64 `yaml:"cache_read"`
	CacheWrite *float64 `yaml:"cache_write"`
}

func (o Override) price() Price {
	p := Price{Input: o.Input / 1e6, Output: o.Output / 1e6, CacheRead: o.Input / 1e6, CacheWrite: o.Input / 1e6}
	if o.CacheRead != nil {
		p.CacheRead = *o.CacheRead / 1e6
	}
	if o.CacheWrite != nil {
		p.CacheWrite = *o.CacheWrite / 1e6
	}
	return p
}

// OverrideFile is the format of prices.yaml.
//
//	models:
//	  "llama3.1:8b": {input: 0, output: 0}
//	  "my-org/*":    {input: 0.5, output: 1.5}      # glob patterns allowed
//	upstreams:
//	  ollama:        {input: 0, output: 0}          # default for every model on an upstream
type OverrideFile struct {
	Models    map[string]Override `yaml:"models"`
	Upstreams map[string]Override `yaml:"upstreams"`
}

type pattern struct {
	glob  string
	price Price
}

// Table resolves prices for (upstream, model) pairs.
type Table struct {
	models    map[string]Price
	exact     map[string]Price
	globs     []pattern
	upstreams map[string]Price
}

// Load builds a table from the embedded data and an optional overrides file.
func Load(overridesPath string) (*Table, error) {
	t := &Table{exact: map[string]Price{}, upstreams: map[string]Price{}}
	if err := json.Unmarshal(embedded, &t.models); err != nil {
		return nil, fmt.Errorf("embedded prices: %w", err)
	}
	// Normalize keys to lower case for lookups.
	lower := make(map[string]Price, len(t.models))
	for k, p := range t.models {
		lower[strings.ToLower(k)] = p
	}
	t.models = lower
	if overridesPath == "" {
		return t, nil
	}
	b, err := os.ReadFile(overridesPath)
	if err != nil {
		return nil, fmt.Errorf("prices file: %w", err)
	}
	var f OverrideFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("prices file %s: %w", overridesPath, err)
	}
	t.ApplyOverrides(f)
	return t, nil
}

// ApplyOverrides adds local price overrides.
func (t *Table) ApplyOverrides(f OverrideFile) {
	for k, o := range f.Models {
		k = strings.ToLower(k)
		if strings.ContainsAny(k, "*?[") {
			t.globs = append(t.globs, pattern{k, o.price()})
		} else {
			t.exact[k] = o.price()
		}
	}
	sort.Slice(t.globs, func(i, j int) bool { return len(t.globs[i].glob) > len(t.globs[j].glob) })
	for k, o := range f.Upstreams {
		t.upstreams[k] = o.price()
	}
}

// Models returns the number of embedded models.
func (t *Table) Models() int { return len(t.models) }

var dateSuffix = regexp.MustCompile(`-(\d{4}-\d{2}-\d{2}|\d{8})$`)

// Lookup returns the price for a model served by an upstream.
func (t *Table) Lookup(upstream, model string) (Price, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		if p, ok := t.upstreams[upstream]; ok {
			return p, true
		}
		return Price{}, false
	}
	if p, ok := t.exact[m]; ok {
		return p, true
	}
	for _, g := range t.globs {
		if ok, _ := path.Match(g.glob, m); ok {
			return g.price, true
		}
	}
	if p, ok := t.upstreams[upstream]; ok {
		return p, true
	}
	for _, c := range candidates(upstream, m) {
		if p, ok := t.models[c]; ok {
			return p, true
		}
	}
	return Price{}, false
}

func candidates(upstream, m string) []string {
	c := []string{m}
	if upstream != "" {
		c = append(c, upstream+"/"+m)
	}
	if base := dateSuffix.ReplaceAllString(m, ""); base != m {
		c = append(c, base)
		if upstream != "" {
			c = append(c, upstream+"/"+base)
		}
	}
	// "anthropic/claude-x" (OpenRouter style) -> "claude-x"
	if i := strings.LastIndexByte(m, '/'); i >= 0 {
		c = append(c, m[i+1:], dateSuffix.ReplaceAllString(m[i+1:], ""))
	}
	// OpenRouter writes "claude-sonnet-4.5" where the table has "claude-sonnet-4-5".
	for _, x := range c {
		if strings.Contains(x, ".") && strings.Contains(x, "claude") {
			c = append(c, strings.ReplaceAll(x, ".", "-"))
		}
	}
	return c
}

// Cost computes the USD cost of a request:
//
//	cost = t_in*p_in + t_out*p_out + t_cache_read*p_cache_read + t_cache_write*p_cache_write
func Cost(p Price, u usage.Usage) float64 {
	in, out, cr, cw, cw1h := p.Input, p.Output, p.CacheRead, p.CacheWrite, p.CacheWrite1h
	if u.Input+u.CacheRead+u.CacheWrite > 200_000 {
		if p.InputAbove200k > 0 {
			in = p.InputAbove200k
		}
		if p.OutputAbove200k > 0 {
			out = p.OutputAbove200k
		}
		if p.CacheReadAbove200k > 0 {
			cr = p.CacheReadAbove200k
		}
		if p.CacheWriteAbove200 > 0 {
			cw = p.CacheWriteAbove200
		}
	}
	if cw1h == 0 {
		cw1h = cw
	}
	w5m := u.CacheWrite - u.CacheWrite1h
	return float64(u.Input)*in + float64(u.Output)*out + float64(u.CacheRead)*cr +
		float64(w5m)*cw + float64(u.CacheWrite1h)*cw1h
}

// Compute returns the cost for usage, or nil when usage or price is unknown.
// A cost reported by the provider (OpenRouter) takes precedence.
func (t *Table) Compute(upstream string, models []string, u usage.Usage) *float64 {
	if !u.Known {
		return nil
	}
	if u.Cost != nil {
		c := *u.Cost
		return &c
	}
	for _, m := range models {
		if m == "" {
			continue
		}
		if p, ok := t.Lookup(upstream, m); ok {
			c := Cost(p, u)
			return &c
		}
	}
	if p, ok := t.upstreams[upstream]; ok {
		c := Cost(p, u)
		return &c
	}
	return nil
}
