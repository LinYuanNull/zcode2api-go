package adminapi

import (
	"net/http"
	"strings"

	"github.com/LinYuanNull/zcode2api-go/internal/captcha"
	"github.com/LinYuanNull/zcode2api-go/internal/claim"
	"github.com/LinYuanNull/zcode2api-go/internal/httpx"
)

// ── GET /admin/api/claim/preview ────────────────────────────
//
// 空池 → `{"preview":[]}`（样本 `13-claim-preview-empty.GET.json`）。
// 有 JWT 账号时必须打上游 → A5；这里显式报错。
type claimPreviewResponse struct {
	Preview []claim.PreviewRow `json:"preview"`
}

func (a *API) handleClaimPreview(w http.ResponseWriter, _ *http.Request) {
	if len(claim.Candidates(a.d.Store.List())) == 0 {
		httpx.WriteJSON(w, http.StatusOK, claimPreviewResponse{Preview: []claim.PreviewRow{}})
		return
	}
	rows, err := a.d.Claimer.Preview()
	if err != nil {
		notImplemented(w, "领取预览需要上游调用（A5）")
		return
	}
	if rows == nil {
		rows = []claim.PreviewRow{}
	}
	httpx.WriteJSON(w, http.StatusOK, claimPreviewResponse{Preview: rows})
}

// ── POST /admin/api/claim ───────────────────────────────────
//
// 键顺序取自样本 `14-claim-empty.POST.json`：outcomes, summary。
type claimRequest struct {
	AccountIDs []string `json:"account_ids"`
}

type claimResponse struct {
	Outcomes []claim.Outcome `json:"outcomes"`
	Summary  okFail          `json:"summary"`
}

func (a *API) handleClaim(w http.ResponseWriter, r *http.Request) {
	var req claimRequest
	if !a.readJSON(w, r, &req) {
		return
	}
	if len(claim.Candidates(a.d.Store.List())) == 0 {
		httpx.WriteJSON(w, http.StatusOK, claimResponse{
			Outcomes: []claim.Outcome{},
			Summary:  okFail{},
		})
		return
	}
	outcomes, err := a.d.Claimer.Claim(req.AccountIDs)
	if err != nil {
		notImplemented(w, "套餐领取需要上游调用与验证码求解（A5/A6）")
		return
	}
	if outcomes == nil {
		outcomes = []claim.Outcome{}
	}
	var sum okFail
	for _, o := range outcomes {
		if o.OK {
			sum.OK++
		} else {
			sum.Fail++
		}
	}
	httpx.WriteJSON(w, http.StatusOK, claimResponse{Outcomes: outcomes, Summary: sum})
}

// ── GET /admin/api/claim/captcha-config ─────────────────────
//
// 响应体就是 captcha.Config（键顺序 enabled, scene_id, region, prefix，取自样本）。
func (a *API) handleCaptchaConfig(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, a.d.Captcha.Config())
}

// 确保 captcha.Config 仍与样本同形（字段被误改时这里会编译失败，
// 比在测试里晚发现更早）。
var _ = captcha.Config{}

// ── POST /admin/api/claim/manual ────────────────────────────
//
// 两条校验分支都有样本：
//   - 缺 `account_id` → 400 `{"detail":"缺少 account_id"}`
//     （`16-claim-manual-missing-id.POST.json`）
//   - 账号不存在或非 JWT → 404 `{"detail":"JWT 账号不存在"}`
//     （`16-claim-manual-404.POST.json`）
//
// 成功分支**无样本**（SPEC.md 异常点：claim/manual 无成功样本），因此不实现、
// 不猜响应体。
type claimManualRequest struct {
	AccountID          string `json:"account_id"`
	CaptchaVerifyParam string `json:"captcha_verify_param"`
}

func (a *API) handleClaimManual(w http.ResponseWriter, r *http.Request) {
	var req claimManualRequest
	if !a.readJSON(w, r, &req) {
		return
	}
	id := strings.TrimSpace(req.AccountID)
	if id == "" {
		httpx.WriteDetail(w, http.StatusBadRequest, "缺少 account_id")
		return
	}
	if _, ok := claim.FindJWT(a.d.Store.List(), id); !ok {
		httpx.WriteDetail(w, http.StatusNotFound, "JWT 账号不存在")
		return
	}
	if _, err := a.d.Claimer.Manual(id, req.CaptchaVerifyParam); err != nil {
		notImplemented(w, "手动领取需要上游调用（A5）")
		return
	}
	// 上面必然返回错误（A3 的 Claimer 是 Unavailable），此处置于不可达；
	// 保留显式 501 以防将来接上实现后忘了补响应装配。
	notImplemented(w, "手动领取的成功响应体无样本可依（待补采）")
}
