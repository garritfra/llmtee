// Package proxy is the synchronous request path: route, authenticate,
// enforce budgets and forward to the upstream while teeing the response
// stream to a usage parser. Everything expensive happens later in the
// pipeline.
package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/garritfra/llmtee/internal/budget"
	"github.com/garritfra/llmtee/internal/config"
	"github.com/garritfra/llmtee/internal/keys"
	"github.com/garritfra/llmtee/internal/metrics"
	"github.com/garritfra/llmtee/internal/pipeline"
	"github.com/garritfra/llmtee/internal/store"
	"github.com/garritfra/llmtee/internal/usage"
)

// RequestIDHeader is set on every proxied response.
const RequestIDHeader = "Llmtee-Request-Id"

const (
	maxRequestBody = 128 << 20 // larger request bodies are rejected with 413
	maxParseBody   = 64 << 20  // non-streamed responses larger than this are not parsed
)

// Submitter receives finished events (the pipeline).
type Submitter interface{ Submit(*pipeline.Event) }

// Proxy forwards LLM API requests.
type Proxy struct {
	cfg       *config.Config
	upstreams map[string]*config.Upstream
	keys      *keys.Cache
	budgets   *budget.Tracker
	sink      Submitter
	metrics   *metrics.M
	client    *http.Client
	now       func() time.Time
}

