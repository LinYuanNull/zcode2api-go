package pyjson_test

import (
	"testing"

	"github.com/LinYuanNull/zcode2api-go/internal/pyjson"
)

// 数字规范化表：**期望值全部来自 CPython 复算**（`json.dumps(json.loads(x))`），
// 见 docs/contract/outbound/fixtures/serialization-probes.json 的 python_reference_rules。
//
// 这张表是「数字必须按 Python 类型往返」的判据：`1e3`→`1000.0`（float）、
// `100`→`100`（int）、`-0`→`0`（int 归一）、`1e16`→`1e+16`（指数阈值）。
func TestNumberNormalizationMatchesCPython(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1e3", "1000.0"},
		{"1.0", "1.0"},
		{"100", "100"},
		{"-0", "0"},
		{"0", "0"},
		{"1e15", "1000000000000000.0"},
		{"1e16", "1e+16"},
		{"1e17", "1e+17"},
		{"1e-4", "0.0001"},
		{"1e-5", "1e-05"},
		{"1e30", "1e+30"},
		{"1.5e30", "1.5e+30"},
		{"0.1", "0.1"},
		{"3.14159265358979", "3.14159265358979"},
		{"123456789012345678901234567890", "123456789012345678901234567890"},
		{"1.7976931348623157e308", "1.7976931348623157e+308"},
		{"5e-324", "5e-324"},
		{"-0.0", "-0.0"},
		{"2.5", "2.5"},
		{"1E+2", "100.0"},
	}
	for _, c := range cases {
		v, err := pyjson.Parse([]byte(c.in))
		if err != nil {
			t.Fatalf("Parse(%q) 失败: %v", c.in, err)
		}
		if got := string(v.Marshal()); got != c.want {
			t.Errorf("%s → %s，期望 %s", c.in, got, c.want)
		}
	}
}

// 大整数必须**任意精度**：用 float64 承载会毁掉它（这是 pyjson 存在的理由之一）。
func TestBigIntegerIsExact(t *testing.T) {
	const big = "123456789012345678901234567890123456789"
	v, err := pyjson.Parse([]byte(big))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(v.Marshal()); got != big {
		t.Fatalf("大整数被改写：%s", got)
	}
}

// 端到端形态判据：`pyjson.Parse(x).Marshal()` 必须等于 CPython 的
// `json.dumps(json.loads(x), ensure_ascii=False)`。下面每条 want 都是 CPython 现算的。
//
// 注意这里**不含** `messages[].content` 的改写（那是 bodytransform 的职责），
// 只判「保序 + 分隔符 + 数字 + 转义」四件事。
func TestMatchesCPythonDumps(t *testing.T) {
	cases := []struct{ id, in, want string }{
		{
			"compact-oddorder",
			`{"messages":[{"content":"hi","role":"user"}],"model":"GLM-5.3","max_tokens":16,"metadata":{"a":1e3,"b":1.0,"c":100,"d":"中文"}}`,
			`{"messages": [{"content": "hi", "role": "user"}], "model": "GLM-5.3", "max_tokens": 16, "metadata": {"a": 1000.0, "b": 1.0, "c": 100, "d": "中文"}}`,
		},
		{
			"array-whitespace",
			`{"model":"GLM-5.3","messages":[{"role":"user","content":[ { "type" : "text" , "text" : "hi" } ]}],"max_tokens":1}`,
			`{"model": "GLM-5.3", "messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}], "max_tokens": 1}`,
		},
		{
			"toplevel-pretty",
			"{\n  \"model\" : \"GLM-5.3\",\n  \"max_tokens\" : 2,\n  \"messages\" : [ {\"role\":\"user\",\"content\":\"hi\"} ]\n}",
			`{"model": "GLM-5.3", "max_tokens": 2, "messages": [{"role": "user", "content": "hi"}]}`,
		},
		{
			"html-escape",
			`{"model":"GLM-5.3","max_tokens":1,"messages":[{"role":"user","content":"a<b>&c \u0041"}]}`,
			`{"model": "GLM-5.3", "max_tokens": 1, "messages": [{"role": "user", "content": "a<b>&c A"}]}`,
		},
		{
			"empty-containers",
			`{"a":{},"b":[]}`,
			`{"a": {}, "b": []}`,
		},
		{
			"nums",
			`{"a":1e3,"b":-0,"c":100,"d":1e16,"e":5e-324,"f":-0.0,"g":1E+2}`,
			`{"a": 1000.0, "b": 0, "c": 100, "d": 1e+16, "e": 5e-324, "f": -0.0, "g": 100.0}`,
		},
	}
	for _, c := range cases {
		v, err := pyjson.Parse([]byte(c.in))
		if err != nil {
			t.Fatalf("%s: Parse 失败: %v", c.id, err)
		}
		if got := string(v.Marshal()); got != c.want {
			t.Errorf("%s 形态不符：\n got=%s\nwant=%s", c.id, got, c.want)
		}
	}
}

