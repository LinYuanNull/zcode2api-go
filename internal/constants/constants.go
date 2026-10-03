// Package constants 汇总从靶机可观测行为中固化的枚举、默认值与阈值。
//
// 依据：docs/contract/store/observations.md（A2 落盘采样）与 docs/contract/admin/*.json（A1 HTTP 采样）。
// 本包只放**结构性事实** —— 枚举取值、默认值、阈值；不放任何上游文案，也不放业务逻辑。
package constants

// 支持的 provider 枚举。**顺序即对外 `providers` 字段的顺序**（枚举声明序，不是字典序）。
// 依据：observations.md #1（大小写敏感，只有这两个）与 #17。
var Providers = []string{"zai", "bigmodel"}

// IsProvider 判断 provider 是否在枚举内。
func IsProvider(p string) bool {
	for _, v := range Providers {
		if v == p {
			return true
		}
	}
	return false
}

// 账号凭据形态。依据：observations.md #14（新增接口固定产出 apiKey）与 #4 未覆盖分支。
const (
	ModeAPIKey = "apiKey"
	ModeJWT    = "jwt"
)

// 账号状态枚举。
//
// `active` / `disabled` 已实测（observations.md #13）；`cooling` / `exhausted` / `invalid`
// 由 `stats` 的键名登记而来，**转移条件尚未采样**（见 #4 未覆盖分支）。
const (
	StatusActive    = "active"
	StatusDisabled  = "disabled"
	StatusCooling   = "cooling"
	StatusExhausted = "exhausted"
	StatusInvalid   = "invalid"
)

// 设置项默认值。依据：observations.md #12。
const (
	DefaultAdminKey             = "1234"
	DefaultGatewayKey           = ""
	DefaultQuotaRefreshInterval = 1800
	DefaultAccountConcurrency   = 2
	DefaultClaimRoundInterval   = 0
)

// meta 表的键名。依据：schema.sql。
const (
	MetaAdminKey             = "admin_key"
	MetaGatewayKey           = "gateway_key"
	MetaQuotaRefreshInterval = "quota_refresh_interval"
	MetaAccountConcurrency   = "account_concurrency"
	MetaClaimRoundInterval   = "claim_round_interval"
)

// 账号 id 的 slug 最大长度。依据：observations.md #3（`a`×80 → `a`×32）。
const SlugMaxLen = 32

// 掩码阈值。依据：observations.md #6（token）与 #7（密钥）。
const (
	// TokenMaskThreshold 以下（含）原样返回；超过则 head…tail。
	TokenMaskThreshold = 16
	TokenMaskHead      = 8
	TokenMaskTail      = 6

	// KeyMaskThreshold 以下（含）整体打点；超过则 head…tail。空串单独返回空串。
	KeyMaskThreshold = 8
	KeyMaskHead      = 4
	KeyMaskTail      = 4
	KeyMaskDots      = "••••"
)

// MonitoringKeep 是请求监控环形的容量。依据：docs/contract/admin/21-monitoring.GET.json。
const MonitoringKeep = 500

// Ellipsis 是掩码里用的省略号（U+2026），与靶机一致。
const Ellipsis = "…"

// MaskToken 按靶机规则掩码账号凭据（observations.md #6）。
func MaskToken(s string) string {
	if len(s) <= TokenMaskThreshold {
		return s
	}
	return s[:TokenMaskHead] + Ellipsis + s[len(s)-TokenMaskTail:]
}

// MaskSecret 按靶机规则掩码密钥（observations.md #7）。
func MaskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= KeyMaskThreshold {
		return KeyMaskDots
	}
	return s[:KeyMaskHead] + Ellipsis + s[len(s)-KeyMaskTail:]
}
