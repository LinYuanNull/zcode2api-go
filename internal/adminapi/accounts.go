package adminapi

import (
	"net/http"
	"strings"

	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/httpx"
	"github.com/LinYuanNull/zcode2api-go/internal/models"
	"github.com/LinYuanNull/zcode2api-go/internal/quota"
)

// ── GET /admin/api/accounts ─────────────────────────────────
//
// 键顺序取自样本 `02-accounts-empty|one.GET.json`：accounts, stats, providers, ts。
type accountsListResponse struct {
	Accounts  []models.PublicView `json:"accounts"`
	Stats     models.Stats        `json:"stats"`
	Providers []string            `json:"providers"`
	TS        float64             `json:"ts"`
}

func (a *API) handleAccountsList(w http.ResponseWriter, _ *http.Request) {
	accts := a.d.Store.List()
	views := make([]models.PublicView, 0, len(accts))
	for _, x := range accts {
		views = append(views, x.View())
	}
	httpx.WriteJSON(w, http.StatusOK, accountsListResponse{
		Accounts:  views,
		Stats:     models.ComputeStats(accts),
		Providers: constants.Providers,
		TS:        a.d.Now(),
	})
}

// ── POST /admin/api/accounts ────────────────────────────────
//
// 请求体 `{name?, provider, tokens[]}`；成功体键顺序 `count, ids`。
type accountsAddRequest struct {
	Name     string   `json:"name"`
	Provider string   `json:"provider"`
	Tokens   []string `json:"tokens"`
}

type accountsAddResponse struct {
	Count int64    `json:"count"`
	IDs   []string `json:"ids"`
}

func (a *API) handleAccountsAdd(w http.ResponseWriter, r *http.Request) {
	var req accountsAddRequest
	if !a.readJSON(w, r, &req) {
		return
	}
	// 校验顺序按样本：`04-accounts-add-badprovider` 的 tokens 是合法的，
	// `04-accounts-add-notokens` 的 provider 是合法的 —— 两条互斥，顺序不影响结论，
	// 但 provider 先查更贴近「先看能不能收这个 provider」的语义。
	if !constants.IsProvider(req.Provider) {
		httpx.WriteDetail(w, http.StatusBadRequest, "不支持的 provider")
		return
	}
	tokens := make([]string, 0, len(req.Tokens))
	for _, t := range req.Tokens {
		if s := strings.TrimSpace(t); s != "" {
			tokens = append(tokens, s)
		}
	}
	if len(tokens) == 0 {
		httpx.WriteDetail(w, http.StatusBadRequest, "请输入至少一个 Token / API Key")
		return
	}

	ids := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		// 去重键 = (provider, 凭据)；命中则返回既有 id 且**不改 name**。
		// 依据：docs/contract/store/observations.md #5。
		if exist, ok := a.d.Store.FindByCredential(req.Provider, tok); ok {
			ids = append(ids, exist.ID)
			continue
		}
		acc, err := models.NewAPIKeyAccount(
			req.Provider, req.Name, tok,
			a.d.Store.CountByProvider(req.Provider),
			a.d.Fp.Next(), a.d.Now(),
		)
		if err != nil {
			httpx.WriteDetail(w, http.StatusBadRequest, err.Error())
			return
		}
		// 粘贴 JWT 与粘贴 API Key 走同一个入口，mode 由凭据形态决定。
		applyCredential(&acc, tok)
		if err := a.d.Store.Put(acc); err != nil {
			httpx.WriteDetail(w, http.StatusInternalServerError, err.Error())
			return
		}
		ids = append(ids, acc.ID)
	}
	httpx.WriteJSON(w, http.StatusOK, accountsAddResponse{Count: int64(len(ids)), IDs: ids})
}

// ── DELETE /admin/api/accounts ──────────────────────────────
//
// 请求体是**顶层 JSON 数组**（不是对象，也不是 query），元素为账号 id。
// 依据：`05-accounts-delete-ok|none.DELETE.json` 的 request.body 是数组。
type accountsDeleteResponse struct {
	Deleted int64 `json:"deleted"`
}

func (a *API) handleAccountsDelete(w http.ResponseWriter, r *http.Request) {
	var ids []string
	if !a.readJSON(w, r, &ids) {
		return
	}
	var n int64
	for _, id := range ids {
		ok, err := a.d.Store.Delete(id)
		if err != nil {
			httpx.WriteDetail(w, http.StatusInternalServerError, err.Error())
			return
		}
		if ok {
			n++
		}
	}
	httpx.WriteJSON(w, http.StatusOK, accountsDeleteResponse{Deleted: n})
}

// ── PUT /admin/api/accounts/{account_id} ────────────────────
//
// 成功体 `{ok:true}`；账号不存在 → 404 `{"detail":"账号不存在"}`。
//
// 支持改 `name` 与 `token`。`token` 这一项**不是**从 A1 样本推出来的
// （样本只发了 `{name}`），而是 ModelMux 面板的编辑弹窗里有「新的 Token」输入框
// 所必需；`mode` 随凭据形态重判（三段点分 → jwt，否则 apiKey）。
// 这两点属**推断**，已登记在 PROVENANCE.md。
type accountEditRequest struct {
	Name  *string `json:"name"`
	Token *string `json:"token"`
}