// 字符串转义：Python `ensure_ascii=False` 只转义 `"` `\` 与控制字符。
// `< > &` 与 U+2028 必须**原样**（Go 的 encoding/json 会转义它们，这是差异点）。
func TestStringEscapingMatchesPython(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"a<b>&c"`, `"a<b>&c"`},
		{`"\u0041"`, `"A"`},
		{`"\/"`, `"/"`},
		{`"\n"`, `"\n"`},
		{`"\t"`, `"\t"`},
		{`"\u0001"`, `"\u0001"`},
		{`"\u2028"`, "\"\u2028\""},
		{`"\ud83d\ude00"`, `"😀"`},
		{`"中文"`, `"中文"`},
		{`"\\"`, `"\\"`},
		{`"\""`, `"\""`},
		{`"\b\f\r"`, `"\b\f\r"`},
	}
	for _, c := range cases {
		v, err := pyjson.Parse([]byte(c.in))
		if err != nil {
			t.Fatalf("Parse(%s) 失败: %v", c.in, err)
		}
		if got := string(v.Marshal()); got != c.want {
			t.Errorf("%s → %s，期望 %s", c.in, got, c.want)
		}
	}
}

// 键序保留。
func TestKeyOrderPreserved(t *testing.T) {
	v, err := pyjson.Parse([]byte(`{"zeta":1,"alpha":2,"mid":{},"list":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"zeta": 1, "alpha": 2, "mid": {}, "list": []}`
	if got := string(v.Marshal()); got != want {
		t.Fatalf("键序不符：\n got=%s\nwant=%s", got, want)
	}
}

// 重复键：Python dict 取**最后**的值、保留**首次**出现的位置。
func TestDuplicateKeysFollowPython(t *testing.T) {
	v, err := pyjson.Parse([]byte(`{"a":1,"b":2,"a":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(v.Marshal()), `{"a": 3, "b": 2}`; got != want {
		t.Fatalf("重复键语义不符：got=%s want=%s", got, want)
	}
}

// 非有限数用裸记号（json.dumps 默认 allow_nan=True）。
func TestNonFiniteNumbers(t *testing.T) {
	v, err := pyjson.Parse([]byte(`{"a":1e400,"b":-1e400}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(v.Marshal()), `{"a": Infinity, "b": -Infinity}`; got != want {
		t.Fatalf("非有限数形态不符：got=%s want=%s", got, want)
	}
}

// Set 必须**原位替换**（保持首次位置），否则键序契约会被改值操作破坏。
func TestSetKeepsPosition(t *testing.T) {
	v, err := pyjson.Parse([]byte(`{"a":1,"b":"x","c":3}`))
	if err != nil {
		t.Fatal(err)
	}
	v.Set("b", pyjson.NewString("y"))
	if got, want := string(v.Marshal()), `{"a": 1, "b": "y", "c": 3}`; got != want {
		t.Fatalf("Set 改动了位置：got=%s want=%s", got, want)
	}
	// 新键追加到末尾
	v.Set("d", pyjson.NewArray(pyjson.NewString("t")))
	if got, want := string(v.Marshal()), `{"a": 1, "b": "y", "c": 3, "d": ["t"]}`; got != want {
		t.Fatalf("新键未追加到末尾：got=%s want=%s", got, want)
	}
}

// 尾部多余内容必须报错（不能让「半个 JSON」静默通过）。
func TestRejectsTrailingGarbage(t *testing.T) {
	if _, err := pyjson.Parse([]byte(`{"a":1} garbage`)); err == nil {
		t.Fatal("尾部多余内容应报错")
	}
}
