// Runs the official OpenAI and Anthropic TypeScript SDKs against llmtee.
import assert from "node:assert/strict";
import OpenAI from "openai";
import Anthropic from "@anthropic-ai/sdk";

const BASE = process.env.LLMTEE_URL ?? "http://127.0.0.1:8080";
const ADMIN = process.env.LLMTEE_ADMIN_PASSWORD ?? "integration";

async function lastRequest(session) {
  for (let i = 0; i < 50; i++) {
    const r = await fetch(`${BASE}/mcp`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: `Bearer ${ADMIN}` },
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/call",
        params: { name: "list_requests", arguments: { session, range: "all", limit: 1 } } }),
    });
    const reqs = (await r.json()).result.structuredContent.requests;
    if (reqs.length) return reqs[0];
    await new Promise((res) => setTimeout(res, 100));
  }
  throw new Error(`request not logged: ${session}`);
}

const tests = {
  async openaiStream() {
    const c = new OpenAI({ baseURL: `${BASE}/openai/v1`, apiKey: "sk-ts", defaultHeaders: { "Helicone-Session-Id": "ts-chat" } });
    let text = "";
    const stream = await c.chat.completions.create({ model: "gpt-4o-mini", stream: true, messages: [{ role: "user", content: "hi" }] });
    for await (const chunk of stream) text += chunk.choices[0]?.delta?.content ?? "";
    assert.equal(text, "Hello! How can I help you today?");
    const r = await lastRequest("ts-chat");
    assert.equal(r.tokens_in, 219);
    assert.equal(r.tokens_cache_read, 1024);
  },
  async openaiResponses() {
    const c = new OpenAI({ baseURL: `${BASE}/openai/v1`, apiKey: "sk-ts", defaultHeaders: { "Helicone-Session-Id": "ts-resp" } });
    const res = await c.responses.create({ model: "gpt-4.1", input: "hi" });
    assert.equal(res.usage.input_tokens, 36);
    assert.equal((await lastRequest("ts-resp")).tokens_out, 7);
  },
  async anthropicStream() {
    const c = new Anthropic({ baseURL: `${BASE}/anthropic`, apiKey: "sk-ant-ts", defaultHeaders: { "Helicone-Session-Id": "ts-ant" } });
    const s = c.messages.stream({ model: "claude-sonnet-4-5", max_tokens: 100, messages: [{ role: "user", content: "hi" }] });
    const msg = await s.finalMessage();
    assert.equal(msg.usage.output_tokens, 21);
    const r = await lastRequest("ts-ant");
    assert.equal(r.tokens_cache_write, 1500);
    assert.ok(r.cost_usd > 0);
  },
};

for (const [name, fn] of Object.entries(tests)) {
  await fn();
  console.log("ok", name);
}
