// Package config loads llmtee's configuration from defaults, an optional
// YAML file and environment variables (in increasing precedence).
package config

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// API formats of upstreams.
const (
	TypeOpenAI    = "openai"
	TypeAnthropic = "anthropic"
)

// Upstream is an LLM API llmtee forwards to.
type Upstream struct {
	Name    string `yaml:"name"`
	Type    string `yaml:"type"`     // openai | anthropic
	BaseURL string `yaml:"base_url"` // e.g. https://api.openai.com (without /v1)
	// APIKey is injected when a client authenticates with a managed proxy key.
	APIKey    string `yaml:"api_key"`
	LogBodies *bool  `yaml:"log_bodies"`
}

// Alerts configures budget notifications.
type Alerts struct {
	WebhookURL     string    `yaml:"webhook_url"`
	NtfyURL        string    `yaml:"ntfy_url"` // full topic URL, e.g. https://ntfy.sh/my-llm-alerts
	NtfyToken      string    `yaml:"ntfy_token"`
	Thresholds     []float64 `yaml:"thresholds"` // fractions of a budget, default [0.8, 1.0]
	GlobalDayUSD   float64   `yaml:"global_budget_day_usd"`
	GlobalMonthUSD float64   `yaml:"global_budget_month_usd"`
}

// Config is the complete configuration.
type Config struct {
	Listen        string `yaml:"listen"`
	DatabaseURL   string `yaml:"database_url"`
	AdminPassword string `yaml:"admin_password"`
	// UIAuth can be disabled when a reverse proxy already authenticates.
	UIAuth bool `yaml:"ui_auth"`
	// ReadOnly turns the UI into a read-only demo (no key management).
	ReadOnly bool `yaml:"read_only"`

	RequireProxyKey   bool     `yaml:"require_proxy_key"`
	LogBodies         bool     `yaml:"log_bodies"`
	MaxBodyBytes      int64    `yaml:"max_body_bytes"`
	BodyRetentionDays int      `yaml:"body_retention_days"`
	RetentionDays     int      `yaml:"retention_days"`
	RedactFields      []string `yaml:"redact_fields"`
	InjectStreamUsage bool     `yaml:"inject_stream_usage"`
	PricesFile        string   `yaml:"prices_file"`

	QueueSize     int           `yaml:"queue_size"`
	BatchSize     int           `yaml:"batch_size"`
	FlushInterval time.Duration `yaml:"flush_interval"`
	// BudgetSync is how often budget counters are re-read from the database
	// (PostgreSQL only, for multiple proxy instances).
	BudgetSync time.Duration `yaml:"budget_sync"`

	Upstreams []Upstream `yaml:"upstreams"`
	Alerts    Alerts     `yaml:"alerts"`
}

// Defaults returns the built-in configuration.
func Defaults() *Config {
	return &Config{
		Listen:            ":8080",
		DatabaseURL:       "",
		UIAuth:            true,
		LogBodies:         true,
		MaxBodyBytes:      2 << 20,
		BodyRetentionDays: 30,
		InjectStreamUsage: true,
		QueueSize:         10000,
		BatchSize:         100,
		FlushInterval:     500 * time.Millisecond,
		BudgetSync:        10 * time.Second,
		Alerts:            Alerts{Thresholds: []float64{0.8, 1.0}},
	}
}

// BuiltinUpstreams are always available unless overridden by name.
func BuiltinUpstreams() []Upstream {
	return []Upstream{
		{Name: "openai", Type: TypeOpenAI, BaseURL: "https://api.openai.com"},
		{Name: "anthropic", Type: TypeAnthropic, BaseURL: "https://api.anthropic.com"},
		{Name: "openrouter", Type: TypeOpenAI, BaseURL: "https://openrouter.ai/api"},
	}
}

