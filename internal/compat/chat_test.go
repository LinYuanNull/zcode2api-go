package compat

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/LinYuanNull/zcode2api-go/internal/pyjson"
)

// 期望值全部取自实测基线：
//   - 出站：fixtures/chat-completions-requests.json 的 pairs；
//   - 响应：对 Python 基线跑的 harness（tools/_dump_py_scenario.py）逐字节输出。

// anthropicOK 是 harness 里 ANTHROPIC_OK 的原文。
const anthropicOK = `{"id": "msg_harness", "type": "message", "role": "assistant", "model": "GLM-5.3", "content": [{"type": "text", "text": "hello"}], "stop_reason": "end_turn", "stop_sequence": null, "usage": {"input_tokens": 3, "output_tokens": 1}}`

// anthropicSSE 是 harness 里 ANTHROPIC_SSE 的 6 帧（每帧含 event: 行）。
var anthropicSSE = strings.Join([]string{
	`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_harness","type":"message","role":"assistant","model":"GLM-5.3","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":0}}}`,
	`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
	`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
	`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
	`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
}, "\n\n") + "\n\n"

func TestRebuildChatRequestNonStream(t *testing.T) {
	in := []byte(`{"model": "GLM-5.3", "max_tokens": 16, "messages": [{"role": "user", "content": "hi"}]}`)
	got, err := RebuildChatRequest(in, false)
	if err != nil {
		t.Fatalf("RebuildChatRequest: %v", err)
	}
	// 键序固定 model, messages, max_tokens；Python 默认分隔符（带空格）。
	want := `{"model": "GLM-5.3", "messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}], "max_tokens": 16}`
	if string(got) != want {
		t.Errorf("出站 body 不一致\n got: %s\nwant: %s", got, want)
	}
}

func TestRebuildChatRequestStreamAppendedLast(t *testing.T) {
	// 入站键序是 model/max_tokens/stream/messages，出站必须重排成
	// model/messages/max_tokens/stream（实测 ok-openai-stream）。
	in := []byte(`{"model": "GLM-5.3", "max_tokens": 16, "stream": true, "messages": [{"role": "user", "content": "hi"}]}`)
	got, err := RebuildChatRequest(in, true)
	if err != nil {
		t.Fatalf("RebuildChatRequest: %v", err)
	}
	want := `{"model": "GLM-5.3", "messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}], "max_tokens": 16, "stream": true}`
	if string(got) != want {
		t.Errorf("出站 body 不一致\n got: %s\nwant: %s", got, want)
	}
}

func TestRebuildChatRequestStreamFalseOmitsKey(t *testing.T) {
	// stream 真值为假 ⇒ 出站**没有**该键（与入站是否有该键无关）。
	in := []byte(`{"model": "GLM-5.3", "max_tokens": 16, "stream": false, "messages": [{"role": "user", "content": "hi"}]}`)
	got, err := RebuildChatRequest(in, false)
	if err != nil {
		t.Fatalf("RebuildChatRequest: %v", err)
	}
	if strings.Contains(string(got), `"stream"`) {
		t.Errorf("stream 假值时不该出现该键: %s", got)
	}
}

func TestRebuildChatRequestDropsUnknownFields(t *testing.T) {
	// 白名单重建：temperature / tools / n 等一律丢弃（未采样但由「重建」语义决定）。
	in := []byte(`{"model": "GLM-5.3", "max_tokens": 16, "temperature": 0.7, "tools": [1], "n": 2, "messages": [{"role": "user", "content": "hi"}]}`)
	got, err := RebuildChatRequest(in, false)
	if err != nil {
		t.Fatalf("RebuildChatRequest: %v", err)
	}
	for _, k := range []string{"temperature", "tools", `"n"`} {
		if strings.Contains(string(got), k) {
			t.Errorf("重建体不该含 %s: %s", k, got)
		}
	}
}

func TestRebuildChatRequestMissingKeysAreNull(t *testing.T) {
	got, err := RebuildChatRequest([]byte(`{}`), false)
	if err != nil {
		t.Fatalf("RebuildChatRequest: %v", err)
	}
	want := `{"model": null, "messages": null, "max_tokens": null}`
	if string(got) != want {
		t.Errorf(" got: %s\nwant: %s", got, want)
	}
}

func TestRebuildChatRequestNotObject(t *testing.T) {
	if _, err := RebuildChatRequest([]byte(`[1,2]`), false); err == nil {
		t.Fatal("根不是对象时应报错")
	}
}

func TestToOpenAIResponse(t *testing.T) {
	now := time.Unix(1791059609, 0)
	got, err := ToOpenAIResponse([]byte(anthropicOK), now)
	if err != nil {
		t.Fatalf("ToOpenAIResponse: %v", err)
	}
	// 紧凑 JSON（Starlette JSONResponse 形态），逐字节实测。
	want := `{"id":"msg_harness","object":"chat.completion","created":1791059609,"model":"GLM-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`
	if string(got) != want {
		t.Errorf("响应不一致\n got: %s\nwant: %s", got, want)
	}
}

func TestToOpenAIResponseConcatenatesTextBlocks(t *testing.T) {
	body := `{"id":"m1","model":"M","content":[{"type":"text","text":"a"},{"type":"tool_use","id":"t"},{"type":"text","text":"b"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`
	got, err := ToOpenAIResponse([]byte(body), time.Unix(0, 0))
	if err != nil {
		t.Fatalf("ToOpenAIResponse: %v", err)
	}
	if !strings.Contains(string(got), `"content":"ab"`) {
		t.Errorf("多 text 块应拼接: %s", got)
	}
}

