// Package compat 是 OpenAI 兼容层：`/v1/chat/completions` 的请求重建、
// 响应转换与 SSE 转换。
//
// # 契约来源（全部实测，不许凭直觉改）
//
//   - `docs/contract/outbound/fixtures/chat-completions-requests.json`：入站↔出站配对。
//   - `docs/contract/outbound/behavior.md` §一/§二：两个入口的转发语义差异与响应形状。
//   - 实测工具：`tools/behavior_diff.py`（假上游把响应当输入参数）+ 对 Python 基线的
//     逐字节对照（`ok-openai` / `ok-openai-stream` / `chat-stream-from-json` /
//     `chat-sse-on-nonstream` / `stream-string-false` 五个场景）。
//
// # 两个入口的语义**完全不同**（最容易被"顺手统一"搞错的地方）
//
//	| 入口                    | 出站 body           | 入站响应                        |
//	|------------------------|---------------------|---------------------------------|
//	| `POST /v1/messages`     | 保序改写（只包 content） | 上游响应**逐字节透传**            |
//	| `POST /v1/chat/...`     | **白名单重建**        | **必须解析**成 OpenAI 形状 / SSE  |
//
// 所以本包**不能**复用 `/v1/messages` 的 `bodytransform.Messages`；两者只在
// 「`messages[].content` 字符串包装」这一条规则上共用实现。
package compat

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/LinYuanNull/zcode2api-go/internal/bodytransform"
	"github.com/LinYuanNull/zcode2api-go/internal/pyjson"
)

// chatIDPrefix 是流式首帧 id 的前缀（实测 `chatcmpl-<24 位小写 hex>`）。
const chatIDPrefix = "chatcmpl-"

// ErrUpstreamFormat 表示上游 200 的响应体**解析不出 Anthropic message**。
//
// 客户端的判据（behavior.md §一）：`/v1/chat/completions` 且入站 `stream` 为
// **假值**时，上游给 SSE 或非 JSON 文本都会走到这里 ⇒ 网关回
// `502 {"error":{"message":"上游响应格式异常","type":"upstream_error"}}`。
var ErrUpstreamFormat = errors.New("上游响应格式异常")

// NewChatCompletionID 生成流式首帧的 id：`chatcmpl-` + 12 字节随机数的 24 位小写 hex。
//
// 实测形态（harness `ok-openai-stream`）：`chatcmpl-449bcc5fc74a495681af128f`。
// 该字段在对照里被掩码为易变值，取值不参与逐字段比较。
func NewChatCompletionID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 不可用极罕见；响应路径不能 panic，用时间兜底。
		return chatIDPrefix + fmt.Sprintf("%024x", uint64(time.Now().UnixNano()))
	}
	return chatIDPrefix + hex.EncodeToString(b[:])
}

// RebuildChatRequest 把 `/v1/chat/completions` 的入站体重建成出站 Anthropic 体。
//
// 契约（`fixtures/chat-completions-requests.json` 全部实测）：
//
//   - 出站键序**固定**为 `model, messages, max_tokens`；其它入站字段
//     （`temperature` / `top_p` / `tools` / `n` …）**一律丢弃** ——
//     这是**白名单重建**，不是保序改写（入站 `model,max_tokens,messages`
//     会变成出站 `model,messages,max_tokens`）。
//   - `stream` 只在**真值**为真时追加到**末尾**，值恒为 `true`
//     （实测 `stream:"false"` ⇒ 出站 `"stream": true`；`stream` 缺失/假值 ⇒
//     出站**没有**该键）。
//   - `messages[].content` 的字符串包装规则与 `/v1/messages` **完全相同**
//     （复用 `bodytransform.WrapMessageContents`）。
//   - 缺失的 `model` / `messages` / `max_tokens` 写 `null`（Python
//     `body.get(key)` 返回 `None` 的形态；**未采样**，登记在 behavior.md §六）。
//   - 序列化是 Python `json.dumps` **默认分隔符**（带空格），与出站实测一致。
//
// `stream` 由调用方传入（已由 `bodytransform.Inspect` 按 Python 真值算好），
// 避免在此重复一遍真值判定，也保证两处口径永远一致。
func RebuildChatRequest(raw []byte, stream bool) ([]byte, error) {
	root, err := pyjson.Parse(raw)
	if err != nil {
		return nil, err
	}
	if !root.IsObject() {
		return nil, bodytransform.ErrNotObject
	}
	// 先把 messages 里的字符串 content 包成 text 块（与 messages 入口同一实现）。
	bodytransform.WrapMessageContents(root)

	out := pyjson.NewObject()
	out.Set("model", fieldOrNull(root, "model"))
	out.Set("messages", fieldOrNull(root, "messages"))
	out.Set("max_tokens", fieldOrNull(root, "max_tokens"))
	if stream {
		out.Set("stream", pyjson.NewBool(true))
	}
	return out.Marshal(), nil
}

