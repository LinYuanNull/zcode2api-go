// Package claim 套餐领取：定时窗口触发与手动通路。
//
// # 语义来源
//
// 三条路由的**已采样**分支都只依赖账号池里的**已存状态**，不出站
// （`docs/contract/outbound-admin/observations.md` 第五节：实测 `claim/preview`、
// `claim`、`claim/manual` 三条全部零出站）：
//
//   - 候选账号 = JWT 账号（样本 notes：「无 JWT 账号时 preview/outcomes 为空」）。
//
//   - 凭据失效（`status=invalid`）分支有**逐字节样本**（旁录
//     `outbound-admin/fixtures/admin-responses.json` 的 `claim/preview` 与 `claim`）。
//     形状是（为可读性折行，实为一行）：
//
//     preview：`{"preview":[{"account_id":…,"account_name":…,"plans":[],"error":…,`
//     `"activated":false,"activation_error":null}]}`
//
//     claim/manual：`{"outcomes":[{"account_id":…,"account_name":…,"ok":false,`
//     `"message":…}],"summary":{"ok":0,"fail":1}}`
//
//   - `POST /admin/api/claim/manual` 的两条校验分支有样本：
//     缺 `account_id` → 400 `{"detail":"缺少 account_id"}`；
//     账号不存在或非 JWT → 404 `{"detail":"JWT 账号不存在"}`。
//
// # 未覆盖（不凭猜测补全）
//
// **领取成功路径**（`status=active` 的账号）属未覆盖：observations.md 第九节第 4 条，
// 且第一节的上游端点表里**根本没有领取端点** —— 连往哪发都不知道。所以那里一律
// 走 `ErrUnsampled` 明确报错，不按近似形状「试一下」。
package claim

import (
	"errors"

	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/models"
)

// ErrUpstreamUnavailable 表示领取需要上游调用（+ 验证码求解），本阶段未实现。
var ErrUpstreamUnavailable = errors.New("套餐领取需要上游调用与验证码求解（属于 A5/A6 阶段）")

// ErrUnsampled 表示**该分支的响应结构没有样本**，因此不实现、不猜测。
//
// 目前只有一条：`status=active` 账号的真实领取（含验证码换票、`plans` 填充、
// `activation_error` 的取值）。依据 observations.md 第九节第 4 条。
var ErrUnsampled = errors.New("领取成功路径未采样（上游领取端点、验证码换票、plans 填充均无样本）")

// ErrNotJWT 表示目标账号不存在或不是 JWT 账号。
//
// 由 `Service.Manual` 做**防御性**校验时返回；管理 API 层已按样本先回 404
// （`16-claim-manual-404.POST.json`），正常路径到不了这里。
var ErrNotJWT = errors.New("JWT 账号不存在")

// Plan 是可领套餐的一项。
type Plan struct {
	Name   string `json:"name"`
	Grants int64  `json:"grants,omitempty"`
	EndsAt any    `json:"ends_at,omitempty"`
}

// PreviewRow 是 `GET /admin/api/claim/preview` 里的一行。
//
// 字段名与顺序取自旁录样本（逐字）：
// `{account_id, account_name, plans, error, activated, activation_error}`。
//
// ⚠️ **`ActivationError` 必须是指针且不带 `omitempty`**：样本里失效账号那一行是
// `"activation_error":null` —— 键**在**且值为 `null`。写成 `string` + `omitempty`
// 会在空值时把整个键省掉，形态与样本不符。
//
// ⚠️ `plans` 同理必须是**非 nil 空切片**（`[]`），不能是 `null`。
//
// 非失效（即 `active`）的行未采样 ⇒ 本包不产出（见 `ErrUnsampled`）。
type PreviewRow struct {
	AccountID       string  `json:"account_id"`
	AccountName     string  `json:"account_name"`
	Plans           []Plan  `json:"plans"`
	Error           string  `json:"error,omitempty"`
	Activated       bool    `json:"activated"`
	ActivationError *string `json:"activation_error"`
}

// Outcome 是 `POST /admin/api/claim` 与 `claim/manual` 里的一条结果。
//
// 字段名与顺序取自 sample notes
// （`14-claim-empty.POST.json`：`{account_id, account_name, ok, plan_name?,
// grants?, message?, code?, next_at?}`）。
//
// 失效分支的实际产出只需 `account_id, account_name, ok, message` 四个键
// （其余零值被 `omitempty` 省掉），与旁录样本逐字一致。
type Outcome struct {
	AccountID   string `json:"account_id"`
	AccountName string `json:"account_name"`
	OK          bool   `json:"ok"`
	PlanName    string `json:"plan_name,omitempty"`
	Grants      int64  `json:"grants,omitempty"`
	Message     string `json:"message,omitempty"`
	Code        int64  `json:"code,omitempty"`
	NextAt      string `json:"next_at,omitempty"`
}

// Claimer 执行领取。
type Claimer interface {
	Preview() ([]PreviewRow, error)
	Claim(accountIDs []string) ([]Outcome, error)
	Manual(accountID, captchaVerifyParam string) (Outcome, error)
}

// Unavailable 是 Claimer 的占位实现：明确报错，不伪造领取结果。
//
// 生产路径已改接 `Service`（`server.New` 默认装配）；本类型保留给
// **只装配 adminapi 的测试**用，以及作为「没有账号池」时的显式降级。
type Unavailable struct{}

// Preview 实现 Claimer。
func (Unavailable) Preview() ([]PreviewRow, error) { return nil, ErrUpstreamUnavailable }

// Claim 实现 Claimer。
func (Unavailable) Claim([]string) ([]Outcome, error) { return nil, ErrUpstreamUnavailable }

// Manual 实现 Claimer。
func (Unavailable) Manual(string, string) (Outcome, error) {
	return Outcome{}, ErrUpstreamUnavailable
}

// Candidates 返回可参与领取的账号（JWT 账号）。
//
// 依据：样本 notes 反复以「无 JWT 账号时为空」解释空结果
// （`13-claim-preview-empty` / `14-claim-empty`），且 `10-account-refresh-nonjwt`
// 证实 apiKey 账号不走 Coding Plan 通路。
func Candidates(accounts []models.Account) []models.Account {
	out := make([]models.Account, 0, len(accounts))
	for _, a := range accounts {
		if a.Mode == constants.ModeJWT {
			out = append(out, a)
		}
	}
	return out
}

// FindJWT 按 id 找一个 JWT 账号（手动领取用）。
func FindJWT(accounts []models.Account, id string) (models.Account, bool) {
	for _, a := range accounts {
		if a.ID == id && a.Mode == constants.ModeJWT {
			return a, true
		}
	}
	return models.Account{}, false
}

// invalidPreviewRow 产出失效账号的预览行（旁录样本的单行形状）。
func invalidPreviewRow(a models.Account) PreviewRow {
	return PreviewRow{
		AccountID:   a.ID,
		AccountName: a.Name,
		Plans:       []Plan{},
		Error:       constants.MsgInvalidCredential,
		Activated:   false,
	}
}

// invalidOutcome 产出失效账号的领取结果（旁录样本的单条形状）。
func invalidOutcome(a models.Account) Outcome {
	return Outcome{
		AccountID:   a.ID,
		AccountName: a.Name,
		OK:          false,
		Message:     constants.MsgInvalidCredential,
	}
}
