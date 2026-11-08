package usage

import "testing"

func TestParseBody(t *testing.T) {
	cases := []struct {
		name, format, body string
		want               Usage
	}{
		{"openai chat cached", FormatOpenAI, `{"model":"gpt-4o","usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":64}}}`,
			Usage{Known: true, Model: "gpt-4o", Input: 36, CacheRead: 64, Output: 7}},
		{"openai embeddings", FormatOpenAI, `{"model":"e","usage":{"prompt_tokens":8,"total_tokens":8}}`,
			Usage{Known: true, Model: "e", Input: 8}},
		{"openai responses", FormatOpenAI, `{"model":"gpt-4.1","usage":{"input_tokens":50,"input_tokens_details":{"cached_tokens":10},"output_tokens":5}}`,
			Usage{Known: true, Model: "gpt-4.1", Input: 40, CacheRead: 10, Output: 5}},
		{"anthropic 1h cache", FormatAnthropic, `{"model":"claude","usage":{"input_tokens":3,"cache_creation_input_tokens":100,"cache_read_input_tokens":50,"cache_creation":{"ephemeral_5m_input_tokens":40,"ephemeral_1h_input_tokens":60},"output_tokens":9}}`,
			Usage{Known: true, Model: "claude", Input: 3, CacheWrite: 100, CacheWrite1h: 60, CacheRead: 50, Output: 9}},
		{"openrouter cost", FormatOpenAI, `{"model":"x/y","usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0.0042}}`,
			Usage{Known: true, Model: "x/y", Input: 10, Output: 2}},
		{"no usage", FormatOpenAI, `{"model":"m","choices":[]}`, Usage{Model: "m"}},
		{"error body", FormatOpenAI, `{"error":{"message":"bad"}}`, Usage{}},
		{"not json", FormatOpenAI, `<html>`, Usage{}},
	}
	for _, tc := range cases {
		got := ParseBody(tc.format, []byte(tc.body))
		cost := got.Cost
		got.Cost = nil
		if got != tc.want {
			t.Errorf("%s: got %+v want %+v", tc.name, got, tc.want)
		}
		if tc.name == "openrouter cost" && (cost == nil || *cost != 0.0042) {
			t.Errorf("provider cost not parsed: %v", cost)
		}
	}
}

func TestStreamParserSplitsAndCRLF(t *testing.T) {
	stream := "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"c\",\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\r\n\r\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"usage\"}}\r\n\r\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}" // no trailing newline
	for size := 1; size <= len(stream); size += 7 {
		p := NewStreamParser(FormatAnthropic)
		for i := 0; i < len(stream); i += size {
			p.Write([]byte(stream[i:min(i+size, len(stream))]))
		}
		want := Usage{Known: true, Model: "c", Input: 5, Output: 42}
		if got := p.Result(); got != want {
			t.Fatalf("chunk %d: got %+v", size, got)
		}
	}
}

func TestStreamParserNoUsage(t *testing.T) {
	p := NewStreamParser(FormatOpenAI)
	p.Write([]byte("data: {\"model\":\"m\",\"choices\":[]}\n\ndata: [DONE]\n\n"))
	if got := p.Result(); got.Known || got.Model != "m" {
		t.Fatalf("got %+v", got)
	}
}

func BenchmarkStreamParser(b *testing.B) {
	chunk := []byte(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}],"usage":null}` + "\n\n")
	p := NewStreamParser(FormatOpenAI)
	b.SetBytes(int64(len(chunk)))
	for i := 0; i < b.N; i++ {
		p.Write(chunk)
	}
}
