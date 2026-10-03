package models

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// OrderedObject 是一个**保持键顺序**的 JSON 对象。
//
// 为什么需要它：靶机用 Python 的 `json.dumps` 落盘 `accounts.data`，
// 字典的插入序就是字节序。Go 的 `map` 无序、`struct` 只能表达固定字段集，
// 两者都不足以同时做到「顺序稳定」与「未知字段原样带过」。
//
// 顺序不是学究问题 —— 它是**文件字节层面可观测的事实**，也是 A2 双向读校验的判据
// （见 docs/contract/store/observations.md 第 2 节）。未知字段原样带过则是为了
// 上游将来加字段时，本实现不会把它抹掉。
//
// A3 起它也是 HTTP 层的工具：`GET /admin/api/export` 的 `providers` 是
// 「provider → 条目数组」的映射，键顺序必须是枚举序（`zai` 在前），
// 用 map 就丢了。
type OrderedObject struct {
	keys []string
	vals map[string]json.RawMessage
}

// NewOrderedObject 建一个空的有序对象。
func NewOrderedObject() *OrderedObject {
	return &OrderedObject{vals: make(map[string]json.RawMessage)}
}

// parseOrderedObject 用流式 Token 逐个读出键顺序。
// 直接 json.Unmarshal 到 map 会丢顺序，所以这里必须走 Decoder。
func parseOrderedObject(b []byte) (*OrderedObject, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("期望 JSON 对象，实际是 %v", tok)
	}
	o := NewOrderedObject()
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, fmt.Errorf("对象键不是字符串: %v", kt)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		if _, dup := o.vals[key]; dup {
			return nil, fmt.Errorf("JSON 对象出现重复键 %q", key)
		}
		o.keys = append(o.keys, key)
		o.vals[key] = raw
	}
	// 吃掉收尾的 '}'
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return o, nil
}

// Has 报告键是否存在。
func (o *OrderedObject) Has(key string) bool {
	_, ok := o.vals[key]
	return ok
}

// Raw 取原始值（不存在时返回 nil, false）。
func (o *OrderedObject) Raw(key string) (json.RawMessage, bool) {
	v, ok := o.vals[key]
	return v, ok
}

// SetRaw 写入原始值：键已存在则**原位替换**（保持位置），否则追加到末尾。
func (o *OrderedObject) SetRaw(key string, v json.RawMessage) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
}

// Set 用 json 编码后的值写入。
func (o *OrderedObject) Set(key string, v any) error {
	b, err := marshalNoHTMLEscape(v)
	if err != nil {
		return err
	}
	o.SetRaw(key, b)
	return nil
}

// Take 移除一个键（同时从顺序表里摘掉）。
func (o *OrderedObject) Take(key string) {
	if _, ok := o.vals[key]; !ok {
		return
	}
	delete(o.vals, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// Bytes 按键顺序编码回 JSON。
func (o *OrderedObject) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := marshalNoHTMLEscape(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		raw := o.vals[k]
		if len(raw) == 0 {
			raw = []byte("null")
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			return nil, fmt.Errorf("键 %q 的值不是合法 JSON: %w", k, err)
		}
		buf.Write(compact.Bytes())
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// marshalNoHTMLEscape 是内部别名，见 MarshalNoHTMLEscape。
func marshalNoHTMLEscape(v any) ([]byte, error) { return MarshalNoHTMLEscape(v) }

// MarshalNoHTMLEscape 是**保真编码**的唯一入口：不把 < > & 转成 \u00xx。
//
// 为什么必须显式调用它、而不能用 `json.Marshal`：
//
//   - `json.Marshal` / `json.NewEncoder` 默认转义 HTML 字符；
//   - 即使某类型自己实现了 `MarshalJSON` 并返回未转义字节，**外层**的
//     `json.Marshal` 仍会对结果再做一次「带转义的 compact」。所以「在 MarshalJSON
//     里想办法」是无效的 —— 调用方必须走这个入口，否则 `<` 会变成 `\u003c`。
//   - 靶机用 Python `json.dumps(..., ensure_ascii=False)`：既不转义非 ASCII，
//     也不转义 HTML 字符。落盘 blob 与 HTTP 响应都必须是同形，否则逐字段比对
//     会出现无谓差异（实测：账号名含 `<` 时两者不同形）。
//
// A3 的 HTTP 响应编码同样必须用它。
func MarshalNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encode 会补一个换行
	b := buf.Bytes()
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	return b, nil
}

// MarshalAccount 按落盘保真规则编码账号的 `data` blob（store 与测试都走这里）。
func MarshalAccount(a Account) ([]byte, error) { return MarshalNoHTMLEscape(a) }

// decodeField 把原始值解到目标；字段不存在时保持目标不动。
func decodeField(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, dst)
}

// decodeOptString 解可空字符串：null / 缺失 → nil。
func decodeOptString(raw json.RawMessage) (*string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// decodeOptFloat 解可空浮点：null / 缺失 → nil。
func decodeOptFloat(raw json.RawMessage) (*float64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	return &f, nil
}
