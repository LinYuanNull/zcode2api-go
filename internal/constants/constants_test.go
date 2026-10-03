package constants_test

import (
	"strings"
	"testing"

	"github.com/LinYuanNull/zcode2api-go/internal/constants"
)

// probeString 复现 A2 采样时的可复现输入，用于逐字复算 observations.md #6/#7 的样例。
func probeString(n int) string {
	base := strings.Repeat("abcdefghij", (n/10)+1)
	return base[:n]
}

// TestProvidersOrder 枚举顺序即对外 `providers` 字段的顺序（observations.md #1/#17）。
func TestProvidersOrder(t *testing.T) {
	want := []string{"zai", "bigmodel"}
	if len(constants.Providers) != len(want) {
		t.Fatalf("provider 数量应为 %d，实际 %d", len(want), len(constants.Providers))
	}
	for i := range want {
		if constants.Providers[i] != want[i] {
			t.Errorf("provider[%d] = %q，期望 %q", i, constants.Providers[i], want[i])
		}
	}
	for _, p := range want {
		if !constants.IsProvider(p) {
			t.Errorf("IsProvider(%q) 应为 true", p)
		}
	}
	for _, p := range []string{"ZAI", "BigModel", "zhipu", "glm", "openai", "gemini", "deepseek", ""} {
		if constants.IsProvider(p) {
			t.Errorf("IsProvider(%q) 应为 false（大小写敏感、且只有两个）", p)
		}
	}
}

// TestMaskToken 对齐 observations.md #6：len ≤ 16 原样；否则 前8 + '…' + 后6。
func TestMaskToken(t *testing.T) {
	cases := []struct{ n int }{{4}, {8}, {12}, {15}, {16}}
	for _, c := range cases {
		s := probeString(c.n)
		if got := constants.MaskToken(s); got != s {
			t.Errorf("MaskToken(len=%d) = %q，应原样返回 %q", c.n, got, s)
		}
	}
	masked := []struct {
		n    int
		want string
	}{
		{17, "abcdefgh…bcdefg"},
		{20, "abcdefgh…efghij"},
		{64, "abcdefgh…ijabcd"},
	}
	for _, c := range masked {
		if got := constants.MaskToken(probeString(c.n)); got != c.want {
			t.Errorf("MaskToken(len=%d) = %q，期望 %q", c.n, got, c.want)
		}
	}
	if got := constants.MaskToken(""); got != "" {
		t.Errorf("MaskToken(\"\") = %q，期望空串", got)
	}
}

// TestMaskSecret 对齐 observations.md #7：空串→""；len ≤ 8→`••••`；否则 前4 + '…' + 后4。
func TestMaskSecret(t *testing.T) {
	if got := constants.MaskSecret(""); got != "" {
		t.Errorf("MaskSecret(\"\") = %q，期望空串", got)
	}
	for _, n := range []int{1, 2, 4, 7, 8} {
		if got := constants.MaskSecret(probeString(n)); got != "••••" {
			t.Errorf("MaskSecret(len=%d) = %q，期望 ••••", n, got)
		}
	}
	if got := constants.MaskSecret(probeString(9)); got != "abcd…fghi" {
		t.Errorf("MaskSecret(len=9) = %q，期望 abcd…fghi", got)
	}
	// 夹具里的真实取值：admin_key="1234"（len 4）与 gateway_key="fixture-gw-key-abcdef"（len 23）。
	if got := constants.MaskSecret("1234"); got != "••••" {
		t.Errorf("MaskSecret(\"1234\") = %q，期望 ••••", got)
	}
	if got := constants.MaskSecret("fixture-gw-key-abcdef"); got != "fixt…cdef" {
		t.Errorf("MaskSecret(夹具 gateway_key) = %q，期望 fixt…cdef", got)
	}
}

// TestDefaults 对齐 observations.md #12。
func TestDefaults(t *testing.T) {
	// ⚠️ `admin_key` 没有常量默认值 —— 它是运行期配置（`ZCODE_ADMIN_KEY`）。
	// A3 的对照实验推翻了 A2 的「字面量 1234」假设，详见 observations.md #8。
	if constants.FallbackAdminKey != "zcode" {
		t.Errorf("FallbackAdminKey = %q，期望 zcode", constants.FallbackAdminKey)
	}
	if constants.DefaultGatewayKey != "" {
		t.Errorf("DefaultGatewayKey = %q，期望空串", constants.DefaultGatewayKey)
	}
	// ⚠️ claim_round_interval 是 3600，**不是 0** —— A2 把这份部署 `.env` 的
	// `ZCODE_CLAIM_ROUND_INTERVAL=0` 当成了默认值。A3 移走 `.env` 后实测为 3600。
	if constants.DefaultQuotaRefreshInterval != 1800 ||
		constants.DefaultAccountConcurrency != 2 ||
		constants.DefaultClaimRoundInterval != 3600 {
		t.Errorf("整数默认值不符: %d/%d/%d",
			constants.DefaultQuotaRefreshInterval, constants.DefaultAccountConcurrency, constants.DefaultClaimRoundInterval)
	}
}
