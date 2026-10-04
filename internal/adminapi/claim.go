package adminapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/LinYuanNull/zcode2api-go/internal/captcha"
	"github.com/LinYuanNull/zcode2api-go/internal/claim"
	"github.com/LinYuanNull/zcode2api-go/internal/httpx"
)

// ── GET /admin/api/claim/preview ────────────────────────────
//
// 空池 → `{"preview":[]}`（样本 `13-claim-preview-empty.GET.json`）。
// 池里有 JWT 账号时由 `claim.Service` 产出：失效账号走**有样本**的分支
// （旁录 `claim/preview`），`active` 账号走 `ErrUnsampled` ⇒ 501。
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
		writeClaimError(w, err)
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
		writeClaimError(w, err)
		return
	}
	if outcomes == nil {
		outcomes = []claim.Outcome{}
	}
	httpx.WriteJSON(w, http.StatusOK, claimEnvelope(outcomes))
}

// claimEnvelope 按样本装配 `{outcomes, summary}`（`claim` 与 `claim/manual` 同形）。
//
// `summary` 是**逐条数出来的**，不是另算的：旁录样本里
// `[ok:false] × 1 → {"ok":0,"fail":1}`，与计数一致。
func claimEnvelope(outcomes []claim.Outcome) claimResponse {
	var sum okFail
	for _, o := range outcomes {
		if o.OK {
			sum.OK++
		} else {
			sum.Fail++
		}
	}
	return claimResponse{Outcomes: outcomes, Summary: sum}
}

// writeClaimError 把领取的错误分派成两种状态码（口径同 `writeQuotaError`）。
//
//   - 「未采样 / 没有上游」⇒ **501**：`ErrUnsampled`（分支无样本）、
//     `ErrUpstreamUnavailable`（占位实现）、`ErrNotJWT`（防御性，正常路径已被
//     前面的 404 拦下）。本项目的纪律是未实现的分支明确报错，绝不伪造成功。
//   - 其余（落库失败之类的内部错）⇒ **502**。
func writeClaimError(w http.ResponseWriter, err error) {
	if errors.Is(err, claim.ErrUnsampled) ||
		errors.Is(err, claim.ErrUpstreamUnavailable) ||
		errors.Is(err, claim.ErrNotJWT) {
		notImplemented(w, err.Error())
		return
	}
	httpx.WriteDetail(w, http.StatusBadGateway, "领取失败: "+err.Error())
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
// 三条分支：
//
//  1. 缺 `account_id` → 400 `{"detail":"缺少 account_id"}`
//     （样本 `16-claim-manual-missing-id.POST.json`）—— 在调用 Claimer **之前**判。
//  2. 账号不存在或非 JWT → 404 `{"detail":"JWT 账号不存在"}`
//     （样本 `16-claim-manual-404.POST.json`）—— 同样在之前判，与实现无关。
//  3. 其余 → 交给 Claimer；回执与 `POST /admin/api/claim` **同形**
//     （旁录 `claim/manual` 与 `claim` 逐字一致），所以复用同一个信封。
//
// ⚠️ SPEC.md 把 `claim/manual` 记成「无成功样本」—— 那指的是**真的领到**的成功体
// （`plans` 填充那一类）。旁录里的失效回执（200 + `ok:false`）是有样本的，
// 它也是 200，所以不能因为「没有成功样本」就把这条路由整体 501。
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
	out, err := a.d.Claimer.Manual(id, req.CaptchaVerifyParam)
	if err != nil {
		writeClaimError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, claimEnvelope([]claim.Outcome{out}))
}
