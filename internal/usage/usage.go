// Package usage extracts token usage from OpenAI- and Anthropic-style
// responses, both from complete JSON bodies and from SSE streams.
//
// Token convention: Input counts only *uncached* input tokens. Cached input
// is reported separately as CacheRead (and CacheWrite for Anthropic prompt
// caching). For OpenAI this means Input = prompt_tokens - cached_tokens.
package usage

import (
	"bytes"
	"encoding/json"
)

// API formats.
const (
	FormatOpenAI    = "openai"
	FormatAnthropic = "anthropic"
)

// Usage is the normalized token usage of one request.
type Usage struct {
	Known        bool
	Model        string
	Input        int64
	Output       int64
	CacheRead    int64
	CacheWrite   int64
	CacheWrite1h int64    // subset of CacheWrite billed at the 1h TTL rate (Anthropic)
	Cost         *float64 // cost reported by the provider itself (OpenRouter), if any
}

type details struct {
	CachedTokens *int64 `json:"cached_tokens"`
}

type cacheCreation struct {
	Ephemeral5m *int64 `json:"ephemeral_5m_input_tokens"`
	Ephemeral1h *int64 `json:"ephemeral_1h_input_tokens"`
}

type rawUsage struct {
	PromptTokens             *int64         `json:"prompt_tokens"`
	CompletionTokens         *int64         `json:"completion_tokens"`
	PromptTokensDetails      *details       `json:"prompt_tokens_details"`
	InputTokens              *int64         `json:"input_tokens"`
	OutputTokens             *int64         `json:"output_tokens"`
	InputTokensDetails       *details       `json:"input_tokens_details"`
	CacheCreationInputTokens *int64         `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int64         `json:"cache_read_input_tokens"`
	CacheCreation            *cacheCreation `json:"cache_creation"`
	Cost                     *float64       `json:"cost"`
}

type nested struct {
	Model string    `json:"model"`
	Usage *rawUsage `json:"usage"`
}

// envelope covers every shape we care about:
//   - OpenAI chat/embeddings (body or chunk): {model, usage}
//   - OpenAI responses: {model, usage} (body) or {type, response:{model, usage}} (stream)
//   - Anthropic: {model, usage} (body), {type:message_start, message:{model, usage}},
//     {type:message_delta, usage}
type envelope struct {
	Model    string    `json:"model"`
	Usage    *rawUsage `json:"usage"`
	Message  *nested   `json:"message"`
	Response *nested   `json:"response"`
}

// Accumulator merges usage fragments; later non-nil values win. This matches
// Anthropic (message_delta carries the final cumulative output_tokens) and
// OpenAI (only the final chunk carries usage).
type Accumulator struct {
	format string
	model  string
	raw    rawUsage
	seen   bool
}

// NewAccumulator returns an accumulator for the given API format.
func NewAccumulator(format string) *Accumulator { return &Accumulator{format: format} }

func (a *Accumulator) addModel(m string) {
	if m != "" && a.model == "" {
		a.model = m
	}
}

func (a *Accumulator) merge(u *rawUsage) {
	if u == nil {
		return
	}
	a.seen = true
	r := &a.raw
	set := func(dst **int64, src *int64) {
		if src != nil {
			*dst = src
		}
	}
	set(&r.PromptTokens, u.PromptTokens)
	set(&r.CompletionTokens, u.CompletionTokens)
	set(&r.InputTokens, u.InputTokens)
	set(&r.OutputTokens, u.OutputTokens)
	set(&r.CacheCreationInputTokens, u.CacheCreationInputTokens)
	set(&r.CacheReadInputTokens, u.CacheReadInputTokens)
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens != nil {
		r.PromptTokensDetails = u.PromptTokensDetails
	}
	if u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens != nil {
		r.InputTokensDetails = u.InputTokensDetails
	}
	if u.CacheCreation != nil {
		r.CacheCreation = u.CacheCreation
	}
	if u.Cost != nil {
		r.Cost = u.Cost
	}
}

func (a *Accumulator) add(e *envelope) {
	a.addModel(e.Model)
	a.merge(e.Usage)
	if e.Message != nil {
		a.addModel(e.Message.Model)
		a.merge(e.Message.Usage)
	}
	if e.Response != nil {
		a.addModel(e.Response.Model)
		a.merge(e.Response.Usage)
	}
}

// AddJSON feeds one JSON document (a full body or one SSE data payload).
func (a *Accumulator) AddJSON(b []byte) {
	var e envelope
	if json.Unmarshal(b, &e) != nil {
		return
	}
	a.add(&e)
}

func v(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// Result returns the normalized usage.
func (a *Accumulator) Result() Usage {
	u := Usage{Model: a.model}
	if !a.seen {
		return u
	}
	r := a.raw
	switch {
	case r.PromptTokens != nil: // OpenAI chat completions / embeddings
		cached := int64(0)
		if r.PromptTokensDetails != nil {
			cached = v(r.PromptTokensDetails.CachedTokens)
		}
		u.Input = max(v(r.PromptTokens)-cached, 0)
		u.CacheRead = cached
		u.Output = v(r.CompletionTokens)
		u.Known = true
	case r.InputTokens != nil || r.OutputTokens != nil:
		if a.format == FormatAnthropic {
			u.Input = v(r.InputTokens)
			u.CacheRead = v(r.CacheReadInputTokens)
			u.CacheWrite = v(r.CacheCreationInputTokens)
			if r.CacheCreation != nil {
				u.CacheWrite1h = v(r.CacheCreation.Ephemeral1h)
			}
		} else { // OpenAI responses API: input_tokens includes cached tokens
			cached := int64(0)
			if r.InputTokensDetails != nil {
				cached = v(r.InputTokensDetails.CachedTokens)
			}
			u.Input = max(v(r.InputTokens)-cached, 0)
			u.CacheRead = cached
		}
		u.Output = v(r.OutputTokens)
		u.Known = true
	}
	u.Cost = r.Cost
	return u
}

// ParseBody extracts usage from a complete (non-streamed) JSON response.
func ParseBody(format string, body []byte) Usage {
	a := NewAccumulator(format)
	a.AddJSON(body)
	return a.Result()
}

// maxLine bounds memory for a single SSE line. Larger lines are skipped.
const maxLine = 32 << 20

// StreamParser is an io.Writer that consumes raw SSE bytes in arbitrary
// chunks and extracts usage incrementally with O(1) state besides the
// current line.
type StreamParser struct {
	acc      *Accumulator
	line     []byte
	overflow bool
}

// NewStreamParser returns a parser for the given API format.
func NewStreamParser(format string) *StreamParser {
	return &StreamParser{acc: NewAccumulator(format)}
}

var (
	dataPrefix = []byte("data:")
	usageKey   = []byte(`"usage"`)
	doneMarker = []byte("[DONE]")
)

// Write implements io.Writer. It never returns an error.
func (p *StreamParser) Write(b []byte) (int, error) {
	n := len(b)
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			p.appendLine(b)
			break
		}
		p.appendLine(b[:i])
		p.handleLine()
		b = b[i+1:]
	}
	return n, nil
}

func (p *StreamParser) appendLine(b []byte) {
	if p.overflow {
		return
	}
	if len(p.line)+len(b) > maxLine {
		p.overflow = true
		p.line = p.line[:0]
		return
	}
	p.line = append(p.line, b...)
}

func (p *StreamParser) handleLine() {
	line := bytes.TrimSuffix(p.line, []byte{'\r'})
	overflow := p.overflow
	p.line = p.line[:0]
	p.overflow = false
	if overflow || !bytes.HasPrefix(line, dataPrefix) {
		return
	}
	payload := bytes.TrimSpace(line[len(dataPrefix):])
	if len(payload) == 0 || bytes.Equal(payload, doneMarker) {
		return
	}
	// Only decode events that can change the result: the first one (model)
	// and any that mention usage.
	if p.acc.model != "" && !bytes.Contains(payload, usageKey) {
		return
	}
	p.acc.AddJSON(payload)
}

// Result returns the usage seen so far. Call after the stream ended.
func (p *StreamParser) Result() Usage {
	if len(p.line) > 0 { // stream ended without trailing newline
		p.handleLine()
	}
	return p.acc.Result()
}
