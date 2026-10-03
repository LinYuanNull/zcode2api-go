// Package pyjson 是 **Python `json.loads` + `json.dumps(..., ensure_ascii=False)` 兼容**的
// 保序 JSON 编解码器。
//
// 为什么需要它（而不是 encoding/json）
// ----------------------------------
// 转发链路要求出站 body 与参考实现**逐字节**一致，而参考实现是
//
//	data = json.loads(body); data[...] = ...; out = json.dumps(data, ensure_ascii=False)
//
// 于是出站字节由 **CPython 的序列化规则**决定，Go 的 encoding/json 有三处不同：
//
//  1. **键序**：Go 的 `map` 无序；`struct` 只能表达固定字段集。参考实现靠 Python
//     dict 保插入序，所以必须有一个有序对象模型。
//  2. **分隔符**：Python 默认 `", "` / `": "`（带空格）；Go 一律紧凑。
//  3. **数字**：Python 把整数字面量解析成**任意精度 int**（`-0` → `0`）、把带 `.`/`e`
//     的解析成 float 再按 `repr` 重排（`1e3` → `1000.0`）；Go 的 `json.Number`
//     原样保留文本，`float64` 又会毁掉大整数。
//
// 另有字符串转义：Python 在 `ensure_ascii=False` 下**只**转义 `"` `\` 与控制字符，
// 不转义 `< > &`，也不转义 U+2028/U+2029；而 Go 的编码器无条件转义后两者。
//
// 依据：docs/contract/outbound/observations.md #40–#46 与
// `fixtures/serialization-probes.json`（全部是实测，不是推测）。
//
// 未覆盖：孤代理（lone surrogate）—— Go 侧与 `encoding/json` 一致地替换为 U+FFFD。
package pyjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Kind 是值的种类。
type Kind uint8

// 取值与 JSON 的六种值一一对应。
const (
	Null Kind = iota
	Bool
	Number
	String
	Array
	Object
)

// Value 是一个 JSON 值。零值是 `null`。
//
// 对象用「键切片 + 映射」而不是单个 map：键切片给顺序，映射给 O(1) 取值。
// **改值走 Set**（原位替换，保持首次出现的位置），与 Python dict 的语义一致。
type Value struct {
	kind Kind
	b    bool
	num  string // 已按 Python 规则规范化后的数字文本
	str  string
	arr  []*Value
	keys []string
	obj  map[string]*Value
}

// Kind 返回值种类。
func (v *Value) Kind() Kind {
	if v == nil {
		return Null
	}
	return v.kind
}

// IsNull / IsBool / IsNumber / IsString / IsArray / IsObject 是种类判定。
func (v *Value) IsNull() bool   { return v.Kind() == Null }
func (v *Value) IsBool() bool   { return v.Kind() == Bool }
func (v *Value) IsNumber() bool { return v.Kind() == Number }
func (v *Value) IsString() bool { return v.Kind() == String }
func (v *Value) IsArray() bool  { return v.Kind() == Array }
func (v *Value) IsObject() bool { return v.Kind() == Object }

// Bool 返回布尔值（非布尔时返回 false）。
func (v *Value) Bool() bool { return v != nil && v.kind == Bool && v.b }

// String 返回字符串值（非字符串时返回空串）。
func (v *Value) String() string {
	if v != nil && v.kind == String {
		return v.str
	}
	return ""
}

// NumberText 返回规范化后的数字文本（非数字时返回空串）。
func (v *Value) NumberText() string {
	if v != nil && v.kind == Number {
		return v.num
	}
	return ""
}

// Len 返回数组长度 / 对象成员数（其它种类返回 0）。
func (v *Value) Len() int {
	switch v.Kind() {
	case Array:
		return len(v.arr)
	case Object:
		return len(v.keys)
	}
	return 0
}

// Index 取数组元素；越界或非数组返回 nil。
func (v *Value) Index(i int) *Value {
	if v == nil || v.kind != Array || i < 0 || i >= len(v.arr) {
		return nil
	}
	return v.arr[i]
}

// Get 取对象成员；不存在或非对象返回 nil, false。
func (v *Value) Get(key string) (*Value, bool) {
	if v == nil || v.kind != Object {
		return nil, false
	}
	c, ok := v.obj[key]
	return c, ok
}