// ToOpenAIResponse 把上游 Anthropic message（JSON 字节）转成 OpenAI
// `chat.completion`（**紧凑 JSON**，Starlette `JSONResponse` 形态）。
//
// 形状（behavior.md §2.1，逐字节实测）：
//
//	{"id":…,"object":"chat.completion","created":<unix秒>,"model":…,
//	 "choices":[{"index":0,"message":{"role":"assistant","content":<所有 text 块拼接>},
//	             "finish_reason":<映射>}],
//	 "usage":{"prompt_tokens":…,"completion_tokens":…,"total_tokens":…}}
//
// `id` / `model` 取**上游 message 的**同名字段（缺键 ⇒ `null`）；`created` 取
// `now` 的 unix 秒；`content` 是把所有 `type=="text"` 块的 `text` 拼起来。
//
// 解析失败（不是 JSON、根不是对象）⇒ `ErrUpstreamFormat`。
func ToOpenAIResponse(body []byte, now time.Time) ([]byte, error) {
	msg, err := pyjson.Parse(body)
	if err != nil || !msg.IsObject() {
		return nil, ErrUpstreamFormat
	}

	message := pyjson.NewObject()
	message.Set("role", pyjson.NewString("assistant"))
	message.Set("content", pyjson.NewString(collectText(msg)))

	choice := pyjson.NewObject()
	choice.Set("index", pyjson.NewNumber("0"))
	choice.Set("message", message)
	choice.Set("finish_reason", finishReason(msg))

	inTok := tokenOf(child(msg, "usage"), "input_tokens")
	outTok := tokenOf(child(msg, "usage"), "output_tokens")
	usage := pyjson.NewObject()
	usage.Set("prompt_tokens", intValue(inTok))
	usage.Set("completion_tokens", intValue(outTok))
	usage.Set("total_tokens", intValue(inTok+outTok))

	out := pyjson.NewObject()
	out.Set("id", fieldOrNull(msg, "id"))
	out.Set("object", pyjson.NewString("chat.completion"))
	out.Set("created", pyjson.NewNumber(strconv.FormatInt(now.Unix(), 10)))
	out.Set("model", fieldOrNull(msg, "model"))
	out.Set("choices", pyjson.NewArray(choice))
	out.Set("usage", usage)
	return out.MarshalCompact(), nil
}

