// Package bodytransform 是入站请求体 → 出站请求体的变换，以及路由日志所需字段的提取。
//
// # 一、`/v1/messages` 的出站变换（本包 `Messages`）
//
// 契约（**全部实测**，见 docs/contract/outbound/observations.md #9–#16 与
// `fixtures/outbound-requests.json` / `fixtures/serialization-probes.json`）：
//
//   - 唯一被改写的字段是 `messages[].content`：**字符串**包装成
//     `[{"type":"text","text":<原串>}]`；**已是数组**则原样。
//   - `system`（字符串或数组）、`tools`、`stream`、`temperature`、`top_p`、
//     `stop_sequences`、`metadata`、`model`、`max_tokens` **全部原样**。
//   - **键序保持入站顺序**，且整个 body 会被**重新序列化**成
//     Python `json.dumps(..., ensure_ascii=False)` 的形态 —— 所以实现是
//     「有序解析 → 改写 → Python 兼容序列化」，不是字符串拼接（见 pyjson 包注释）。
//
// # 二、路由日志字段（本包 `Inspect`）
//
// `>>> <reqid>  <model>  sync|stream  "<preview>"` 这一行的后三项由 `Inspect` 读出。
// 三项的取值规则**全部由探针钉死**（`fixtures/route-log-probes.json`），与直觉不同的地方：
//
//   - `preview` 取的是**最后一条 `role == "user"`** 消息的首段文本，**不是首条消息**。
//     全量采样里 m-multiturn（`[user hi, assistant yo, user again]`）印的是 `again`。
//   - `<model>` 在缺键 / `null` / 空串时印 `-`（真值判定），且**不做模型名校验**。
//   - `sync|stream` 用的是值的 **Python 真值**，不是 `is True`：
//     `"false"` 是**非空字符串 ⇒ 真值 ⇒ 印 `stream`**。
//
// # 三、不负责的事
//
// `/v1/chat/completions` 的**重建**语义（键序固定 `model, messages, max_tokens[, stream]`）
// 属 A4-4，见 `fixtures/chat-completions-requests.json`，不在本包的 `Messages` 里。
package bodytransform

import (
	"errors"

	"github.com/LinYuanNull/zcode2api-go/internal/pyjson"
)

// ErrNotObject 表示请求体是合法 JSON 但**根不是对象**。
//
// 参考实现对此回 400 `{"error":{"message":"请求体必须是 JSON 对象","type":"invalid_request_error"}}`
// （样本 docs/contract/gateway/26-messages-notobject.POST.json），且**不发生任何出站**。
var ErrNotObject = errors.New("请求体必须是 JSON 对象")

// ErrModelNotString 表示 `model` 以**非字符串、非 null** 的形态出现。
//
// 参考实现在这种入站体上会**未处理异常**：入站 500 `text/plain` `Internal Server Error`，
// 且**连 `>>>` 日志行都没打、零出站**（探针 `p-model-number-crashes`）。
// 本实现**有意偏离**，改为明确拒绝并给出原因 —— 崩溃不是契约，见 behavior.md §六。
var ErrModelNotString = errors.New("model 必须是字符串")

// Messages 按出站契约改写 `/v1/messages` 的请求体。
//
// 无论是否真的改动了 `content`，返回的都是**重新序列化**后的字节 ——
// 参考实现是 `json.loads` + `json.dumps` 往返，紧凑入站体会被写成带空格的形态
// （实测：入站 `{"model":"x"}` → 出站 `{"model": "x"}`）。
func Messages(raw []byte) ([]byte, error) {
	root, err := parseObjectBody(raw)
	if err != nil {
		return nil, err
	}
	wrapStringContents(root)
	return root.Marshal(), nil
}

// RequestInfo 是路由日志（`>>>` 行）需要从请求体里读出的三项。
type RequestInfo struct {
	// Model 是 `model` 字段的值；缺键 / `null` / 空串 ⇒ 空串（日志印 `-`）。
	Model string
	// Stream 是 `stream` 字段的 **Python 真值**（不是「严格等于 true」）。
	Stream bool
	// Preview 是**最后一条 `role == "user"`** 消息的首段文本；取不到时为空串。
	Preview string
}

// ModelLabel 返回日志里该印的模型名：真值取原值，否则 `-`。
func (ri RequestInfo) ModelLabel() string {
	if ri.Model == "" {
		return "-"
	}
	return ri.Model
}

// StreamLabel 返回日志里该印的第三字段。
func (ri RequestInfo) StreamLabel() string {
	if ri.Stream {
		return "stream"
	}
	return "sync"
}

// Inspect 读请求体里用于日志的三项，不改写任何内容。
//
// 除解析错误外还会返回 `ErrNotObject`（根不是对象）与 `ErrModelNotString`
// （`model` 是非字符串非 null 的值）—— 后者在参考实现里表现为崩溃，本实现有意改为拒绝。
func Inspect(raw []byte) (RequestInfo, error) {
	root, err := parseObjectBody(raw)
	if err != nil {
		return RequestInfo{}, err
	}
	var info RequestInfo

	if m, ok := root.Get("model"); ok {
		switch {
		case m.IsNull():
			// 印 `-`（Model 留空）。
		case m.IsString():
			info.Model = m.String()
		default:
			return RequestInfo{}, ErrModelNotString
		}
	}

	if s, ok := root.Get("stream"); ok {
		info.Stream = PyTruthy(s)
	}

	info.Preview = routeLogPreview(root)
	return info, nil
}