// Set 写入对象成员：键已存在则**原位替换**（保持位置），否则追加到末尾。
// 非对象上调用无效果。返回自身以便链式构造。
func (v *Value) Set(key string, val *Value) *Value {
	if v == nil || v.kind != Object {
		return v
	}
	if _, ok := v.obj[key]; !ok {
		v.keys = append(v.keys, key)
	}
	v.obj[key] = val
	return v
}

// Keys 返回键顺序的副本。
func (v *Value) Keys() []string {
	if v == nil || v.kind != Object {
		return nil
	}
	out := make([]string, len(v.keys))
	copy(out, v.keys)
	return out
}

// NewNull / NewBool / NewString / NewNumber / NewArray / NewObject 是构造器。
func NewNull() *Value              { return &Value{kind: Null} }
func NewBool(b bool) *Value        { return &Value{kind: Bool, b: b} }
func NewString(s string) *Value    { return &Value{kind: String, str: s} }
func NewArray(vs ...*Value) *Value { return &Value{kind: Array, arr: vs} }

// NewNumber 用**规范化后的数字文本**建一个数字值（见 NormalizeNumber）。
func NewNumber(text string) *Value { return &Value{kind: Number, num: text} }

// NewObject 建一个空对象。
func NewObject() *Value { return &Value{kind: Object, obj: map[string]*Value{}} }

// Parse 解析 JSON，保留对象键序，并按 Python 规则规范化数字。
func Parse(raw []byte) (*Value, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	// 保证没有尾部多余内容（`json.Valid` 已过一遍，这里是本包的自我完整性）。
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("JSON 尾部有多余内容")
		}
		return nil, err
	}
	return v, nil
}

func parseValue(dec *json.Decoder) (*Value, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return parseObject(dec)
		case '[':
			return parseArray(dec)
		}
		return nil, fmt.Errorf("意外的 JSON 分隔符 %q", t)
	case string:
		return &Value{kind: String, str: t}, nil
	case json.Number:
		return normalizeNumber(string(t))
	case bool:
		return &Value{kind: Bool, b: t}, nil
	case nil:
		return &Value{kind: Null}, nil
	}
	return nil, fmt.Errorf("意外的 JSON 记号 %v", tok)
}

func parseObject(dec *json.Decoder) (*Value, error) {
	v := NewObject()
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, fmt.Errorf("对象键不是字符串: %v", kt)
		}
		child, err := parseValue(dec)
		if err != nil {
			return nil, err
		}
		// Python dict：重复键取**最后**的值、保留**首次**出现的位置。
		v.Set(key, child)
	}
	if _, err := dec.Token(); err != nil { // 吃掉 '}'
		return nil, err
	}
	return v, nil
}

func parseArray(dec *json.Decoder) (*Value, error) {
	v := &Value{kind: Array}
	for dec.More() {
		child, err := parseValue(dec)
		if err != nil {
			return nil, err
		}
		v.arr = append(v.arr, child)
	}
	if _, err := dec.Token(); err != nil { // 吃掉 ']'
		return nil, err
	}
	return v, nil
}

// normalizeNumber 把 JSON 数字字面量转成 Python 解析后的输出文本。
//
// 判据：字面量含 `.` / `e` / `E` ⇒ Python 解析成 float；否则解析成**任意精度 int**。
// 这条判据直接决定 `1e3` → `1000.0`、`100` → `100`、`-0` → `0`。
func normalizeNumber(tok string) (*Value, error) {
	if !strings.ContainsAny(tok, ".eE") {
		s := tok
		if strings.HasPrefix(s, "-") && isAllZero(s[1:]) {
			s = "0" // Python: json.loads("-0") is int 0
		}
		return &Value{kind: Number, num: s}, nil
	}
	f, err := strconv.ParseFloat(tok, 64)
	if err != nil {
		var ne *strconv.NumError
		if !(asNumError(err, &ne) && ne.Err == strconv.ErrRange) {
			return nil, fmt.Errorf("数字 %q 无法解析: %w", tok, err)
		}
		// 溢出：ParseFloat 已把 f 置为 ±Inf，Python 同样得到 ±inf。
	}
	return &Value{kind: Number, num: formatPyFloat(f)}, nil
}