// StreamOpenAI 把上游响应体逐行转成 OpenAI SSE 写到 w。
//
// 输入既可能是 Anthropic SSE，也可能是**单个** Anthropic message JSON ——
// 两种都按同一逻辑处理：逐行扫 `data: ` 前缀，扫到才解析事件。
//
// 关键实测（harness `ok-openai-stream` / `chat-stream-from-json`）：
//
//   - 首帧 id 是**新生成**的 `chatcmpl-<24hex>`；后续帧用 `message_start` 里的
//     上游 id。`model` 一律取**入站请求的 model** —— 首帧在解析任何事件**之前**
//     就要写 model，且上游给 JSON 体时根本不解析事件（见下一条）。
//   - 上游是 JSON 体时**扫不到任何 `data: ` 行** ⇒ 只发首帧 + `[DONE]`，
//     **不报错**（`chat-stream-from-json` 实测 len=240，只有两帧）。
//   - `prompt_tokens` ← `message_start.message.usage.input_tokens`；
//     `completion_tokens` ← `message_delta.usage.output_tokens`；
//     `total_tokens` = 两者之和。
//   - 末帧在**收到 `message_delta` 时**发出，不是循环结束后补 —— 否则
//     `chat-stream-from-json` 也会多出一帧。
//
// 每帧写成 `data: <JSON>\n\n`，JSON 用 Python **默认分隔符**（带空格）。
func StreamOpenAI(w io.Writer, upstream io.Reader, reqModel string, now time.Time) error {
	created := now.Unix()
	if err := writeFrame(w, chunkFrame(NewChatCompletionID(), reqModel, created,
		deltaRole(), pyjson.NewNull(), nil)); err != nil {
		return err
	}

	var (
		msgID  string
		prompt int64
	)
	sc := bufio.NewScanner(upstream)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		ev, err := pyjson.Parse([]byte(line[len("data: "):]))
		if err != nil || !ev.IsObject() {
			// 上游给了非法 data 行 ⇒ 跳过（未采样，登记 behavior.md §六）。
			continue
		}
		switch stringOf(ev, "type") {
		case "message_start":
			msg := child(ev, "message")
			msgID = stringOf(msg, "id")
			prompt = tokenOf(child(msg, "usage"), "input_tokens")
		case "content_block_delta":
			d := child(ev, "delta")
			if d == nil {
				continue
			}
			txt, ok := d.Get("text")
			if !ok || !pyjson.Truthy(txt) {
				continue
			}
			if err := writeFrame(w, chunkFrame(msgID, reqModel, created,
				deltaContent(txt), pyjson.NewNull(), nil)); err != nil {
				return err
			}
		case "message_delta":
			outTok := tokenOf(child(ev, "usage"), "output_tokens")
			if err := writeFrame(w, chunkFrame(msgID, reqModel, created,
				pyjson.NewObject(), stopReasonOf(ev),
				usageObject(prompt, outTok))); err != nil {
				return err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return writeFrame(w, []byte("data: [DONE]\n\n"))
}

// writeFrame 写一帧并保证整帧一次写入（便于调用方在每次写后 flush）。
func writeFrame(w io.Writer, frame []byte) error {
	_, err := w.Write(frame)
	return err
}

// chunkFrame 组装一帧 `chat.completion.chunk`。
//
// 键序（实测）：`id, object, created, model, choices[, usage]`；
// `choices[0]` = `index, delta, finish_reason`。只有末帧带 `usage`。
func chunkFrame(id, model string, created int64, delta, finish *pyjson.Value, usage *pyjson.Value) []byte {
	choice := pyjson.NewObject()
	choice.Set("index", pyjson.NewNumber("0"))
	choice.Set("delta", delta)
	choice.Set("finish_reason", finish)

	obj := pyjson.NewObject()
	obj.Set("id", pyjson.NewString(id))
	obj.Set("object", pyjson.NewString("chat.completion.chunk"))
	obj.Set("created", pyjson.NewNumber(strconv.FormatInt(created, 10)))
	obj.Set("model", pyjson.NewString(model))
	obj.Set("choices", pyjson.NewArray(choice))
	if usage != nil {
		obj.Set("usage", usage)
	}

	frame := make([]byte, 0, 160)
	frame = append(frame, "data: "...)
	frame = append(frame, obj.Marshal()...)
	return append(frame, '\n', '\n')
}

// deltaRole 是首帧的 delta：`{"role": "assistant", "content": ""}`。
func deltaRole() *pyjson.Value {
	d := pyjson.NewObject()
	d.Set("role", pyjson.NewString("assistant"))
	d.Set("content", pyjson.NewString(""))
	return d
}

// deltaContent 是内容帧的 delta：`{"content": <text>}`（值原样搬运）。
func deltaContent(text *pyjson.Value) *pyjson.Value {
	d := pyjson.NewObject()
	d.Set("content", text)
	return d
}

// usageObject 组装末帧的 usage（键序 prompt_tokens, completion_tokens, total_tokens）。
func usageObject(prompt, completion int64) *pyjson.Value {
	u := pyjson.NewObject()
	u.Set("prompt_tokens", intValue(prompt))
	u.Set("completion_tokens", intValue(completion))
	u.Set("total_tokens", intValue(prompt+completion))
	return u
}

// intValue 把一个整数包成 JSON 数字值。
func intValue(n int64) *pyjson.Value {
	return pyjson.NewNumber(strconv.FormatInt(n, 10))
}

// stopReasonOf 读 `message_delta.delta.stop_reason` 并映射成 OpenAI 的
// `finish_reason`；缺失 / 非字符串 / null ⇒ `null`。
func stopReasonOf(ev *pyjson.Value) *pyjson.Value {
	d := child(ev, "delta")
	if d == nil {
		return pyjson.NewNull()
	}
	sr, ok := d.Get("stop_reason")
	if !ok || !sr.IsString() {
		return pyjson.NewNull()
	}
	return pyjson.NewString(mapStopReason(sr.String()))
}

// finishReason 读非流式 message 的 `stop_reason` 并映射；缺失 / null ⇒ `null`。
func finishReason(msg *pyjson.Value) *pyjson.Value {
	sr, ok := msg.Get("stop_reason")
	if !ok || !sr.IsString() {
		return pyjson.NewNull()
	}
	return pyjson.NewString(mapStopReason(sr.String()))
}

// mapStopReason 把 Anthropic 的 `stop_reason` 映射成 OpenAI 的 `finish_reason`。
//
// 只有 `end_turn → stop` 是实测（harness `ok-openai` / `ok-openai-stream`）；
// 其余映射按两家公开规范的常识对齐，**未采样**，登记在 behavior.md §六；
// 未知取值**原样透出**（不猜）。
func mapStopReason(s string) string {
	switch s {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	}
	return s
}

// collectText 把所有 `type == "text"` 块的 `text` 拼成一个字符串。
//
// `content` 本身是字符串时直接用它（未采样，登记 §六）；其余形态返回空串。
func collectText(msg *pyjson.Value) string {
	c, ok := msg.Get("content")
	if !ok {
		return ""
	}
	if c.IsString() {
		return c.String()
	}
	if !c.IsArray() {
		return ""
	}
	var b strings.Builder
	for i := 0; i < c.Len(); i++ {
		blk := c.Index(i)
		if blk == nil || !blk.IsObject() {
			continue
		}
		if stringOf(blk, "type") != "text" {
			continue
		}
		if txt, ok := blk.Get("text"); ok && txt.IsString() {
			b.WriteString(txt.String())
		}
	}
	return b.String()
}

// tokenOf 读对象里某个 token 数成员（如 `usage.input_tokens`）；缺失 / 非数字 ⇒ 0。
//
// 实测都是整数；非整数形态未采样，这里按取整处理（登记 §六）。
func tokenOf(obj *pyjson.Value, key string) int64 {
	if obj == nil {
		return 0
	}
	v, ok := obj.Get(key)
	if !ok || !v.IsNumber() {
		return 0
	}
	text := v.NumberText()
	if n, err := strconv.ParseInt(text, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(text, 64); err == nil {
		return int64(f)
	}
	return 0
}

// fieldOrNull 取对象的成员；不存在时返回 `null` 值。
func fieldOrNull(v *pyjson.Value, key string) *pyjson.Value {
	if c, ok := v.Get(key); ok {
		return c
	}
	return pyjson.NewNull()
}

// stringOf 取对象的字符串成员；缺失 / 非字符串 ⇒ 空串。
func stringOf(v *pyjson.Value, key string) string {
	if v == nil {
		return ""
	}
	c, ok := v.Get(key)
	if !ok || !c.IsString() {
		return ""
	}
	return c.String()
}

// child 取对象的**对象**成员；缺失 / 非对象 ⇒ nil。
func child(v *pyjson.Value, key string) *pyjson.Value {
	if v == nil {
		return nil
	}
	c, ok := v.Get(key)
	if !ok || !c.IsObject() {
		return nil
	}
	return c
}
