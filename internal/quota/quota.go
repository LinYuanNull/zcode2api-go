// Package quota 额度与积分：查询、构成、到期分布。
//
// A3 只定义**接缝**：额度刷新要打上游，属 A5。这里给出接口与「未实现」默认实现，
// 让 `POST /admin/api/accounts/refresh` 与 `POST /admin/api/accounts/{id}/refresh`
// 能按样本返回**无需上游**的分支，同时对需要上游的分支**明确报错**而不是伪造结果。
package quota

import (
	"errors"

	"github.com/LinYuanNull/zcode2api-go/internal/models"
)

// ErrUpstreamUnavailable 表示额度刷新需要上游调用，本阶段未实现。
var ErrUpstreamUnavailable = errors.New("额度刷新需要上游调用（属于 A5 阶段）")

// Result 是一次额度刷新的结果。
//
// 注意「业务失败」用 `OK=false` 表达，**HTTP 仍是 200** ——
// 这是样本明确的事实（`10-account-refresh-nonjwt.POST.json` 是 200 + `ok:false`）。
//
// 两种形态靠**键名**区分（实测，outbound-admin 4.5）：
//
//   - FRESH（**真的查了上游**）：`{\"ok\":false,\"result\":{\"error\":…},\"account\":{…}}`
//     ⇒ 置 `Failure`（写进嵌套对象 `result.error`）；
//   - CACHED（**未出站**）：`{\"ok\":false,\"message\":…,\"account\":{…}}`
//     ⇒ 置 `Message`（写进顶层 `message` 字符串）。
//
// 两者的 `account.status` 都可能是 `invalid`，**不能只看状态判别是否出站** ——
// 这也是为什么这个字段不做成「一个字符串 + 一个布尔」。
//
// `Account` 非空时回带 `account` 视图（22 键的 `models.PublicView`）。
type Result struct {
	OK      bool
	Message string
	Failure string

	// Account 是刷新**之后**的账号（可能已被标记 invalid）。由调用方投影成视图。
	// 用值而不是指针之外的额外结构，是为了让「没查到就不回带」这件事显式可见。
	Account *models.Account
}

// Refresher 刷新一个账号的额度。
type Refresher interface {
	Refresh(a models.Account) (Result, error)
}

// Unavailable 是 Refresher 的占位实现。
type Unavailable struct{}

// Refresh 实现 Refresher。
func (Unavailable) Refresh(models.Account) (Result, error) {
	return Result{}, ErrUpstreamUnavailable
}

// SupportsRefresh 报告某账号是否走「上游额度查询」这条路。
//
// 依据：`10-account-refresh-nonjwt.POST.json` —— 非 JWT（apiKey）账号不查上游，
// 直接 200 `{ok:false, message:"仅 Coding Plan (JWT) 账号支持额度查询"}`。
func SupportsRefresh(a models.Account) bool { return a.Mode == "jwt" }

// NonJWTMessages 是给非 JWT 账号的固定说明。
//
// 文案逐字取自样本 `10-account-refresh-nonjwt.POST.json`。
const NonJWTMessages = "仅 Coding Plan (JWT) 账号支持额度查询"

// InvalidCredentialMessage 是「凭据失效」的统一文案。
//
// 逐字取自 `docs/contract/outbound-admin/fixtures/admin-responses.json`：
// `refresh` 的 FRESH/CACHED 两形态、`claim/preview` 的 `error`、
// `claim` 的 `message` 四处**都是同一句**。
//
// ⚠️ 这是**网关自己**的话，不是上游的原话：额度查询失败时上游回的是
// `404 page not found`（usage）或**空体**（billing 的 401）—— 见 observations.md 4.3。
// 所以不能把上游错误体直接当这句话用。
const InvalidCredentialMessage = "凭证失效，请重新授权"
