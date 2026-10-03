package models_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/models"
)

// probeString 复现 A2 采样时用的可复现输入：`abcdefghij` 重复后按长度截断。
// 这样 observations.md #6/#7 里给出的样例才能被逐字复算。
func probeString(n int) string {
	base := strings.Repeat("abcdefghij", (n/10)+1)
	return base[:n]
}

// TestSlugRules 对齐 observations.md #3 的每一条实测样例。
func TestSlugRules(t *testing.T) {
	cases := []struct{ in, want string }{
		{"My Account!", "my-account"},
		{"UPPER Case", "upper-case"},
		{"with_under", "with-under"},
		{"dot.name", "dot-name"},
		{`a/b\c`, "a-b-c"},
		{"t@b#c$d", "t-b-c-d"},
		{"  spaced  ", "spaced"},
		{strings.Repeat("a", 80), strings.Repeat("a", constants.SlugMaxLen)},
		// 判据是 Unicode 的「字母 / 数字」，不是 ASCII 白名单。
		{"账号测试", "账号测试"},
		{"Ünïcödé", "ünïcödé"},
	}
	for _, c := range cases {
		if got := models.Slug(c.in); got != c.want {
			t.Errorf("Slug(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestDefaultNameRules 对齐 observations.md #4：按 **provider** 计数，不是全库计数。
func TestDefaultNameRules(t *testing.T) {
	if got := models.DefaultName("zai", 0); got != "zai-1" {
		t.Errorf("DefaultName(zai, 0) = %q，期望 zai-1", got)
	}
	if got := models.DefaultName("zai", 1); got != "zai-2" {
		t.Errorf("DefaultName(zai, 1) = %q，期望 zai-2", got)
	}
	if got := models.DefaultName("bigmodel", 4); got != "bigmodel-5" {
		t.Errorf("DefaultName(bigmodel, 4) = %q，期望 bigmodel-5", got)
	}
}

// TestNewAccountIDForm 对齐 observations.md #2：`<slug>-<8 位小写 hex>`。
func TestNewAccountIDForm(t *testing.T) {
	a, err := models.NewAPIKeyAccount("zai", "My Account!", "secret-token", 0,
		models.Fingerprint{Platform: "win32"}, 1.0)
	if err != nil {
		t.Fatalf("构造账号失败: %v", err)
	}
	if !strings.HasPrefix(a.ID, "my-account-") {
		t.Errorf("id 前缀应为 slug，实际 %q", a.ID)
	}
	suffix := strings.TrimPrefix(a.ID, "my-account-")
	if len(suffix) != 8 {
		t.Fatalf("id 后缀应为 8 位 hex，实际 %q", suffix)
	}
	for _, r := range suffix {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Errorf("id 后缀含非小写 hex 字符: %q", suffix)
		}
	}
	// 靶机新增接口固定产出 apiKey（observations.md #14）。
	if a.Mode != constants.ModeAPIKey {
		t.Errorf("mode 应为 apiKey，实际 %q", a.Mode)
	}
	if a.Enabled != true || a.Status != constants.StatusActive {
		t.Errorf("新账号应为 enabled/active，实际 %v/%q", a.Enabled, a.Status)
	}
	// 空账号的形态（observations.md #4 未覆盖分支）。
	if string(a.Quota) != "{}" || string(a.Plan) != "{}" || string(a.Plans) != "[]" ||
		string(a.Usage) != "{}" || string(a.RecentResults) != "[]" {
		t.Errorf("空账号的 quota/plan/plans/usage/recent_results 形态不对: %s %s %s %s %s",
			a.Quota, a.Plan, a.Plans, a.Usage, a.RecentResults)
	}
	if a.InstalledAt != nil {
		t.Errorf("新账号 installed_at 应为 null")
	}
}

// TestAccountJSONKeyOrder 锁住落盘键顺序 = DataFieldOrder()。
// 顺序是文件字节层面的事实（observations.md §2），不是风格问题。
func TestAccountJSONKeyOrder(t *testing.T) {
	a, err := models.NewAPIKeyAccount("zai", "order check", "tok", 0, models.Fingerprint{}, 1.0)
	if err != nil {
		t.Fatalf("构造账号失败: %v", err)
	}
	enc, err := models.MarshalNoHTMLEscape(a)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	if got, want := jsonKeyOrder(t, enc), models.DataFieldOrder(); !equalStrings(got, want) {
		t.Errorf("落盘键顺序不一致:\n  实际 %v\n  期望 %v", got, want)
	}
}

// TestFingerprintKeyOrder 锁住指纹内层顺序（observations.md §2 末段）。
func TestFingerprintKeyOrder(t *testing.T) {
	b, err := models.Fingerprint{}.Bytes()
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	want := []string{"platform", "arch", "os_version", "language", "timezone", "screen", "device_mid"}
	if got := jsonKeyOrder(t, b); !equalStrings(got, want) {
		t.Errorf("指纹键顺序不一致:\n  实际 %v\n  期望 %v", got, want)
	}
}

// TestAccountRoundTripKeepsUnknownFields 上游将来加字段时，本实现必须原样带过、不抹掉。
func TestAccountRoundTripKeepsUnknownFields(t *testing.T) {
	raw := []byte(`{"id":"x-00000000","name":"n","provider":"zai","mode":"apiKey",` +
		`"api_key":"t","enabled":true,"status":"active","quota":{},"plan":{},"plans":[],` +
		`"usage":{},"use_count":3,"fail_count":1,"risk_strikes":2,"last_risk_at":null,` +
		`"recent_results":[],"last_used_at":null,"last_checked_at":null,"cooling_until":null,` +
		`"last_error":null,"created_at":1.5,"fingerprint":{"platform":"win32"},` +
		`"install_id":"i","installed_at":null,"future_field":{"a":[1,2,3]},"another_new":"v"}`)
	var a models.Account
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	enc, err := models.MarshalNoHTMLEscape(a)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	keys := jsonKeyOrder(t, enc)
	wantTail := []string{"future_field", "another_new"}
	if len(keys) < 2 || !equalStrings(keys[len(keys)-2:], wantTail) {
		t.Errorf("未知字段应原样追加在已知字段之后，实际键序 %v", keys)
	}
	var back map[string]json.RawMessage
	if err := json.Unmarshal(enc, &back); err != nil {
		t.Fatalf("回解失败: %v", err)
	}
	if string(back["future_field"]) != `{"a":[1,2,3]}` {
		t.Errorf("未知字段的值被改写: %s", back["future_field"])
	}
	if string(back["another_new"]) != `"v"` {
		t.Errorf("未知字段的值被改写: %s", back["another_new"])
	}
	if a.UseCount != 3 || a.FailCount != 1 || a.RiskStrikes != 2 {
		t.Errorf("已知数值字段解错: use=%d fail=%d risk=%d", a.UseCount, a.FailCount, a.RiskStrikes)
	}
}

// TestAccountMarshalNoHTMLEscape 落盘不能把 < > & 转成 \u00xx（与 Python json.dumps 对齐）。
//
// 这条同时是一份「为什么需要 MarshalNoHTMLEscape」的说明书：
// 直接用 json.Marshal 一定会转义，即使 Account 自己的 MarshalJSON 没转义 ——
// 外层还会再压一遍。
func TestAccountMarshalNoHTMLEscape(t *testing.T) {
	a := models.Account{ID: "x", Name: "a<b>c&d", Provider: "zai", Mode: "apiKey"}

	enc, err := models.MarshalNoHTMLEscape(a)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	if !strings.Contains(string(enc), `"name":"a<b>c&d"`) {
		t.Errorf("保真入口把 HTML 字符转义了: %s", enc)
	}

	// 反面对照：标准库入口会转义（这正是必须显式走保真入口的原因）。
	std, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("标准库编码失败: %v", err)
	}
	if strings.Contains(string(std), `"name":"a<b>c&d"`) {
		t.Log("注意：标准库 json.Marshal 不再转义 HTML 字符，本注释与实现可简化")
	}
}

// TestComputeStatsBuckets 状态分桶（observations.md #16 的键序在 store 侧断言）。
func TestComputeStatsBuckets(t *testing.T) {
	accounts := []models.Account{
		{Status: constants.StatusActive, UseCount: 2, FailCount: 1},
		{Status: constants.StatusActive},
		{Status: constants.StatusDisabled},
		{Status: constants.StatusCooling},
		{Status: constants.StatusExhausted},
		{Status: constants.StatusInvalid},
	}
	s := models.ComputeStats(accounts)
	if s.Total != 6 || s.Active != 2 || s.Disabled != 1 || s.Cooling != 1 || s.Exhausted != 1 || s.Invalid != 1 {
		t.Errorf("分桶不正确: %+v", s)
	}
	if s.Calls != 2 || s.Fail != 1 {
		t.Errorf("calls/fail 求和不正确: %+v", s)
	}
}

func jsonKeyOrder(t *testing.T, b []byte) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(b)))
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("读取 JSON 失败: %v", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		t.Fatalf("期望 JSON 对象，实际 %v", tok)
	}
	var keys []string
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			t.Fatalf("读取键失败: %v", err)
		}
		keys = append(keys, kt.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("跳过值失败: %v", err)
		}
	}
	return keys
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