// routeLogPreview 复现 `>>>` 行第 4 字段的取值。
//
// 规则（fixtures/route-log-probes.json 的 `derived_rules.preview`）：
// 遍历 messages（必须是数组、元素必须是对象）；`role` 严格等于 `"user"` 时 ——
//
//   - `content` 是**字符串** ⇒ 取它（**空串也取**，会覆盖）；
//   - `content` 是**数组** ⇒ 取第一个 `type == "text"` 块的 `text`
//     （块里缺 `text` 键 ⇒ 取空串并覆盖；找不到 text 块 ⇒ **不动**）；
//   - 其它形态（数字 / null / 对象）⇒ 不动。
//
// 「不动」与「覆盖成空串」的差别是实测出来的：`[{user "A"}, {user ""}]` → `""`，
// 而 `[{user []}, {user "B"}]` → `"B"`。
func routeLogPreview(root *pyjson.Value) string {
	msgs, ok := root.Get("messages")
	if !ok || !msgs.IsArray() {
		return ""
	}
	preview := ""
	for i := 0; i < msgs.Len(); i++ {
		m := msgs.Index(i)
		if m == nil || !m.IsObject() {
			continue
		}
		role, ok := m.Get("role")
		if !ok || !role.IsString() || role.String() != "user" {
			continue
		}
		c, ok := m.Get("content")
		if !ok {
			continue
		}
		if t, ok := previewOf(c); ok {
			preview = t
		}
	}
	return preview
}

// previewOf 从一条消息的 `content` 里取首段文本。
//
// 第二个返回值表示「取到了值」（含取到空串）—— 取不到时调用方**不应覆盖**已有值。
func previewOf(content *pyjson.Value) (string, bool) {
	switch {
	case content.IsString():
		return content.String(), true
	case content.IsArray():
		for i := 0; i < content.Len(); i++ {
			b := content.Index(i)
			if b == nil || !b.IsObject() {
				continue
			}
			t, ok := b.Get("type")
			if !ok || !t.IsString() || t.String() != "text" {
				continue
			}
			// 找到了 text 块：块里没有 text 键、或值不是字符串 ⇒ 空串。
			//
			// 参考实现在「值不是字符串」时**崩溃**（探针 p-text-nonstring-crashes）；
			// 本实现有意改为空串（不崩），见 behavior.md §六。
			if s, ok := b.Get("text"); ok && s.IsString() {
				return s.String(), true
			}
			return "", true
		}
	}
	return "", false
}

// PyTruthy 复现 Python 的真值判定（`bool(x)`）—— 是 pyjson.Truthy 的别名。
//
// `/v1/messages` 与 `/v1/chat/completions` 都用它：前者判 `>>>` 第 3 字段，
// 后者判流式/非流式与是否把 `stream: true` 写进出站体（实测 `stream:"false"`
// 是真值 ⇒ 出站 `"stream": true`）。
func PyTruthy(v *pyjson.Value) bool { return pyjson.Truthy(v) }

// parseObjectBody 解析请求体并要求根是对象。
func parseObjectBody(raw []byte) (*pyjson.Value, error) {
	root, err := pyjson.Parse(raw)
	if err != nil {
		return nil, err
	}
	if !root.IsObject() {
		return nil, ErrNotObject
	}
	return root, nil
}

// WrapMessageContents 就地改写 `messages[].content` 里的字符串值。
//
// 这条规则**两个入口共用**（`/v1/messages` 保序改写与 `/v1/chat/completions`
// 重建都做同样的包装，见 fixtures/chat-completions-requests.json
// `_observations` 第 2 条），所以在此导出给 compat 复用，避免两处规则漂移。
//
// 只在 `messages` **是数组**、元素**是对象**、`content` **是字符串**时才动；
// 其余形态一律原样（实测：`messages` 是对象、`content` 是数字 / null 时参考实现都原样转发）。
func WrapMessageContents(root *pyjson.Value) { wrapStringContents(root) }

// wrapStringContents 就地改写 `messages[].content` 里的字符串值。
//
// 只在 `messages` **是数组**、元素**是对象**、`content` **是字符串**时才动；
// 其余形态一律原样（实测：`messages` 是对象、`content` 是数字 / null 时参考实现都原样转发）。
func wrapStringContents(root *pyjson.Value) {
	msgs, ok := root.Get("messages")
	if !ok || !msgs.IsArray() {
		return
	}
	for i := 0; i < msgs.Len(); i++ {
		m := msgs.Index(i)
		if m == nil || !m.IsObject() {
			continue
		}
		c, ok := m.Get("content")
		if !ok || !c.IsString() {
			continue
		}
		m.Set("content", textBlocks(c.String()))
	}
}

// textBlocks 把一段文本包成 Anthropic 的 text 块数组。
func textBlocks(text string) *pyjson.Value {
	block := pyjson.NewObject()
	block.Set("type", pyjson.NewString("text"))
	block.Set("text", pyjson.NewString(text))
	return pyjson.NewArray(block)
}