// New creates a proxy.
func New(cfg *config.Config, kc *keys.Cache, b *budget.Tracker, sink Submitter, m *metrics.M) *Proxy {
	ups := map[string]*config.Upstream{}
	for i := range cfg.Upstreams {
		ups[cfg.Upstreams[i].Name] = &cfg.Upstreams[i]
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   128,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &Proxy{
		cfg: cfg, upstreams: ups, keys: kc, budgets: b, sink: sink, metrics: m, now: time.Now,
		client: &http.Client{
			Transport:     tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Prefixes returns the URL path prefixes the proxy serves.
func Prefixes() []string { return []string{"/openai/", "/anthropic/", "/openrouter/", "/u/"} }

// route maps a request path to an upstream and the remaining path.
func (p *Proxy) route(path string) (*config.Upstream, string, bool) {
	var name, rest string
	if after, ok := strings.CutPrefix(path, "/u/"); ok {
		name, rest, _ = strings.Cut(after, "/")
		rest = "/" + rest
	} else {
		trimmed := strings.TrimPrefix(path, "/")
		name, rest, _ = strings.Cut(trimmed, "/")
		rest = "/" + rest
		if name != "openai" && name != "anthropic" && name != "openrouter" {
			return nil, "", false
		}
	}
	up, ok := p.upstreams[name]
	return up, rest, ok
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 32*1024); return &b }}

// capture is a bounded buffer for the response copy.
type capture struct {
	buf      []byte
	limit    int
	overflow bool
}

func (c *capture) Write(b []byte) {
	if c.overflow {
		return
	}
	if len(c.buf)+len(b) > c.limit {
		c.buf = append(c.buf, b[:c.limit-len(c.buf)]...)
		c.overflow = true
		return
	}
	c.buf = append(c.buf, b...)
}

type reqMeta struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	User     string `json:"user"`
	Metadata *struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
	StreamOptions *struct {
		IncludeUsage *bool `json:"include_usage"`
	} `json:"stream_options"`
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := p.now()
	up, rest, ok := p.route(r.URL.Path)
	if !ok {
		writeError(w, config.TypeOpenAI, http.StatusNotFound, "not_found",
			"llmtee: unknown upstream; use /openai/v1/..., /anthropic/v1/..., /openrouter/v1/... or /u/<name>/v1/...")
		return
	}
	id := ulid.Make().String()
	w.Header().Set(RequestIDHeader, id)
	format := usage.FormatOpenAI
	if up.Type == config.TypeAnthropic {
		format = usage.FormatAnthropic
	}

	auth, aerr := p.authenticate(r, up)
	if aerr != nil {
		writeError(w, up.Type, aerr.status, aerr.kind, aerr.msg)
		return
	}

	logged := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions &&
		!strings.HasSuffix(rest, "/count_tokens")
	ev := &pipeline.Event{
		ID: id, Start: start, Upstream: up.Name, Format: format, Path: rest,
		Key: auth.key, KeyID: auth.keyID,
		LogBodies: p.cfg.LogBodies,
	}
	if up.LogBodies != nil {
		ev.LogBodies = *up.LogBodies
	}
	p.readMetaHeaders(r, ev)

	// Budget check: in-memory only.
	if err := p.budgets.Check(auth.key); err != nil {
		var ex *budget.ExceededError
		window := "daily"
		if errors.As(err, &ex) {
			window = ex.Window
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter(window, p.now())))
		}
		if p.metrics != nil {
			p.metrics.BudgetRejections.WithLabelValues(auth.keyID, window).Inc()
		}
		writeError(w, up.Type, http.StatusTooManyRequests, "budget_exceeded", err.Error())
		ev.Status, ev.Error = http.StatusTooManyRequests, err.Error()
		ev.LatencyMS = p.now().Sub(start).Milliseconds()
		ev.LogBodies = false
		if logged {
			p.sink.Submit(ev)
		}
		return
	}

	// Read and inspect the request body.
	var body []byte
	if r.Body != nil && r.Body != http.NoBody {
		var err error
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				writeError(w, up.Type, http.StatusRequestEntityTooLarge, "request_too_large", "llmtee: request body too large")
				return
			}
			writeError(w, up.Type, http.StatusBadRequest, "invalid_request", "llmtee: reading request body: "+err.Error())
			return
		}
	}
	var meta reqMeta
	if len(body) > 0 && looksJSON(body) {
		_ = json.Unmarshal(body, &meta)
		ev.ReqModel = meta.Model
		if ev.UserID == "" {
			ev.UserID = meta.User
			if ev.UserID == "" && meta.Metadata != nil {
				ev.UserID = meta.Metadata.UserID
			}
		}
		if p.cfg.InjectStreamUsage && up.Type == config.TypeOpenAI && meta.Stream &&
			strings.HasSuffix(rest, "/chat/completions") &&
			(meta.StreamOptions == nil || meta.StreamOptions.IncludeUsage == nil || !*meta.StreamOptions.IncludeUsage) {
			if nb, err := injectIncludeUsage(body); err == nil {
				body = nb
			}
		}
	}
	if ev.LogBodies {
		ev.Req = body
	}

	// Build the upstream request.
	target := up.BaseURL + rest
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		writeError(w, up.Type, http.StatusBadGateway, "upstream_error", "llmtee: "+err.Error())
		return
	}
	copyRequestHeaders(out.Header, r.Header)
	out.ContentLength = int64(len(body))
	if len(body) == 0 {
		out.Body = http.NoBody
	}
	if auth.inject {
		out.Header.Del("Authorization")
		out.Header.Del("X-Api-Key")
		if up.Type == config.TypeAnthropic {
			out.Header.Set("X-Api-Key", up.APIKey)
		} else {
			out.Header.Set("Authorization", "Bearer "+up.APIKey)
		}
	}

	resp, err := p.client.Do(out)
	if err != nil {
		status, msg := http.StatusBadGateway, "llmtee: upstream request failed: "+err.Error()
		if r.Context().Err() != nil {
			status, msg = 499, "client closed request"
		} else {
			writeError(w, up.Type, status, "upstream_error", msg)
		}
		ev.Status, ev.Error = status, msg
		ev.LatencyMS = p.now().Sub(start).Milliseconds()
		if logged {
			p.sink.Submit(ev)
		}
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	ev.Status = resp.StatusCode
	ev.Streamed = strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")

	// Forward with tee: every chunk goes to the client immediately and a
	// copy goes to the parser / capture buffer.
	var parser *usage.StreamParser
	capt := &capture{limit: maxParseBody}
	if !logged {
		capt.limit = 0
	}
	if ev.Streamed {
		parser = usage.NewStreamParser(format)
		capt.limit = 0
		if ev.LogBodies {
			capt.limit = int(p.cfg.MaxBodyBytes)
		}
	}
	rc := http.NewResponseController(w)
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	first := true
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if first {
				first = false
				if ev.Streamed {
					t := p.now().Sub(start).Milliseconds()
					ev.TTFTMS = &t
				}
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				ev.Error = "client disconnected: " + werr.Error()
				break
			}
			if ev.Streamed {
				if ferr := rc.Flush(); ferr != nil {
					ev.Error = "client disconnected: " + ferr.Error()
					break
				}
				parser.Write(buf[:n])
			}
			capt.Write(buf[:n])
		}
		if rerr != nil {
			if rerr != io.EOF {
				if r.Context().Err() != nil {
					ev.Error = "client disconnected"
				} else {
					ev.Error = "upstream stream error: " + rerr.Error()
				}
			}
			break
		}
	}
	ev.LatencyMS = p.now().Sub(start).Milliseconds()
	if parser != nil {
		u := parser.Result()
		ev.StreamUsage = &u
	}
	ev.Resp = capt.buf
	ev.RespComplete = !capt.overflow
	if logged {
		p.sink.Submit(ev)
	}
}