func asNumError(err error, dst **strconv.NumError) bool {
	if ne, ok := err.(*strconv.NumError); ok {
		*dst = ne
		return true
	}
	return false
}

func isAllZero(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}

// formatPyFloat 复现 CPython `repr(float)` 的形态（也就是 `json.dumps` 对 float 的输出）。
//
// 规则（用 CPython 逐个复算确认，见 fixtures/serialization-probes.json）：
//   - `NaN` / `Infinity` / `-Infinity` 三个裸记号（`json.dumps` 默认 `allow_nan=True`）；
//   - 数字部分用**最短可往返**十进制（与 Go 的 `-1` 精度一致，两者都取最短）；
//   - `decpt`（小数点前的位数）满足 `decpt <= -4 || decpt > 16` 时用指数形；
//   - 指数至少两位、带显式符号（`1e+30`、`5e-324`）；
//   - 其余用定点，整数值补 `.0`（`1000.0`）。
func formatPyFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	neg := math.Signbit(f)
	if f == 0 {
		if neg {
			return "-0.0"
		}
		return "0.0"
	}
	// 先取绝对值的最短往返指数形，例如 "1.5e+30" / "5e-324" / "3.14159265358979e+00"。
	s := strconv.FormatFloat(math.Abs(f), 'e', -1, 64)
	mant, expStr, ok := strings.Cut(s, "e")
	if !ok {
		return s // 理论上不会发生
	}
	exp, err := strconv.Atoi(expStr)
	if err != nil {
		return s
	}
	digits := strings.Replace(mant, ".", "", 1)
	decpt := exp + 1

	var out string
	switch {
	case decpt <= -4 || decpt > 16:
		m := digits[:1]
		if len(digits) > 1 {
			m += "." + digits[1:]
		}
		sign := "+"
		e := exp
		if e < 0 {
			sign, e = "-", -e
		}
		es := strconv.Itoa(e)
		if len(es) < 2 {
			es = "0" + es
		}
		out = m + "e" + sign + es
	case decpt <= 0:
		out = "0." + strings.Repeat("0", -decpt) + digits
	case decpt >= len(digits):
		out = digits + strings.Repeat("0", decpt-len(digits)) + ".0"
	default:
		out = digits[:decpt] + "." + digits[decpt:]
	}
	if neg {
		out = "-" + out
	}
	return out
}

// Marshal 按 Python `json.dumps(..., ensure_ascii=False)` 的形态编码。
func (v *Value) Marshal() []byte { return v.appendTo(nil) }

func (v *Value) appendTo(dst []byte) []byte {
	if v == nil {
		return append(dst, "null"...)
	}
	switch v.kind {
	case Null:
		return append(dst, "null"...)
	case Bool:
		if v.b {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case Number:
		return append(dst, v.num...)
	case String:
		return appendPyString(dst, v.str)
	case Array:
		dst = append(dst, '[')
		for i, e := range v.arr {
			if i > 0 {
				dst = append(dst, ',', ' ')
			}
			dst = e.appendTo(dst)
		}
		return append(dst, ']')
	case Object:
		dst = append(dst, '{')
		for i, k := range v.keys {
			if i > 0 {
				dst = append(dst, ',', ' ')
			}
			dst = appendPyString(dst, k)
			dst = append(dst, ':', ' ')
			dst = v.obj[k].appendTo(dst)
		}
		return append(dst, '}')
	}
	return dst
}

const hexDigits = "0123456789abcdef"

// appendPyString 按 Python `ensure_ascii=False` 的规则写一个 JSON 字符串。
//
// **只**转义 `"` `\` 与 C 风格控制字符（`\b \f \n \r \t`，其余 < 0x20 用 `\u00XX`）。
// `< > &`、非 ASCII、U+2028/U+2029 一律原样 —— 这正是与 Go `encoding/json`
// 的关键差异（Go 会转义 `< > &` 与 U+2028/U+2029）。
func appendPyString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for _, r := range s {
		switch r {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			if r < 0x20 {
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[(r>>4)&0xf], hexDigits[r&0xf])
			} else {
				dst = utf8.AppendRune(dst, r)
			}
		}
	}
	return append(dst, '"')
}
