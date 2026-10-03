package models

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/LinYuanNull/zcode2api-go/internal/constants"
)

// ExtraField 保存 `data` 里本实现不认识的键，用于「上游加字段不被抹掉」。
type ExtraField struct {
	Key string
	Raw json.RawMessage
}

// Account 是 `accounts` 表一行的类型化视图（含 `data` blob 的全部字段）。
//
// **字段声明顺序 = 落盘 JSON 键顺序**，见 docs/contract/store/account-data-shape.json。
// 这不是风格问题：A2 的验收要求与靶机产出「逐字段一致」，而键顺序是文件字节层面
// 可观测的事实（observations.md 第 2 节）。改动顺序前请先读那份文档。
type Account struct {
	ID            string
	Name          string
	Provider      string
	Mode          string
	JWTToken      *string
	APIKey        *string
	Enabled       bool
	Status        string
	Quota         json.RawMessage
	Plan          json.RawMessage
	Plans         json.RawMessage
	Usage         json.RawMessage
	UseCount      int64
	FailCount     int64
	RiskStrikes   int64
	LastRiskAt    *float64
	RecentResults json.RawMessage
	LastUsedAt    *float64
	LastCheckedAt *float64
	CoolingUntil  *float64
	LastError     *string
	CreatedAt     float64
	Fingerprint   Fingerprint
	InstallID     string
	InstalledAt   *float64

	// Extra 是 `data` 里未识别的键，按原始相对顺序保存；编码时追加在已知字段之后。
	Extra []ExtraField
}

// dataFieldOrder 是已知字段的落盘顺序（供文档与测试引用）。
var dataFieldOrder = []string{
	"id", "name", "provider", "mode", "jwt_token", "api_key", "enabled", "status",
	"quota", "plan", "plans", "usage", "use_count", "fail_count", "risk_strikes",
	"last_risk_at", "recent_results", "last_used_at", "last_checked_at",
	"cooling_until", "last_error", "created_at", "fingerprint", "install_id", "installed_at",
}

// DataFieldOrder 返回 `data` blob 的已知字段顺序（副本，调用方可安全修改）。
func DataFieldOrder() []string {
	out := make([]string, len(dataFieldOrder))
	copy(out, dataFieldOrder)
	return out
}

// UnmarshalJSON 按 key 顺序解码，未识别的键收进 Extra。
func (a *Account) UnmarshalJSON(b []byte) error {
	o, err := parseOrderedObject(b)
	if err != nil {
		return err
	}
	*a = Account{}
	for _, k := range o.keys {
		raw := o.vals[k]
		var derr error
		switch k {
		case "id":
			derr = decodeField(raw, &a.ID)
		case "name":
			derr = decodeField(raw, &a.Name)
		case "provider":
			derr = decodeField(raw, &a.Provider)
		case "mode":
			derr = decodeField(raw, &a.Mode)
		case "jwt_token":
			a.JWTToken, derr = decodeOptString(raw)
		case "api_key":
			a.APIKey, derr = decodeOptString(raw)
		case "enabled":
			derr = decodeField(raw, &a.Enabled)
		case "status":
			derr = decodeField(raw, &a.Status)
		case "quota":
			a.Quota = append(json.RawMessage(nil), raw...)
		case "plan":
			a.Plan = append(json.RawMessage(nil), raw...)
		case "plans":
			a.Plans = append(json.RawMessage(nil), raw...)
		case "usage":
			a.Usage = append(json.RawMessage(nil), raw...)
		case "use_count":
			derr = decodeField(raw, &a.UseCount)
		case "fail_count":
			derr = decodeField(raw, &a.FailCount)
		case "risk_strikes":
			derr = decodeField(raw, &a.RiskStrikes)
		case "last_risk_at":
			a.LastRiskAt, derr = decodeOptFloat(raw)
		case "recent_results":
			a.RecentResults = append(json.RawMessage(nil), raw...)
		case "last_used_at":
			a.LastUsedAt, derr = decodeOptFloat(raw)
		case "last_checked_at":
			a.LastCheckedAt, derr = decodeOptFloat(raw)
		case "cooling_until":
			a.CoolingUntil, derr = decodeOptFloat(raw)
		case "last_error":
			a.LastError, derr = decodeOptString(raw)
		case "created_at":
			derr = decodeField(raw, &a.CreatedAt)
		case "fingerprint":
			derr = decodeField(raw, &a.Fingerprint)
		case "install_id":
			derr = decodeField(raw, &a.InstallID)
		case "installed_at":
			a.InstalledAt, derr = decodeOptFloat(raw)
		default:
			a.Extra = append(a.Extra, ExtraField{Key: k, Raw: append(json.RawMessage(nil), raw...)})
		}
		if derr != nil {
			return fmt.Errorf("data 字段 %q: %w", k, derr)
		}
	}
	return nil
}

