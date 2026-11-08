package redact

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	r := New([]string{"api_key", "Email"})
	out := string(r.JSON([]byte(`{"messages":[{"content":"hi","email":"a@b.c"}],"api_key":"sk","n":1.50}`)))
	if strings.Contains(out, "a@b.c") || strings.Contains(out, `"sk"`) || !strings.Contains(out, "1.50") {
		t.Fatalf("redacted = %s", out)
	}
	if got := string(r.JSON([]byte(`{"x":1}`))); got != `{"x":1}` {
		t.Errorf("untouched body changed: %s", got)
	}
	sse := string(r.SSE([]byte("event: x\ndata: {\"email\":\"a@b.c\"}\n\ndata: [DONE]\n\n")))
	if strings.Contains(sse, "a@b.c") || !strings.Contains(sse, "[DONE]") || !strings.HasPrefix(sse, "event: x\n") {
		t.Errorf("sse = %q", sse)
	}
	var nilR *Redactor
	if string(nilR.JSON([]byte("x"))) != "x" || New(nil) != nil {
		t.Error("nil redactor must be a no-op")
	}
}