// Load reads the YAML file at path (optional) and applies environment overrides.
func Load(path string) (*Config, error) {
	c := Defaults()
	if path == "" {
		path = os.Getenv("LLMTEE_CONFIG")
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		if err := yaml.Unmarshal([]byte(os.ExpandEnv(string(b))), c); err != nil {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
	}
	if err := c.applyEnv(os.Environ()); err != nil {
		return nil, err
	}
	if err := c.finish(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) applyEnv(environ []string) error {
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	var errs []string
	str := func(name string, dst *string) {
		if v, ok := env[name]; ok && v != "" {
			*dst = v
		}
	}
	boolean := func(name string, dst *bool) {
		if v, ok := env[name]; ok && v != "" {
			switch strings.ToLower(v) {
			case "1", "true", "yes", "on":
				*dst = true
			case "0", "false", "no", "off":
				*dst = false
			default:
				errs = append(errs, fmt.Sprintf("%s: invalid boolean %q", name, v))
			}
		}
	}
	integer := func(name string, dst *int) {
		if v, ok := env[name]; ok && v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			}
			*dst = n
		}
	}
	str("LLMTEE_LISTEN", &c.Listen)
	str("DATABASE_URL", &c.DatabaseURL)
	str("LLMTEE_DATABASE_URL", &c.DatabaseURL)
	str("LLMTEE_ADMIN_PASSWORD", &c.AdminPassword)
	boolean("LLMTEE_UI_AUTH", &c.UIAuth)
	boolean("LLMTEE_READ_ONLY", &c.ReadOnly)
	boolean("LLMTEE_REQUIRE_PROXY_KEY", &c.RequireProxyKey)
	boolean("LLMTEE_LOG_BODIES", &c.LogBodies)
	boolean("LLMTEE_INJECT_STREAM_USAGE", &c.InjectStreamUsage)
	integer("LLMTEE_BODY_RETENTION_DAYS", &c.BodyRetentionDays)
	integer("LLMTEE_RETENTION_DAYS", &c.RetentionDays)
	str("LLMTEE_PRICES_FILE", &c.PricesFile)
	if v := env["LLMTEE_MAX_BODY_BYTES"]; v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			errs = append(errs, "LLMTEE_MAX_BODY_BYTES: "+err.Error())
		}
		c.MaxBodyBytes = n
	}
	if v := env["LLMTEE_REDACT_FIELDS"]; v != "" {
		c.RedactFields = splitList(v)
	}
	str("LLMTEE_ALERT_WEBHOOK_URL", &c.Alerts.WebhookURL)
	str("LLMTEE_ALERT_NTFY_URL", &c.Alerts.NtfyURL)
	str("LLMTEE_ALERT_NTFY_TOKEN", &c.Alerts.NtfyToken)
	if v := env["LLMTEE_ALERT_THRESHOLDS"]; v != "" {
		c.Alerts.Thresholds = nil
		for _, s := range splitList(v) {
			f, err := strconv.ParseFloat(s, 64)
			if err != nil {
				errs = append(errs, "LLMTEE_ALERT_THRESHOLDS: "+err.Error())
			}
			c.Alerts.Thresholds = append(c.Alerts.Thresholds, f)
		}
	}
	for name, dst := range map[string]*float64{
		"LLMTEE_ALERT_GLOBAL_BUDGET_DAY_USD":   &c.Alerts.GlobalDayUSD,
		"LLMTEE_ALERT_GLOBAL_BUDGET_MONTH_USD": &c.Alerts.GlobalMonthUSD,
	} {
		if v := env[name]; v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				errs = append(errs, name+": "+err.Error())
			}
			*dst = f
		}
	}

	// Upstreams: LLMTEE_UPSTREAM_<NAME>=<base url>, optional _TYPE, _API_KEY, _LOG_BODIES.
	var names []string
	for k := range env {
		if rest, ok := strings.CutPrefix(k, "LLMTEE_UPSTREAM_"); ok && !strings.Contains(rest, "_") {
			names = append(names, rest)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		u := Upstream{Name: strings.ToLower(n), BaseURL: env["LLMTEE_UPSTREAM_"+n], Type: env["LLMTEE_UPSTREAM_"+n+"_TYPE"],
			APIKey: env["LLMTEE_UPSTREAM_"+n+"_API_KEY"]}
		if v := env["LLMTEE_UPSTREAM_"+n+"_LOG_BODIES"]; v != "" {
			b := v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "on")
			u.LogBodies = &b
		}
		c.setUpstream(u)
	}
	// Provider keys for managed mode on built-in upstreams.
	for name, envName := range map[string]string{"openai": "OPENAI_API_KEY", "anthropic": "ANTHROPIC_API_KEY", "openrouter": "OPENROUTER_API_KEY"} {
		if v := env[envName]; v != "" {
			c.setUpstream(Upstream{Name: name, APIKey: v})
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return nil
}

// setUpstream merges u into the upstream list (non-empty fields win).
func (c *Config) setUpstream(u Upstream) {
	for i := range c.Upstreams {
		if c.Upstreams[i].Name == u.Name {
			e := &c.Upstreams[i]
			if u.BaseURL != "" {
				e.BaseURL = u.BaseURL
			}
			if u.Type != "" {
				e.Type = u.Type
			}
			if u.APIKey != "" {
				e.APIKey = u.APIKey
			}
			if u.LogBodies != nil {
				e.LogBodies = u.LogBodies
			}
			return
		}
	}
	c.Upstreams = append(c.Upstreams, u)
}

func (c *Config) finish() error {
	// Fill built-ins underneath user-defined upstreams.
	user := c.Upstreams
	c.Upstreams = BuiltinUpstreams()
	for _, u := range user {
		u.Name = strings.ToLower(u.Name)
		c.setUpstream(u)
	}
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		if u.Type == "" {
			u.Type = TypeOpenAI
		}
		if u.Type != TypeOpenAI && u.Type != TypeAnthropic {
			return fmt.Errorf("upstream %s: unknown type %q (openai or anthropic)", u.Name, u.Type)
		}
		if u.BaseURL == "" {
			return fmt.Errorf("upstream %s: base_url missing", u.Name)
		}
		u.BaseURL = strings.TrimSuffix(strings.TrimSuffix(u.BaseURL, "/"), "/v1")
		if u.Name == "" || strings.ContainsAny(u.Name, "/ ") {
			return fmt.Errorf("upstream name %q invalid", u.Name)
		}
	}
	if c.QueueSize <= 0 {
		c.QueueSize = 10000
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = 500 * time.Millisecond
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = 2 << 20
	}
	if len(c.Alerts.Thresholds) == 0 {
		c.Alerts.Thresholds = []float64{0.8, 1.0}
	}
	sort.Float64s(c.Alerts.Thresholds)
	return nil
}

// Upstream returns the upstream with the given name.
func (c *Config) Upstream(name string) (*Upstream, bool) {
	for i := range c.Upstreams {
		if c.Upstreams[i].Name == name {
			return &c.Upstreams[i], true
		}
	}
	return nil, false
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