// MarshalJSON 按落盘顺序编码；未知字段追加在末尾。
func (a Account) MarshalJSON() ([]byte, error) {
	o := NewOrderedObject()
	fpRaw, err := a.Fingerprint.Bytes()
	if err != nil {
		return nil, err
	}
	fields := []struct {
		key string
		val any
	}{
		{"id", a.ID},
		{"name", a.Name},
		{"provider", a.Provider},
		{"mode", a.Mode},
		{"jwt_token", a.JWTToken},
		{"api_key", a.APIKey},
		{"enabled", a.Enabled},
		{"status", a.Status},
		{"quota", rawOr(a.Quota, "{}")},
		{"plan", rawOr(a.Plan, "{}")},
		{"plans", rawOr(a.Plans, "[]")},
		{"usage", rawOr(a.Usage, "{}")},
		{"use_count", a.UseCount},
		{"fail_count", a.FailCount},
		{"risk_strikes", a.RiskStrikes},
		{"last_risk_at", a.LastRiskAt},
		{"recent_results", rawOr(a.RecentResults, "[]")},
		{"last_used_at", a.LastUsedAt},
		{"last_checked_at", a.LastCheckedAt},
		{"cooling_until", a.CoolingUntil},
		{"last_error", a.LastError},
		{"created_at", a.CreatedAt},
		{"fingerprint", json.RawMessage(fpRaw)},
		{"install_id", a.InstallID},
		{"installed_at", a.InstalledAt},
	}
	for _, f := range fields {
		if err := o.Set(f.key, f.val); err != nil {
			return nil, err
		}
	}
	for _, e := range a.Extra {
		o.SetRaw(e.Key, e.Raw)
	}
	return o.Bytes()
}

func rawOr(r json.RawMessage, def string) json.RawMessage {
	if len(r) == 0 {
		return json.RawMessage(def)
	}
	return r
}

// Credential 返回账号当前生效的凭据明文（apiKey 模式取 api_key，jwt 模式取 jwt_token）。
func (a Account) Credential() string {
	switch a.Mode {
	case constants.ModeJWT:
		if a.JWTToken != nil {
			return *a.JWTToken
		}
	default:
		if a.APIKey != nil {
			return *a.APIKey
		}
	}
	return ""
}

// MaskedToken 返回对外展示用的掩码凭据。依据：observations.md #6。
func (a Account) MaskedToken() string { return constants.MaskToken(a.Credential()) }

// Usable 报告账号当前是否**可参与网关调度**（启用且状态为 active）。
//
// 两处消费它：`GET /admin/api/status` 的 `quota_pool` 计数，以及网关的
// 「无可用账号」判定。判定口径统一放这里，避免两处漂移。
func (a Account) Usable() bool {
	return a.Enabled && a.Status == constants.StatusActive
}

// PublicView 是对外暴露的账号视图（`GET /admin/api/accounts` 的 `accounts[]` 元素）。
//
// 字段顺序同样是实测事实（observations.md 未列全，取自 A2 采样时靶机的原始响应序）。
// 注意它**不含** `usage` / `last_risk_at` / 明文凭据。
type PublicView struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Provider      string          `json:"provider"`
	Mode          string          `json:"mode"`
	TokenMasked   string          `json:"token_masked"`
	Enabled       bool            `json:"enabled"`
	Status        string          `json:"status"`
	Quota         json.RawMessage `json:"quota"`
	Plan          json.RawMessage `json:"plan"`
	Plans         json.RawMessage `json:"plans"`
	UseCount      int64           `json:"use_count"`
	FailCount     int64           `json:"fail_count"`
	RiskStrikes   int64           `json:"risk_strikes"`
	RecentResults json.RawMessage `json:"recent_results"`
	LastUsedAt    *float64        `json:"last_used_at"`
	LastCheckedAt *float64        `json:"last_checked_at"`
	CoolingUntil  *float64        `json:"cooling_until"`
	LastError     *string         `json:"last_error"`
	CreatedAt     float64         `json:"created_at"`
	Fingerprint   Fingerprint     `json:"fingerprint"`
	InstallID     string          `json:"install_id"`
	InstalledAt   *float64        `json:"installed_at"`
}

// View 投影出对外视图。
func (a Account) View() PublicView {
	return PublicView{
		ID:            a.ID,
		Name:          a.Name,
		Provider:      a.Provider,
		Mode:          a.Mode,
		TokenMasked:   a.MaskedToken(),
		Enabled:       a.Enabled,
		Status:        a.Status,
		Quota:         rawOr(a.Quota, "{}"),
		Plan:          rawOr(a.Plan, "{}"),
		Plans:         rawOr(a.Plans, "[]"),
		UseCount:      a.UseCount,
		FailCount:     a.FailCount,
		RiskStrikes:   a.RiskStrikes,
		RecentResults: rawOr(a.RecentResults, "[]"),
		LastUsedAt:    a.LastUsedAt,
		LastCheckedAt: a.LastCheckedAt,
		CoolingUntil:  a.CoolingUntil,
		LastError:     a.LastError,
		CreatedAt:     a.CreatedAt,
		Fingerprint:   a.Fingerprint,
		InstallID:     a.InstallID,
		InstalledAt:   a.InstalledAt,
	}
}

// Slug 把显示名转成 id 前缀。规则见 observations.md #3：
// 转小写 → 逐字符「字母数字保留、否则替换为 '-'」→ 合并连续 '-' → 去首尾 '-' → 截断 32 字符。
//
// 注意判据是 **Unicode 的「字母 / 数字」**，不是 ASCII 白名单 —— 实测 `账号测试` 与
// `Ünïcödé` 都被原样保留，用 `[^a-z0-9]` 会得到错误结果。
func Slug(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if r := []rune(s); len(r) > constants.SlugMaxLen {
		s = string(r[:constants.SlugMaxLen])
	}
	return strings.Trim(s, "-")
}

// DefaultName 在用户没给名字时生成默认显示名：`<provider>-<该 provider 现有账号数 + 1>`。
// 依据：observations.md #4（按 provider 计数，不是全库计数）。
func DefaultName(provider string, existingForProvider int) string {
	return fmt.Sprintf("%s-%d", provider, existingForProvider+1)
}

// NewAccountID 拼出账号 id：`<slug>-<8 位小写 hex>`。依据：observations.md #2。
func NewAccountID(slug string, suffix string) string {
	return slug + "-" + suffix
}
