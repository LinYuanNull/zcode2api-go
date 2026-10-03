package settings_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/settings"
)

// memMeta 是 MetaStore 的内存实现，让单测不碰文件。
type memMeta struct{ m map[string]string }

func newMemMeta() *memMeta { return &memMeta{m: map[string]string{}} }

func (f *memMeta) GetMeta(key string) (string, bool, error) {
	v, ok := f.m[key]
	return v, ok, nil
}

func (f *memMeta) SetMeta(key, value string) error {
	f.m[key] = value
	return nil
}

func raw(s string) *json.RawMessage {
	r := json.RawMessage(s)
	return &r
}

// TestCoerceInt 对齐 observations.md #10 的每一条实测样例。
func TestCoerceInt(t *testing.T) {
	ok := []struct {
		in   string
		want int64
	}{
		{"-5", 0},                        // 负数钳到 0
		{"3.7", 3},                       // 浮点向零截断
		{"1.9", 1},                       //
		{"-0.5", 0},                      // 负浮点也钳到 0
		{"true", 1},                      // 布尔
		{"false", 0},                     //
		{"1000000000000", 1000000000000}, // 无上界
		{"1800", 1800},                   //
		{"0", 0},                         //
		{"3.0", 3},                       //
	}
	for _, c := range ok {
		got, err := settings.CoerceInt(json.RawMessage(c.in))
		if err != nil {
			t.Errorf("CoerceInt(%s) 意外报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("CoerceInt(%s) = %d，期望 %d", c.in, got, c.want)
		}
	}

	// 靶机对 string / null 返回 400，而不是「归一成 0」—— 这个区分必须保留。
	for _, bad := range []string{`"abc"`, `null`, ``, `[1]`, `{}`, `"3"`} {
		if _, err := settings.CoerceInt(json.RawMessage(bad)); !errors.Is(err, settings.ErrNotANumber) {
			t.Errorf("CoerceInt(%s) 应返回 ErrNotANumber，实际 %v", bad, err)
		}
	}
}

// TestApplyPartialWrite 对齐 observations.md #11：只写请求里出现的键。
func TestApplyPartialWrite(t *testing.T) {
	ms := newMemMeta()
	if _, err := settings.Apply(ms, settings.Patch{}, cfg("1234")); err != nil {
		t.Fatalf("空 Patch 应无副作用: %v", err)
	}
	if len(ms.m) != 0 {
		t.Fatalf("空 Patch 不应写入任何键，实际 %v", ms.m)
	}

	// 先落一次全量，再只改一项。
	if _, err := settings.Apply(ms, settings.Patch{
		AdminKey:             strPtr("1234"),
		GatewayKey:           strPtr("gw-key-abcdef"),
		QuotaRefreshInterval: raw("1800"),
		AccountConcurrency:   raw("2"),
		ClaimRoundInterval:   raw("0"),
	}, cfg("1234")); err != nil {
		t.Fatalf("全量写失败: %v", err)
	}
	s, err := settings.Apply(ms, settings.Patch{AccountConcurrency: raw("7")}, cfg("1234"))
	if err != nil {
		t.Fatalf("部分写失败: %v", err)
	}
	if s.AccountConcurrency != 7 {
		t.Errorf("account_concurrency 应为 7，实际 %d", s.AccountConcurrency)
	}
	if s.QuotaRefreshInterval != 1800 {
		t.Errorf("未出现的键被改动了: quota_refresh_interval = %d", s.QuotaRefreshInterval)
	}
	if s.GatewayKey != "gw-key-abcdef" {
		t.Errorf("未出现的键被改动了: gateway_key = %q", s.GatewayKey)
	}
}

// TestApplyAdminKeyEmpty 对齐 observations.md #9：不允许置空，且原值不动。
func TestApplyAdminKeyEmpty(t *testing.T) {
	ms := newMemMeta()
	if _, err := settings.Apply(ms, settings.Patch{AdminKey: strPtr("keep-me")}, cfg("")); err != nil {
		t.Fatalf("写入 admin_key 失败: %v", err)
	}
	_, err := settings.Apply(ms, settings.Patch{AdminKey: strPtr("")}, cfg(""))
	if !errors.Is(err, settings.ErrAdminKeyEmpty) {
		t.Fatalf("置空 admin_key 应返回 ErrAdminKeyEmpty，实际 %v", err)
	}
	if ms.m[constants.MetaAdminKey] != "keep-me" {
		t.Errorf("置空失败时原值不应被改动，实际 %q", ms.m[constants.MetaAdminKey])
	}
}

// TestApplyIntRejectsString 设置项为字符串 / null 时报错（observations.md #10）。
func TestApplyIntRejectsString(t *testing.T) {
	ms := newMemMeta()
	if _, err := settings.Apply(ms, settings.Patch{QuotaRefreshInterval: raw(`"abc"`)}, cfg("")); err == nil {
		t.Fatal("字符串值应报错")
	}
	if _, err := settings.Apply(ms, settings.Patch{QuotaRefreshInterval: raw(`null`)}, cfg("")); err == nil {
		t.Fatal("null 应报错")
	}
}

// TestAdminKeyIsDefaultUsesConfiguredDefault 是 A3 对照实验的固化。
//
// 实验（临时实例，全新数据目录，`ZCODE_ADMIN_KEY=9999`）：
//
//	admin_key=9999 → admin_key_is_default=true
//	admin_key=1234 → admin_key_is_default=false   ← 推翻「字面量 1234」假设
//	admin_key=9999 → admin_key_is_default=true
//
// 即比较基准是**配置给的默认值**，不是常量。A2 的 `constants.DefaultAdminKey`
// 正是被这一条推翻的。
func TestAdminKeyIsDefaultUsesConfiguredDefault(t *testing.T) {
	ms := newMemMeta()
	// 配置默认 = 9999，库里存 9999 → 是默认。
	if _, err := settings.Apply(ms, settings.Patch{AdminKey: strPtr("9999")}, cfg("9999")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	s, err := settings.Load(ms, cfg("9999"))
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !s.View().AdminKeyIsDefault {
		t.Errorf("admin_key=9999 / 配置默认=9999 应 is_default=true，实际 %+v", s.View())
	}

	// 改成 1234（**曾经被误当成常量默认值**）→ 不是默认。
	if _, err := settings.Apply(ms, settings.Patch{AdminKey: strPtr("1234")}, cfg("9999")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	s, err = settings.Load(ms, cfg("9999"))
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if s.View().AdminKeyIsDefault {
		t.Errorf("admin_key=1234 / 配置默认=9999 应 is_default=false，实际 %+v", s.View())
	}

	// 未配置（空串）时基准回落到 FallbackAdminKey。
	if _, err := settings.Apply(ms, settings.Patch{AdminKey: strPtr(constants.FallbackAdminKey)}, cfg("")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	s, err = settings.Load(ms, cfg(""))
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !s.View().AdminKeyIsDefault {
		t.Errorf("未配置时基准应为 %q，实际 %+v", constants.FallbackAdminKey, s.View())
	}
}

// TestDefaultsAndView 对齐 observations.md #12 / #7 / #8。
func TestDefaultsAndView(t *testing.T) {
	d := settings.Defaults(cfg("1234"))
	if d.AdminKey != "1234" || d.GatewayKey != "" ||
		d.QuotaRefreshInterval != 1800 || d.AccountConcurrency != 2 || d.ClaimRoundInterval != 3600 {
		t.Fatalf("默认值不符: %+v", d)
	}
	v := d.View()
	if !v.AdminKeySet || !v.AdminKeyIsDefault {
		t.Errorf("默认 admin_key 应 set=true / is_default=true，实际 %+v", v)
	}
	if v.AdminKeyMasked != "••••" {
		t.Errorf("默认 admin_key 掩码应为 ••••，实际 %q", v.AdminKeyMasked)
	}
	if v.GatewayKeySet || v.GatewayKeyMasked != "" {
		t.Errorf("空 gateway_key 应 set=false / masked=\"\"，实际 %+v", v)
	}
	if v.QuotaRefreshInterval != 1800 || v.AccountConcurrency != 2 || v.ClaimRoundInterval != 3600 {
		t.Errorf("视图整数项不符: %+v", v)
	}

	// 未配置（空串）时，默认密码回落到 FallbackAdminKey 且仍算「默认」。
	d = settings.Defaults(cfg(""))
	if d.AdminKey != constants.FallbackAdminKey || !d.View().AdminKeyIsDefault {
		t.Errorf("未配置时应回落到 %q 且 is_default=true，实际 %+v", constants.FallbackAdminKey, d)
	}
}

// TestLoadFallsBackToDefaults 库里缺键时回落到默认值。
func TestLoadFallsBackToDefaults(t *testing.T) {
	ms := newMemMeta()
	s, err := settings.Load(ms, cfg("1234"))
	if err != nil {
		t.Fatalf("空库读取失败: %v", err)
	}
	if s != settings.Defaults(cfg("1234")) {
		t.Errorf("空库应回落到默认值，实际 %+v", s)
	}
}

// TestLoadRejectsUnparsableInt 库里的整数项不可解析时**不猜、不静默改写**，直接报错。
func TestLoadRejectsUnparsableInt(t *testing.T) {
	ms := newMemMeta()
	ms.m[constants.MetaQuotaRefreshInterval] = "not-a-number"
	if _, err := settings.Load(ms, cfg("1234")); err == nil {
		t.Fatal("不可解析的整数项应报错")
	}
}

func strPtr(s string) *string { return &s }

// cfg 是「只配置后台密码」的简写，其余四项取 constants 回落值。
func cfg(adminKey string) settings.Configured {
	return settings.ConfiguredWithAdminKey(adminKey)
}
