// Package mcp is a minimal Model Context Protocol server (streamable HTTP
// transport, JSON responses) that lets agents query their own spend.
//
// Tools: get_spend, list_requests, get_request.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/garritfra/llmtee/internal/budget"
	"github.com/garritfra/llmtee/internal/keys"
	"github.com/garritfra/llmtee/internal/store"
)

const latestProtocol = "2025-06-18"

var supported = map[string]bool{"2024-11-05": true, "2025-03-26": true, "2025-06-18": true, "2025-11-25": true}

// Server handles POST /mcp.
type Server struct {
	Store      store.Store
	Keys       *keys.Cache
	Budgets    *budget.Tracker
	Authorized func(*http.Request) bool
	Version    string
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.Authorized != nil && !s.Authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="llmtee"`)
		http.Error(w, "unauthorized: send the admin password as Authorization: Bearer <password>", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodPost:
	case http.MethodGet, http.MethodDelete:
		// No server-initiated stream and no sessions.
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	var reqs []rpcRequest
	batch := len(body) > 0 && body[0] == '['
	if batch {
		err = json.Unmarshal(body, &reqs)
	} else {
		var one rpcRequest
		err = json.Unmarshal(body, &one)
		reqs = []rpcRequest{one}
	}
	if err != nil {
		writeJSON(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
		return
	}
	var out []rpcResponse
	for _, req := range reqs {
		if len(req.ID) == 0 { // notification
			continue
		}
		res, rerr := s.dispatch(r.Context(), req)
		resp := rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: res, Error: rerr}
		out = append(out, resp)
	}
	if len(out) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if batch {
		writeJSON(w, out)
	} else {
		writeJSON(w, out[0])
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) dispatch(ctx context.Context, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := latestProtocol
		if supported[p.ProtocolVersion] {
			v = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "llmtee", "version": s.Version},
			"instructions":    "Query LLM spend and request logs recorded by the llmtee proxy. Costs are in USD; days are UTC.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{-32602, "invalid params"}
		}
		if len(p.Arguments) == 0 {
			p.Arguments = json.RawMessage("{}")
		}
		res, err := s.call(ctx, p.Name, p.Arguments)
		if errors.Is(err, errUnknownTool) {
			return nil, &rpcError{-32602, err.Error()}
		}
		if err != nil {
			return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": err.Error()}}}, nil
		}
		b, _ := json.MarshalIndent(res, "", "  ")
		return map[string]any{
			"content":           []any{map[string]any{"type": "text", "text": string(b)}},
			"structuredContent": res,
		}, nil
	default:
		return nil, &rpcError{-32601, "method not found: " + req.Method}
	}
}

var errUnknownTool = errors.New("unknown tool")

type schema = map[string]any

func str(desc string) schema { return schema{"type": "string", "description": desc} }

var filterProps = schema{
	"range":    schema{"type": "string", "enum": []string{"today", "month", "24h", "7d", "30d", "90d", "all"}, "description": "Time window (UTC). Default 7d."},
	"model":    str("Only this model."),
	"key":      str("Only this key id (mk-... or pt-...)."),
	"user":     str("Only this user id (Llmtee-User-Id / Helicone-User-Id header)."),
	"session":  str("Only this session id."),
	"upstream": str("Only this upstream, e.g. openai, anthropic."),
}

func with(extra schema) schema {
	m := schema{}
	for k, v := range filterProps {
		m[k] = v
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

var tools = []any{
	schema{
		"name":        "get_spend",
		"title":       "Get LLM spend",
		"description": "Total LLM cost in USD, request and token counts for a time window, optionally grouped by day, model, key, user, upstream or session. Also returns current budget status of keys with budgets.",
		"inputSchema": schema{"type": "object", "properties": with(schema{
			"group_by": schema{"type": "string", "enum": []string{"none", "day", "model", "key", "user", "upstream", "session"}, "description": "Grouping. Default none."},
		})},
		"annotations": schema{"readOnlyHint": true},
	},
	schema{
		"name":        "list_requests",
		"title":       "List LLM requests",
		"description": "Most recent proxied requests with model, tokens, cost, latency and status. Newest first.",
		"inputSchema": schema{"type": "object", "properties": with(schema{
			"status": schema{"type": "string", "enum": []string{"ok", "error"}},
			"limit":  schema{"type": "integer", "minimum": 1, "maximum": 200, "description": "Default 20."},
			"before": str("Pagination cursor: only requests with an id smaller than this."),
		})},
		"annotations": schema{"readOnlyHint": true},
	},
	schema{
		"name":        "get_request",
		"title":       "Get one LLM request",
		"description": "Full details of one request by id, optionally including the stored request and response bodies.",
		"inputSchema": schema{"type": "object", "required": []string{"id"}, "properties": schema{
			"id":             str("Request id (ULID)."),
			"include_bodies": schema{"type": "boolean", "description": "Include bodies (truncated to 20000 characters each). Default false."},
		}},
		"annotations": schema{"readOnlyHint": true},
	},
}

type args struct {
	Range         string `json:"range"`
	Model         string `json:"model"`
	Key           string `json:"key"`
	User          string `json:"user"`
	Session       string `json:"session"`
	Upstream      string `json:"upstream"`
	Status        string `json:"status"`
	GroupBy       string `json:"group_by"`
	Limit         int    `json:"limit"`
	Before        string `json:"before"`
	ID            string `json:"id"`
	IncludeBodies bool   `json:"include_bodies"`
}

func (a args) filter(now time.Time) (store.Filter, error) {
	f := store.Filter{Model: a.Model, KeyID: a.Key, UserID: a.User, SessionID: a.Session, Upstream: a.Upstream,
		Status: a.Status, Before: a.Before}
	day, month := budget.Starts(now)
	switch a.Range {
	case "", "7d":
		f.Since = now.Add(-7 * 24 * time.Hour).UnixMilli()
	case "today":
		f.Since = day
	case "month":
		f.Since = month
	case "24h":
		f.Since = now.Add(-24 * time.Hour).UnixMilli()
	case "30d":
		f.Since = now.Add(-30 * 24 * time.Hour).UnixMilli()
	case "90d":
		f.Since = now.Add(-90 * 24 * time.Hour).UnixMilli()
	case "all":
	default:
		return f, fmt.Errorf("invalid range %q", a.Range)
	}
	return f, nil
}

type requestOut struct {
	ID               string            `json:"id"`
	Time             string            `json:"time"`
	Upstream         string            `json:"upstream"`
	Path             string            `json:"path,omitempty"`
	Model            string            `json:"model,omitempty"`
	KeyID            string            `json:"key_id,omitempty"`
	KeyName          string            `json:"key_name,omitempty"`
	UserID           string            `json:"user_id,omitempty"`
	SessionID        string            `json:"session_id,omitempty"`
	Status           int               `json:"status"`
	Streamed         bool              `json:"streamed"`
	TokensIn         *int64            `json:"tokens_in"`
	TokensOut        *int64            `json:"tokens_out"`
	TokensCacheRead  *int64            `json:"tokens_cache_read"`
	TokensCacheWrite *int64            `json:"tokens_cache_write"`
	CostUSD          *float64          `json:"cost_usd"`
	LatencyMS        int64             `json:"latency_ms"`
	TTFTMS           *int64            `json:"ttft_ms,omitempty"`
	Error            string            `json:"error,omitempty"`
	Properties       map[string]string `json:"properties,omitempty"`
	RequestBody      *string           `json:"request_body,omitempty"`
	ResponseBody     *string           `json:"response_body,omitempty"`
}

func (s *Server) out(r *store.Request) requestOut {
	return requestOut{ID: r.ID, Time: r.Time().UTC().Format(time.RFC3339Nano), Upstream: r.Upstream, Path: r.Path,
		Model: r.Model, KeyID: r.KeyID, KeyName: s.Keys.Name(r.KeyID), UserID: r.UserID, SessionID: r.SessionID,
		Status: r.Status, Streamed: r.Streamed, TokensIn: r.TokensIn, TokensOut: r.TokensOut,
		TokensCacheRead: r.TokensCacheRead, TokensCacheWrite: r.TokensCacheWrite, CostUSD: r.CostUSD,
		LatencyMS: r.LatencyMS, TTFTMS: r.TTFTMS, Error: r.Error, Properties: r.Properties}
}

func clip(b []byte) *string {
	s := string(b)
	if len(s) > 20000 {
		s = s[:20000] + "\n…[truncated]"
	}
	return &s
}

func (s *Server) call(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	if s.Store == nil {
		return nil, errors.New("database unavailable")
	}
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	now := time.Now()
	switch name {
	case "get_spend":
		f, err := a.filter(now)
		if err != nil {
			return nil, err
		}
		g := a.GroupBy
		if g == "none" {
			g = ""
		}
		rows, err := s.Store.Spend(ctx, f, g)
		if err != nil {
			return nil, err
		}
		type row struct {
			Group     string  `json:"group,omitempty"`
			Name      string  `json:"name,omitempty"`
			CostUSD   float64 `json:"cost_usd"`
			Requests  int64   `json:"requests"`
			TokensIn  int64   `json:"tokens_in"`
			TokensOut int64   `json:"tokens_out"`
			CacheRead int64   `json:"tokens_cache_read"`
			Unpriced  int64   `json:"requests_without_price"`
			Errors    int64   `json:"errors"`
		}
		out := []row{}
		for _, r := range rows {
			o := row{Group: r.Group, CostUSD: r.CostUSD, Requests: r.Requests, TokensIn: r.TokensIn, TokensOut: r.TokensOut,
				CacheRead: r.CacheRead, Unpriced: r.Unpriced, Errors: r.Errors}
			if g == store.ByKey {
				o.Name = s.Keys.Name(r.Group)
			}
			out = append(out, o)
		}
		type budgetOut struct {
			KeyID          string   `json:"key_id"`
			Name           string   `json:"name"`
			SpentTodayUSD  float64  `json:"spent_today_usd"`
			SpentMonthUSD  float64  `json:"spent_month_usd"`
			BudgetDayUSD   *float64 `json:"budget_day_usd,omitempty"`
			BudgetMonthUSD *float64 `json:"budget_month_usd,omitempty"`
		}
		budgets := []budgetOut{}
		if ks, err := s.Store.ListKeys(ctx); err == nil {
			for _, k := range ks {
				if k.BudgetDayUSD == nil && k.BudgetMonthUSD == nil {
					continue
				}
				sp := s.Budgets.Spend(k.ID)
				budgets = append(budgets, budgetOut{k.ID, k.Name, sp.Day, sp.Month, k.BudgetDayUSD, k.BudgetMonthUSD})
			}
		}
		return map[string]any{"range": rangeName(a.Range), "group_by": a.GroupBy, "rows": out, "budgets": budgets}, nil

	case "list_requests":
		f, err := a.filter(now)
		if err != nil {
			return nil, err
		}
		f.Limit = a.Limit
		if f.Limit <= 0 {
			f.Limit = 20
		}
		f.Limit = min(f.Limit, 200)
		reqs, err := s.Store.ListRequests(ctx, f)
		if err != nil {
			return nil, err
		}
		out := []requestOut{}
		for _, r := range reqs {
			out = append(out, s.out(r))
		}
		res := map[string]any{"requests": out}
		if len(reqs) == f.Limit {
			res["next_before"] = reqs[len(reqs)-1].ID
		}
		return res, nil

	case "get_request":
		if a.ID == "" {
			return nil, errors.New("id is required")
		}
		r, err := s.Store.GetRequest(ctx, a.ID, a.IncludeBodies)
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("request %s not found", a.ID)
		}
		if err != nil {
			return nil, err
		}
		o := s.out(r)
		if a.IncludeBodies && r.HasBody {
			o.RequestBody, o.ResponseBody = clip(r.ReqBody), clip(r.RespBody)
		}
		return map[string]any{"request": o, "has_bodies": r.HasBody, "ui_path": "/requests/" + url.PathEscape(r.ID)}, nil
	}
	return nil, fmt.Errorf("%w: %s", errUnknownTool, name)
}

func rangeName(r string) string {
	if r == "" {
		return "7d"
	}
	return r
}
