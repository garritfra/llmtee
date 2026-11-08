// Package fakeupstream is a fake OpenAI/Anthropic API that replays recorded
// responses. It backs the golden tests, SDK integration tests and load tests.
package fakeupstream

import (
	"embed"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed fixtures/*
var fixtures embed.FS

// Fixture returns the raw bytes of a fixture file.
func Fixture(name string) []byte {
	b, err := fixtures.ReadFile("fixtures/" + name)
	if err != nil {
		panic(err)
	}
	return b
}

// Recorded is a request the fake upstream received.
type Recorded struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// Server replays fixtures.
type Server struct {
	// ChunkDelay is slept between stream chunks (simulates token latency).
	ChunkDelay time.Duration
	// RandomChunks splits streams at random byte offsets instead of events.
	RandomChunks bool

	mu   sync.Mutex
	reqs []Recorded
}

// Requests returns all recorded requests.
func (s *Server) Requests() []Recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Recorded(nil), s.reqs...)
}

// Last returns the last recorded request.
func (s *Server) Last() Recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reqs) == 0 {
		return Recorded{}
	}
	return s.reqs[len(s.reqs)-1]
}

// FixtureFor picks the fixture for a path and stream flag.
func FixtureFor(path string, stream bool) (name string, ok bool) {
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		name = "openai_chat"
	case strings.HasSuffix(path, "/responses"):
		name = "openai_responses"
	case strings.HasSuffix(path, "/embeddings"):
		return "openai_embeddings.json", true
	case strings.HasSuffix(path, "/messages"):
		name = "anthropic"
	default:
		return "", false
	}
	if stream {
		return name + "_stream.sse", true
	}
	return name + ".json", true
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.reqs = append(s.reqs, Recorded{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	if len(s.reqs) > 1000 {
		s.reqs = s.reqs[len(s.reqs)-1000:]
	}
	s.mu.Unlock()

	if r.URL.Path == "/v1/models" || strings.HasSuffix(r.URL.Path, "/models") {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","data":[{"id":"gpt-4o-mini","object":"model","created":0,"owned_by":"openai"}]}`)
		return
	}
	if code := r.Header.Get("X-Fake-Status"); code == "500" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		io.WriteString(w, `{"error":{"message":"The server had an error while processing your request.","type":"server_error"}}`)
		return
	}
	var meta struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &meta)
	name, ok := FixtureFor(r.URL.Path, meta.Stream)
	if fx := r.Header.Get("X-Fixture"); fx != "" {
		name, ok = fx, true
	}
	if !ok {
		http.Error(w, `{"error":{"message":"not found"}}`, http.StatusNotFound)
		return
	}
	data := Fixture(name)
	if !strings.HasSuffix(name, ".sse") {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req_fake")
		w.Write(data)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Request-Id", "req_fake")
	w.WriteHeader(200)
	fl := w.(http.Flusher)
	for _, chunk := range s.split(data) {
		if _, err := w.Write(chunk); err != nil {
			return
		}
		fl.Flush()
		if s.ChunkDelay > 0 {
			time.Sleep(s.ChunkDelay)
		}
	}
}

func (s *Server) split(data []byte) [][]byte {
	var out [][]byte
	if s.RandomChunks {
		for len(data) > 0 {
			n := min(1+rand.IntN(97), len(data))
			out = append(out, data[:n])
			data = data[n:]
		}
		return out
	}
	for _, ev := range strings.SplitAfter(string(data), "\n\n") {
		if ev != "" {
			out = append(out, []byte(ev))
		}
	}
	return out
}