func looksJSON(b []byte) bool {
	b = bytes.TrimLeft(b, " \t\r\n")
	return len(b) > 0 && b[0] == '{'
}

// injectIncludeUsage sets stream_options.include_usage=true so OpenAI-style
// streams end with a usage chunk.
func injectIncludeUsage(body []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	so := map[string]json.RawMessage{}
	if raw, ok := m["stream_options"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if err := json.Unmarshal(raw, &so); err != nil {
			return nil, err
		}
	}
	so["include_usage"] = json.RawMessage("true")
	b, err := json.Marshal(so)
	if err != nil {
		return nil, err
	}
	if _, ok := m["stream_options"]; !ok {
		// Insert after the opening brace to keep the rest byte-identical.
		i := bytes.IndexByte(body, '{')
		out := make([]byte, 0, len(body)+40)
		out = append(out, body[:i+1]...)
		out = append(out, `"stream_options":`...)
		out = append(out, b...)
		rest := bytes.TrimLeft(body[i+1:], " \t\r\n")
		if len(rest) > 0 && rest[0] != '}' {
			out = append(out, ',')
		}
		return append(out, body[i+1:]...), nil
	}
	m["stream_options"] = b
	return json.Marshal(m)
}

func retryAfter(window string, now time.Time) int {
	now = now.UTC()
	var next time.Time
	if window == "monthly" {
		next = time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	} else {
		next = time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	}
	return int(next.Sub(now).Seconds()) + 1
}

type authResult struct {
	key    *store.Key
	keyID  string
	inject bool
}

type httpError struct {
	status    int
	kind, msg string
}

func bearer(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return v
}

// authenticate resolves the key used for logging and budgets.
//
//   - Managed: the client sends a llmtee proxy key (Authorization, x-api-key,
//     Llmtee-Key or Helicone-Auth); llmtee injects the provider key.
//   - Passthrough: the client sends its provider key; llmtee logs a hash.
//     A proxy key in Llmtee-Key/Helicone-Auth can be combined with it.
func (p *Proxy) authenticate(r *http.Request, up *config.Upstream) (authResult, *httpError) {
	var res authResult
	proxyTok := bearer(r.Header.Get("Llmtee-Key"))
	if h := bearer(r.Header.Get("Helicone-Auth")); proxyTok == "" && keys.IsManaged(h) {
		proxyTok = h
	}
	provTok := bearer(r.Header.Get("Authorization"))
	if provTok == "" {
		provTok = strings.TrimSpace(r.Header.Get("X-Api-Key"))
	}
	if provTok == "" {
		provTok = strings.TrimSpace(r.Header.Get("Api-Key"))
	}
	if keys.IsManaged(provTok) {
		if proxyTok == "" {
			proxyTok = provTok
		}
		res.inject = true
	}
	switch {
	case proxyTok != "":
		k, ok := p.keys.ByHash(keys.Hash(proxyTok))
		if !ok || k.Kind != keys.Managed {
			return res, &httpError{http.StatusUnauthorized, "authentication_error", "llmtee: invalid proxy key"}
		}
		res.key, res.keyID = k, k.ID
	case p.cfg.RequireProxyKey:
		return res, &httpError{http.StatusUnauthorized, "authentication_error", "llmtee: a llmtee proxy key is required (Authorization: Bearer llmtee-...)"}
	case provTok != "":
		h := keys.Hash(provTok)
		res.keyID = keys.ID(keys.Passthrough, h)
		if k, ok := p.keys.ByHash(h); ok {
			res.key, res.keyID = k, k.ID
		}
	}
	if res.inject && up.APIKey == "" {
		return res, &httpError{http.StatusUnauthorized, "authentication_error",
			fmt.Sprintf("llmtee: no provider API key configured for upstream %q (managed keys need one, e.g. %s_API_KEY)", up.Name, strings.ToUpper(up.Name))}
	}
	return res, nil
}

