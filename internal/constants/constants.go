// Package constants 汇总从靶机可观测行为中固化的枚举、默认值与阈值。
//
// 依据：docs/contract/store/observations.md（A2 落盘采样）与 docs/contract/admin/*.json（A1 HTTP 采样）。
// 本包只放**结构性事实** —— 枚举取值、默认值、阈值；不放任何上游文案，也不放业务逻辑。
package constants

// ProviderZAI 是唯一支持 JWT（Coding Plan）凭据的 provider。
//
// 依据：实测（SPEC.md「补充实测」）—— 同一串 JWT 交给 bigmodel 会被存成 apiKey，
// 只有 zai 会判成 jwt。所以 provider 是 mode 判定的硬条件，不是修饰。
const ProviderZAI = "zai"

// 支持的 provider 枚举。**顺序即对外 `providers` 字段的顺序**（枚举声明序，不是字典序）。
// 依据：observations.md #1（大小写敏感，只有这两个）与 #17。
var Providers = []string{ProviderZAI, "bigmodel"}

// IsProvider 判断 provider 是否在枚举内。
func IsProvider(p string) bool {
	for _, v := range Providers {
		if v == p {
			return true
		}
	}
	return false
}

// 账号凭据形态。
//
// 判据见 `adminapi.detectMode`：**仅 zai 且凭据恰好含 2 个点** 才是 jwt。
// （早先以为「新增接口固定产出 apiKey」是错的 —— 那是只采到一条无点 token 造成的
// 假象，已用运行中的靶机逐条探测推翻，见 SPEC.md「补充实测」。）
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

// 账号级展示文案。**集中在这里是为了防两处字面量漂移** ——
// 同一句话同时出现在额度刷新与领取两个包里，各自写一份迟早会改岔。
const (
	// MsgInvalidCredential 是「凭据失效」的统一文案。
	//
	// 逐字取自 `docs/contract/outbound-admin/fixtures/admin-responses.json`，
	// 四处**都是同一句**：`accounts/refresh` 的 FRESH/CACHED 两形态、
	// `claim/preview` 的 `error`、`claim`（含 `claim/manual`）的 `outcomes[].message`。
	//
	// ⚠️ 这是**网关自己**的话，不是上游的原话：额度查询失败时上游回的是
	// `404 page not found`（usage）或**空体**（billing 的 401）—— 见 observations.md 4.3。
	// 所以不能把上游错误体直接当这句话用。
	MsgInvalidCredential = "凭证失效，请重新授权"
)

// 设置项默认值。依据：observations.md #12。
//
// ⚠️ `admin_key` **没有常量默认值** —— 它是**运行期配置**（`ZCODE_ADMIN_KEY`，
// 见 `settings.Configured`）。
// A2 曾把采样时 `.env` 的取值 `"1234"` 记成常量，A3 的对照实验证明那是错的：
// 靶机的 `admin_key_is_default` 比的是**配置给的默认值**，不是字面量
// （`ZCODE_ADMIN_KEY=9999` 时，`admin_key=9999` → true、`admin_key=1234` → **false**）。
//
// ⚠️ 下面四项里**除 gateway_key 外**都能被同名环境变量覆盖为「首启默认值」
// （实测：`ZCODE_QUOTA_REFRESH_INTERVAL=111` / `ZCODE_ACCOUNT_CONCURRENCY=7` /
// `ZCODE_CLAIM_ROUND_INTERVAL=222` 在全新数据目录上分别落成 111 / 7 / 222）。
// 这里的常量是**环境变量也没给**时的回落值。
const (
	// FallbackAdminKey 是**完全未配置**时的回落值。
	// 依据：把 `.env` 移走、不设 `ZCODE_ADMIN_KEY` 起靶机，用 `Bearer zcode` 得 200，
	// 而 `1234` / `admin` / `changeme` / `password` 全部 401
	// ⇒ 未配置时的默认密码是 `zcode`（可观察行为，非源码）。
	FallbackAdminKey = "zcode"

	// DefaultGatewayKey 是网关 Key 的默认值（空 = 不校验）。
	//
	// ⚠️ **没有对应的环境变量**：`ZCODE_GATEWAY_KEY` 在 os.environ、`.env`、
	// 以及运行期鉴权三条路径上**实测都不生效**（三条独立探测，见 observations.md #12）。
	// 网关 Key 只能经 `PUT /admin/api/settings` 设置。
	DefaultGatewayKey = ""

	// DefaultQuotaRefreshInterval 是 `ZCODE_QUOTA_REFRESH_INTERVAL` 未设时的首启值。
	DefaultQuotaRefreshInterval = 1800

	// DefaultAccountConcurrency 是 `ZCODE_ACCOUNT_CONCURRENCY` 未设时的首启值。
	DefaultAccountConcurrency = 2

	// DefaultClaimRoundInterval 是 3600，**不是 0**。
	//
	// A2 记成 0，是把这份部署 `.env` 里的 `ZCODE_CLAIM_ROUND_INTERVAL=0` 当成了默认值。
	// A3 实测：移走 `.env`、不设该环境变量，全新数据目录首启得到 **3600**；
	// 设成 222 则得到 222。⇒ 3600 是代码回落值，0 只是配置取值。
	DefaultClaimRoundInterval = 3600
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
