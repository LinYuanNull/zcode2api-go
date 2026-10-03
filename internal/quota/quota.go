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
// 注意「业务失败」用 `OK=false` + `Message` 表达，**HTTP 仍是 200** ——
// 这是样本明确的事实（`10-account-refresh-nonjwt.POST.json` 是 200 + `ok:false`）。
type Result struct {
	OK      bool
	Message string
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
