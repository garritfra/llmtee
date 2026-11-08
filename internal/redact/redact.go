// Package redact replaces the values of configured JSON fields before
// bodies are stored.
package redact

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Placeholder replaces redacted values.
const Placeholder = "[REDACTED]"

// Redactor removes values of named fields (case-insensitive, any depth).
type Redactor struct {
	fields map[string]bool
	needle [][]byte
}

// New returns a redactor, or nil when no fields are configured.
func New(fields []string) *Redactor {
	if len(fields) == 0 {
		return nil
	}
	r := &Redactor{fields: map[string]bool{}}
	for _, f := range fields {
		f = strings.ToLower(strings.TrimSpace(f))
		if f == "" {
			continue
		}
		r.fields[f] = true
		r.needle = append(r.needle, []byte(f))
	}
	return r
}

func (r *Redactor) mentions(b []byte) bool {
	lb := bytes.ToLower(b)
	for _, n := range r.needle {
		if bytes.Contains(lb, n) {
			return true
		}
	}
	return false
}

// JSON redacts a JSON document. Non-JSON input is returned unchanged.
func (r *Redactor) JSON(b []byte) []byte {
	if r == nil || len(b) == 0 || !r.mentions(b) {
		return b
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return b
	}
	if !r.walk(v) {
		return b
	}
	out, err := json.Marshal(v)
	if err != nil {
		return b
	}
	return out
}

func (r *Redactor) walk(v any) bool {
	changed := false
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if r.fields[strings.ToLower(k)] {
				t[k] = Placeholder
				changed = true
				continue
			}
			if r.walk(val) {
				changed = true
			}
		}
	case []any:
		for _, val := range t {
			if r.walk(val) {
				changed = true
			}
		}
	}
	return changed
}

// SSE redacts each "data:" line of an event stream.
func (r *Redactor) SSE(b []byte) []byte {
	if r == nil || !r.mentions(b) {
		return b
	}
	lines := bytes.Split(b, []byte("\n"))
	for i, l := range lines {
		if p, ok := bytes.CutPrefix(l, []byte("data:")); ok {
			p = bytes.TrimSpace(p)
			if len(p) > 0 && p[0] == '{' {
				lines[i] = append([]byte("data: "), r.JSON(p)...)
			}
		}
	}
	return bytes.Join(lines, []byte("\n"))
}