func TestToOpenAIResponseRejectsNonJSON(t *testing.T) {
	// 上游 SSE 或非 JSON 文本都走这里 ⇒ ErrUpstreamFormat（网关回 502）。
	for _, body := range []string{"not json at all", "event: message_start\ndata: {}\n\n", "[1,2]"} {
		if _, err := ToOpenAIResponse([]byte(body), time.Unix(0, 0)); err == nil {
			t.Errorf("%q 应报 ErrUpstreamFormat", body)
		}
	}
}

var chatcmplRe = regexp.MustCompile(`chatcmpl-[0-9a-f]+`)

func TestStreamOpenAIFromSSE(t *testing.T) {
	var sb strings.Builder
	if err := StreamOpenAI(&sb, strings.NewReader(anthropicSSE), "GLM-5.3", time.Unix(1791059612, 0)); err != nil {
		t.Fatalf("StreamOpenAI: %v", err)
	}
	// 首帧 id 是易变值，替换后逐字节比较其余部分。
	got := chatcmplRe.ReplaceAllString(sb.String(), "chatcmpl-<ID>")
	want := "data: {\"id\": \"chatcmpl-<ID>\", \"object\": \"chat.completion.chunk\", \"created\": 1791059612, \"model\": \"GLM-5.3\", \"choices\": [{\"index\": 0, \"delta\": {\"role\": \"assistant\", \"content\": \"\"}, \"finish_reason\": null}]}\n\n" +
		"data: {\"id\": \"msg_harness\", \"object\": \"chat.completion.chunk\", \"created\": 1791059612, \"model\": \"GLM-5.3\", \"choices\": [{\"index\": 0, \"delta\": {\"content\": \"hello\"}, \"finish_reason\": null}]}\n\n" +
		"data: {\"id\": \"msg_harness\", \"object\": \"chat.completion.chunk\", \"created\": 1791059612, \"model\": \"GLM-5.3\", \"choices\": [{\"index\": 0, \"delta\": {}, \"finish_reason\": \"stop\"}], \"usage\": {\"prompt_tokens\": 3, \"completion_tokens\": 1, \"total_tokens\": 4}}\n\n" +
		"data: [DONE]\n\n"
	if got != want {
		t.Errorf("SSE 不一致\n got: %q\nwant: %q", got, want)
	}
}

func TestStreamOpenAIFromPlainJSON(t *testing.T) {
	// 上游给 JSON 体 ⇒ 扫不到 data: 行 ⇒ 只发首帧 + [DONE]（实测 len=240）。
	var sb strings.Builder
	if err := StreamOpenAI(&sb, strings.NewReader(anthropicOK), "GLM-5.3", time.Unix(1791059618, 0)); err != nil {
		t.Fatalf("StreamOpenAI: %v", err)
	}
	got := chatcmplRe.ReplaceAllString(sb.String(), "chatcmpl-<ID>")
	want := "data: {\"id\": \"chatcmpl-<ID>\", \"object\": \"chat.completion.chunk\", \"created\": 1791059618, \"model\": \"GLM-5.3\", \"choices\": [{\"index\": 0, \"delta\": {\"role\": \"assistant\", \"content\": \"\"}, \"finish_reason\": null}]}\n\n" +
		"data: [DONE]\n\n"
	if got != want {
		t.Errorf("SSE 不一致\n got: %q\nwant: %q", got, want)
	}
}

func TestStreamOpenAISkipsEmptyTextDelta(t *testing.T) {
	body := `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}` + "\n\n"
	var sb strings.Builder
	if err := StreamOpenAI(&sb, strings.NewReader(body), "M", time.Unix(0, 0)); err != nil {
		t.Fatalf("StreamOpenAI: %v", err)
	}
	if strings.Contains(sb.String(), `"content": ""`) && strings.Contains(sb.String(), "delta\": {\"content") {
		t.Errorf("空 text 不该产生内容帧:\n%s", sb.String())
	}
}

func TestNewChatCompletionIDFormat(t *testing.T) {
	id := NewChatCompletionID()
	if !chatcmplRe.MatchString(id) {
		t.Errorf("id %q 不符合 chatcmpl-<24hex>", id)
	}
	if got := len(id); got != len("chatcmpl-")+24 {
		t.Errorf("id 长度 = %d, 想要 %d", got, len("chatcmpl-")+24)
	}
	if NewChatCompletionID() == id {
		t.Error("两次生成不应相同")
	}
}

func TestMapStopReason(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "stop",
		"stop_sequence": "stop",
		"max_tokens":    "length",
		"tool_use":      "tool_calls",
		"pause_turn":    "pause_turn", // 未知取值原样透出
	}
	for in, want := range cases {
		if got := mapStopReason(in); got != want {
			t.Errorf("mapStopReason(%q) = %q, 想要 %q", in, got, want)
		}
	}
}

func TestFinishReasonNull(t *testing.T) {
	msg, err := pyjson.Parse([]byte(`{"content":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if v := finishReason(msg); !v.IsNull() {
		t.Errorf("stop_reason 缺失应得 null, 得到 %s", v.Marshal())
	}
}