func (a *API) handleAccountEdit(w http.ResponseWriter, r *http.Request) {
	var req accountEditRequest
	if !a.readJSON(w, r, &req) {
		return
	}
	acc, ok := a.accountOr404(w, r)
	if !ok {
		return
	}
	if req.Name != nil {
		if name := strings.TrimSpace(*req.Name); name != "" {
			acc.Name = name
		}
	}
	if req.Token != nil {
		if tok := strings.TrimSpace(*req.Token); tok != "" {
			applyCredential(&acc, tok)
		}
	}
	if err := a.d.Store.Put(acc); err != nil {
		httpx.WriteDetail(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, okTrue)
}

// ── POST /admin/api/accounts/{account_id}/enabled ───────────
type accountEnabledRequest struct {
	Enabled *bool `json:"enabled"`
}

func (a *API) handleAccountEnabled(w http.ResponseWriter, r *http.Request) {
	var req accountEnabledRequest
	if !a.readJSON(w, r, &req) {
		return
	}
	acc, ok := a.accountOr404(w, r)
	if !ok {
		return
	}
	if req.Enabled != nil {
		// enabled 与 status 联动。依据：observations.md #13。
		acc.Enabled = *req.Enabled
		if acc.Enabled {
			acc.Status = constants.StatusActive
		} else {
			acc.Status = constants.StatusDisabled
		}
	}
	if err := a.d.Store.Put(acc); err != nil {
		httpx.WriteDetail(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, okTrue)
}

// ── POST /admin/api/accounts/{account_id}/fingerprint/rotate ─
//
// 成功体键顺序取自样本 `08-account-fingerprint-ok.POST.json`：ok, fingerprint。
type fingerprintRotateResponse struct {
	OK          bool               `json:"ok"`
	Fingerprint models.Fingerprint `json:"fingerprint"`
}

func (a *API) handleFingerprintRotate(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.accountOr404(w, r)
	if !ok {
		return
	}
	acc.Fingerprint = a.d.Fp.Next()
	if err := a.d.Store.Put(acc); err != nil {
		httpx.WriteDetail(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, fingerprintRotateResponse{OK: true, Fingerprint: acc.Fingerprint})
}

// ── POST /admin/api/accounts/refresh ────────────────────────
//
// 键顺序取自样本 `09-accounts-refresh-all.POST.json`：summary, count, skipped_cooling, skipped_invalid。
type refreshAllResponse struct {
	Summary        okFail `json:"summary"`
	Count          int64  `json:"count"`
	SkippedCooling int64  `json:"skipped_cooling"`
	SkippedInvalid int64  `json:"skipped_invalid"`
}

type refreshAllRequest struct {
	All *bool `json:"all"`
}

func (a *API) handleAccountsRefreshAll(w http.ResponseWriter, r *http.Request) {
	var req refreshAllRequest
	if !a.readJSON(w, r, &req) {
		return
	}
	var (
		resp       refreshAllResponse
		candidates []models.Account
	)
	for _, acc := range a.d.Store.List() {
		switch acc.Status {
		case constants.StatusCooling:
			resp.SkippedCooling++
			continue
		case constants.StatusInvalid:
			resp.SkippedInvalid++
			continue
		}
		// 只有 JWT 账号能查额度（observations 与样本 `10-...-nonjwt` 一致）；
		// apiKey 账号既不计入 count 也不计入 skipped_*。
		if quota.SupportsRefresh(acc) {
			candidates = append(candidates, acc)
		}
	}
	if len(candidates) > 0 {
		// 空池分支已由样本证实（全 0）；有候选时必须打上游，属 A5。
		notImplemented(w, "全量额度刷新需要上游调用（A5）")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// ── POST /admin/api/accounts/{account_id}/refresh ───────────
//
// 非 JWT 分支键顺序取自样本 `10-account-refresh-nonjwt.POST.json`：ok, message。
type accountRefreshResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

func (a *API) handleAccountRefresh(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.accountOr404(w, r)
	if !ok {
		return
	}
	if !quota.SupportsRefresh(acc) {
		// 业务失败走 200 + ok:false（样本明确：HTTP 状态码不能当成败判据）。
		httpx.WriteJSON(w, http.StatusOK, accountRefreshResponse{
			OK: false, Message: quota.NonJWTMessages,
		})
		return
	}
	res, err := a.d.Quota.Refresh(acc)
	if err != nil {
		notImplemented(w, "JWT 账号额度查询需要上游调用（A5）")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, accountRefreshResponse{OK: res.OK, Message: res.Message})
}

// ── 工具 ────────────────────────────────────────────────────

// accountOr404 取路径里的账号；不存在时写 404 并返回 false。
func (a *API) accountOr404(w http.ResponseWriter, r *http.Request) (models.Account, bool) {
	acc, ok := a.d.Store.Get(r.PathValue("account_id"))
	if !ok {
		httpx.WriteDetail(w, http.StatusNotFound, "账号不存在")
		return models.Account{}, false
	}
	return acc, true
}

// applyCredential 用凭据设置账号的 mode 与对应字段。
//
// 三条路径共用这一处：新增（粘贴）、导入（导出回灌）、编辑（换 Token）。
// **不能各写各的** —— 判据一旦漂移，同一个 token 会因入口不同而变成不同 mode，
// 表现为面板上 JWT 账号的「刷新额度 / 领取」按钮时有时无。
//
// 判据见 detectMode（仅 zai 且恰好 2 个点 → jwt）。
func applyCredential(acc *models.Account, token string) {
	if detectMode(acc.Provider, token) == constants.ModeJWT {
		acc.Mode = constants.ModeJWT
		acc.JWTToken = &token
		acc.APIKey = nil
		return
	}
	acc.Mode = constants.ModeAPIKey
	acc.APIKey = &token
	acc.JWTToken = nil
}