// readMetaHeaders reads Llmtee-* and Helicone-* metadata headers.
func (p *Proxy) readMetaHeaders(r *http.Request, ev *pipeline.Event) {
	for name, vals := range r.Header {
		if len(vals) == 0 {
			continue
		}
		lname := strings.ToLower(name)
		var rest string
		switch {
		case strings.HasPrefix(lname, "llmtee-"):
			rest = lname[len("llmtee-"):]
		case strings.HasPrefix(lname, "helicone-"):
			rest = lname[len("helicone-"):]
		default:
			continue
		}
		v := vals[0]
		switch {
		case rest == "user-id":
			ev.UserID = v
		case rest == "session-id":
			ev.SessionID = v
		case rest == "session-name", rest == "session-path":
			setProp(ev, strings.ReplaceAll(rest, "-", "_"), v)
		case strings.HasPrefix(rest, "property-"):
			setProp(ev, rest[len("property-"):], v)
		case rest == "omit-request":
			ev.OmitReq = ev.OmitReq || isTrue(v)
		case rest == "omit-response":
			ev.OmitResp = ev.OmitResp || isTrue(v)
		case rest == "omit-body":
			if isTrue(v) {
				ev.OmitReq, ev.OmitResp = true, true
			}
		}
	}
}

func isTrue(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "true" || v == "1" || v == "yes"
}

func setProp(ev *pipeline.Event, k, v string) {
	if k == "" {
		return
	}
	if ev.Properties == nil {
		ev.Properties = map[string]string{}
	}
	ev.Properties[k] = v
}

var hopHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true, "Proxy-Connection": true,
}

func copyRequestHeaders(dst, src http.Header) {
	for k, vs := range src {
		ck := http.CanonicalHeaderKey(k)
		lk := strings.ToLower(ck)
		if hopHeaders[ck] || ck == "Host" || ck == "Content-Length" || ck == "Accept-Encoding" || ck == "Cookie" ||
			strings.HasPrefix(lk, "llmtee-") || strings.HasPrefix(lk, "helicone-") {
			continue
		}
		dst[ck] = append([]string(nil), vs...)
	}
	// Remove headers named in Connection.
	for _, f := range src.Values("Connection") {
		for _, h := range strings.Split(f, ",") {
			dst.Del(strings.TrimSpace(h))
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopHeaders[k] {
			continue
		}
		dst[k] = append([]string(nil), vs...)
	}
}

// writeError writes an error in the upstream's API format so SDKs surface
// a readable message.
func writeError(w http.ResponseWriter, apiType string, status int, kind, msg string) {
	var body any
	if apiType == config.TypeAnthropic {
		t := map[int]string{401: "authentication_error", 404: "not_found_error", 413: "request_too_large",
			429: "rate_limit_error", 400: "invalid_request_error"}[status]
		if t == "" {
			t = "api_error"
		}
		body = map[string]any{"type": "error", "error": map[string]any{"type": t, "message": msg}}
	} else {
		body = map[string]any{"error": map[string]any{"message": msg, "type": kind, "code": kind}}
	}
	b, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	if _, err := w.Write(b); err != nil {
		slog.Debug("writing error response", "err", err)
	}
}
