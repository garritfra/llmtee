"""Runs the official OpenAI and Anthropic Python SDKs against llmtee.

Expects llmtee on $LLMTEE_URL (default http://127.0.0.1:8080) forwarding
to the fake upstream (cmd/fakeupstream), see integration/run.sh.
"""
import json
import os
import time
import urllib.request

import anthropic
import openai

BASE = os.environ.get("LLMTEE_URL", "http://127.0.0.1:8080")
ADMIN = os.environ.get("LLMTEE_ADMIN_PASSWORD", "integration")


def mcp(name, args):
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/call",
                       "params": {"name": name, "arguments": args}}).encode()
    req = urllib.request.Request(BASE + "/mcp", data=body, headers={
        "Content-Type": "application/json", "Authorization": "Bearer " + ADMIN})
    with urllib.request.urlopen(req) as r:
        return json.load(r)["result"]["structuredContent"]


def last_request(session):
    for _ in range(50):
        reqs = mcp("list_requests", {"session": session, "range": "all", "limit": 1})["requests"]
        if reqs:
            return reqs[0]
        time.sleep(0.1)
    raise AssertionError("request not logged: " + session)


def test_openai_chat_stream():
    c = openai.OpenAI(base_url=BASE + "/openai/v1", api_key="sk-py",
                      default_headers={"Helicone-Session-Id": "py-chat-stream"})
    text = ""
    usage = None
    for chunk in c.chat.completions.create(model="gpt-4o-mini", messages=[{"role": "user", "content": "hi"}], stream=True):
        if chunk.choices and chunk.choices[0].delta.content:
            text += chunk.choices[0].delta.content
        if chunk.usage:
            usage = chunk.usage
    assert text == "Hello! How can I help you today?", text
    assert usage is not None and usage.prompt_tokens == 1243
    r = last_request("py-chat-stream")
    assert r["tokens_in"] == 219 and r["tokens_cache_read"] == 1024 and r["tokens_out"] == 9, r
    assert r["cost_usd"] > 0 and r["streamed"]


def test_openai_chat_and_embeddings():
    c = openai.OpenAI(base_url=BASE + "/openai/v1", api_key="sk-py",
                      default_headers={"Helicone-Session-Id": "py-chat"})
    resp = c.chat.completions.create(model="gpt-4o-mini", messages=[{"role": "user", "content": "hi"}])
    assert resp.usage.total_tokens == 28
    assert last_request("py-chat")["tokens_out"] == 9
    emb = openai.OpenAI(base_url=BASE + "/openai/v1", api_key="sk-py",
                        default_headers={"Helicone-Session-Id": "py-emb"}).embeddings.create(
        model="text-embedding-3-small", input="hi", encoding_format="float")
    assert len(emb.data[0].embedding) == 5
    assert last_request("py-emb")["tokens_in"] == 8


def test_openai_responses_stream():
    c = openai.OpenAI(base_url=BASE + "/openai/v1", api_key="sk-py",
                      default_headers={"Helicone-Session-Id": "py-responses"})
    with c.responses.stream(model="gpt-4.1", input="hi") as s:
        for _ in s:
            pass
        final = s.get_final_response()
    assert final.output_text == "Hi! Nice to meet you."
    r = last_request("py-responses")
    assert r["tokens_in"] == 52 and r["tokens_cache_read"] == 2048, r


def test_anthropic_stream_and_plain():
    c = anthropic.Anthropic(base_url=BASE + "/anthropic", api_key="sk-ant-py",
                            default_headers={"Helicone-Session-Id": "py-anthropic"})
    with c.messages.stream(model="claude-sonnet-4-6", max_tokens=100,
                           messages=[{"role": "user", "content": "hi"}]) as s:
        text = "".join(s.text_stream)
        final = s.get_final_message()
    assert text.startswith("Hello! I'm Claude"), text
    assert final.usage.output_tokens == 21
    r = last_request("py-anthropic")
    assert r["tokens_cache_read"] == 20480 and r["tokens_cache_write"] == 1500, r

    msg = anthropic.Anthropic(base_url=BASE + "/anthropic", api_key="sk-ant-py").messages.create(
        model="claude-sonnet-4-6", max_tokens=10, messages=[{"role": "user", "content": "hi"}])
    assert msg.usage.input_tokens == 25


if __name__ == "__main__":
    for name, fn in list(globals().items()):
        if name.startswith("test_"):
            fn()
            print("ok", name)
