package gateway

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// /v1/chat/completions（A4-4 / #145）：出站**重建** + 响应必须解析
//
// 判据：docs/contract/outbound/behavior.md §一/§二 与
// fixtures/chat-completions-requests.json；期望值来自对 Python 基线的逐字节
// 对照（harness ok-openai / ok-openai-stream / chat-stream-from-json /
// chat-sse-on-nonstream）。复用 gateway_test.go 的 newFixture。
// ---------------------------------------------------------------------------

const chatReqBody = `{"model":"GLM-5.3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

const chatReqBodyStream = `{"model":"GLM-5.3","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`

const chatUpstreamOK = `{"id": "msg_harness", "type": "message", "role": "assistant", "model": "GLM-5.3", "content": [{"type": "text", "text": "hello"}], "stop_reason": "end_turn", "stop_sequence": null, "usage": {"input_tokens": 3, "output_tokens": 1}}`

var chatUpstreamSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_harness","type":"message","role":"assistant","model":"GLM-5.3","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":0}}}` +
	"\n\nevent: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}` +
	"\n\nevent: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}` +
	"\n\nevent: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

var (
	reCreated  = regexp.MustCompile(`"created":\d+`)  // 紧凑 JSON（无空格）
	reCreatedS = regexp.MustCompile(`"created": \d+`) // SSE 帧（Python 默认分隔符，有空格）
	reChatcmpl = regexp.MustCompile(`chatcmpl-[0-9a-f]+`)
)

// TestChatJSONConversion 非流式：出站重建 + 入站转 OpenAI 紧凑 JSON，且**无** cache-control。
func TestChatJSONConversion(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		want := `{"model": "GLM-5.3", "messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}], "max_tokens": 16}`
		if string(got) != want {
			t.Errorf("出站体不符（应为重建）:\n got  %s\n want %s", got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, chatUpstreamOK)
	})
	resp, err := http.Post(f.srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatReqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if v := resp.Header.Get("Content-Type"); v != "application/json" {
		t.Errorf("Content-Type = %q, 想要 application/json", v)
	}
	// 实测：非流式 chat 响应**没有** cache-control（harness ok-openai）。
	if v := resp.Header.Get("Cache-Control"); v != "" {
		t.Errorf("Cache-Control = %q, 想要空", v)
	}
	raw, _ := io.ReadAll(resp.Body)
	got := reCreated.ReplaceAllString(string(raw), `"created":<ts>`)
	want := `{"id":"msg_harness","object":"chat.completion","created":<ts>,"model":"GLM-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`
	if got != want {
		t.Errorf("响应体不符:\n got  %s\n want %s", got, want)
	}
}

// TestChatStreamConversion 流式：出站 stream 追加在末尾 + 入站转 OpenAI SSE，且**带** cache-control。
func TestChatStreamConversion(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		want := `{"model": "GLM-5.3", "messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}], "max_tokens": 16, "stream": true}`
		if string(got) != want {
			t.Errorf("出站体不符（stream 应在末尾）:\n got  %s\n want %s", got, want)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, chatUpstreamSSE)
	})
	resp, err := http.Post(f.srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatReqBodyStream))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if v := resp.Header.Get("Content-Type"); v != "text/event-stream; charset=utf-8" {
		t.Errorf("Content-Type = %q", v)
	}
	if v := resp.Header.Get("Cache-Control"); v != "no-cache" {
		t.Errorf("Cache-Control = %q, 想要 no-cache（SSE 加）", v)
	}
	raw, _ := io.ReadAll(resp.Body)
	got := reChatcmpl.ReplaceAllString(string(raw), "chatcmpl-<ID>")
	want := "data: {\"id\": \"chatcmpl-<ID>\", \"object\": \"chat.completion.chunk\", \"created\": 123, \"model\": \"GLM-5.3\", \"choices\": [{\"index\": 0, \"delta\": {\"role\": \"assistant\", \"content\": \"\"}, \"finish_reason\": null}]}\n\n" +
		"data: {\"id\": \"msg_harness\", \"object\": \"chat.completion.chunk\", \"created\": 123, \"model\": \"GLM-5.3\", \"choices\": [{\"index\": 0, \"delta\": {\"content\": \"hello\"}, \"finish_reason\": null}]}\n\n" +
		"data: {\"id\": \"msg_harness\", \"object\": \"chat.completion.chunk\", \"created\": 123, \"model\": \"GLM-5.3\", \"choices\": [{\"index\": 0, \"delta\": {}, \"finish_reason\": \"stop\"}], \"usage\": {\"prompt_tokens\": 3, \"completion_tokens\": 1, \"total_tokens\": 4}}\n\n" +
		"data: [DONE]\n\n"
	got = reCreatedS.ReplaceAllString(got, `"created": 123`)
	if got != want {
		t.Errorf("SSE 不符:\n got  %q\n want %q", got, want)
	}
}

// TestChatSSEOnNonStream 入站 stream 为假值 + 上游 SSE ⇒ 502 上游响应格式异常。
func TestChatSSEOnNonStream(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, chatUpstreamSSE)
	})
	resp, err := http.Post(f.srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatReqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("status = %d, 想要 502", resp.StatusCode)
	}
	if v := resp.Header.Get("Cache-Control"); v != "" {
		t.Errorf("Cache-Control = %q, 想要空", v)
	}
	raw, _ := io.ReadAll(resp.Body)
	want := `{"error":{"message":"上游响应格式异常","type":"upstream_error"}}`
	if string(raw) != want {
		t.Errorf("502 体不符:\n got  %s\n want %s", raw, want)
	}
}

// TestChatNonJSONOnNonStream 上游 200 但体不是 JSON ⇒ 同样 502。
func TestChatNonJSONOnNonStream(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(200)
		fmt.Fprint(w, "not json at all")
	})
	resp, err := http.Post(f.srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatReqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("status = %d, 想要 502", resp.StatusCode)
	}
}
