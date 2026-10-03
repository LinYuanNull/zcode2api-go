// Package claim 套餐领取：定时窗口触发与手动通路。
//
// A3 只定义**接缝与可证实的语义**，真正的领取要打上游 + 解验证码，属 A5/A6：
//
//   - 候选账号 = JWT 账号（样本 notes：「无 JWT 账号时 preview/outcomes 为空」）。
//   - `POST /admin/api/claim/manual` 的两条校验分支有样本：
//     缺 `account_id` → 400 `{"detail":"缺少 account_id"}`；
//     账号不存在或非 JWT → 404 `{"detail":"JWT 账号不存在"}`。
//   - 成功分支**无样本**（SPEC.md 异常点：claim/manual 无成功样本），故不实现、
//     不猜响应体 —— 走 `ErrUpstreamUnavailable` 明确报错。
package claim

import (
	"errors"

	"github.com/LinYuanNull/zcode2api-go/internal/models"
)

// ErrUpstreamUnavailable 表示领取需要上游调用（+ 验证码求解），本阶段未实现。
var ErrUpstreamUnavailable = errors.New("套餐领取需要上游调用与验证码求解（属于 A5/A6 阶段）")

// Plan 是可领套餐的一项。
type Plan struct {
	Name   string `json:"name"`
	Grants int64  `json:"grants,omitempty"`
	EndsAt any    `json:"ends_at,omitempty"`
}

// PreviewRow 是 `GET /admin/api/claim/preview` 里的一行。
//
// 字段名与顺序取自样本 notes：「有账号时每项含
// {account_id, account_name, plans[], error, activated, activation_error}」。
// ⚠️ 非空结构**未采样**（样本是空数组），顺序按 notes 的书写序，待补采校准。
type PreviewRow struct {
	AccountID       string `json:"account_id"`
	AccountName     string `json:"account_name"`
	Plans           []Plan `json:"plans"`
	Error           string `json:"error,omitempty"`
	Activated       bool   `json:"activated"`
	ActivationError string `json:"activation_error,omitempty"`
}

// Outcome 是 `POST /admin/api/claim` 里的一条结果。
//
// 字段名取自样本 notes：`{account_id, account_name, ok, plan_name?, grants?, message?, code?, next_at?}`。
// ⚠️ 非空结构**未采样**。
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
		if a.Mode == "jwt" {
			out = append(out, a)
		}
	}
	return out
}

// FindJWT 按 id 找一个 JWT 账号（手动领取用）。
func FindJWT(accounts []models.Account, id string) (models.Account, bool) {
	for _, a := range accounts {
		if a.ID == id && a.Mode == "jwt" {
			return a, true
		}
	}
	return models.Account{}, false
}
