// Package adminapi 管理 API（22 路由）：路由注册、请求校验与响应装配。
//
// 对应计划中的 A3 阶段。**唯一真源是 `docs/contract/admin/*.json` 的样本**，
// 不是 SPEC.md 里的表格（表格曾按字典序书写，与重采样后的样本不符）。
//
// 三条编码纪律：
//
//  1. **响应一律走 httpx.WriteJSON**（内部是 models.MarshalNoHTMLEscape），
//     否则含 `<`/非 ASCII 的字段会与靶机不同形。
//  2. **struct 字段声明顺序 = 响应键顺序**，按样本逐字声明；不能按字典序重排。
//  3. **空容器写 `[]` / `{}` 而不是 `null`** —— 用 `make([]T, 0)` 与显式默认值。
//
// 边界（A3 的诚实范围）：**不需要上游的分支全部实现**（账号 CRUD、启停、
// 指纹换发、设置读写、导入导出、监控、状态、校验）；**需要上游的分支一律
// 显式报错**（额度刷新、领取、登录发起），由 A5/A6 接上，绝不伪造成功。
package adminapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/LinYuanNull/zcode2api-go/internal/authadmin"
	"github.com/LinYuanNull/zcode2api-go/internal/captcha"
	"github.com/LinYuanNull/zcode2api-go/internal/claim"
	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/fingerprint"
	"github.com/LinYuanNull/zcode2api-go/internal/httpx"
	"github.com/LinYuanNull/zcode2api-go/internal/models"
	"github.com/LinYuanNull/zcode2api-go/internal/oauth"
	"github.com/LinYuanNull/zcode2api-go/internal/quota"
	"github.com/LinYuanNull/zcode2api-go/internal/reqlog"
	"github.com/LinYuanNull/zcode2api-go/internal/settings"
	"github.com/LinYuanNull/zcode2api-go/internal/store"
)

// maxBodyBytes 是请求体上限。管理面请求都很小（最多一次导入几千个 token），
// 1 MiB 足够且能挡住明显的恶意大包。
const maxBodyBytes = 1 << 20

// Deps 是 adminapi 的依赖。除 Store 外都可省略（取默认实现）。
type Deps struct {
	Store *store.Store

	// Guard 是管理面鉴权器。为 nil 时不做鉴权（仅测试用；生产路径由 server 必传）。
	Guard *authadmin.Guard

	Ring    *reqlog.Ring
	Fp      *fingerprint.Generator
	OAuth   *oauth.Registry
	Starter oauth.Starter
	Quota   quota.Refresher
	Claimer claim.Claimer
	Captcha captcha.Provider

	// Settings 是设置的内存快照（为 nil 时按 Store 建一个）。
	Settings *settings.Cache

	// Configured 是本次运行配置给定的设置初值（`ZCODE_*` 环境变量）。
	// **只**用作 `admin_key_is_default` 的比较基准与缺项回落，见
	// settings.Configured 的实测证据。
	Configured settings.Configured

	// Now 返回当前 epoch 秒（可注入，测试用）。
	Now func() float64
}

// API 是管理面的 http.Handler（内部是一张方法+路径路由表）。
type API struct {
	d   Deps
	mux *http.ServeMux
}

// New 装配路由表。
func New(d Deps) *API {
	if d.Now == nil {
		d.Now = models.EpochNow
	}
	if d.Ring == nil {
		d.Ring = reqlog.New(constants.MonitoringKeep)
	}
	if d.Fp == nil {
		d.Fp = fingerprint.New()
	}
	if d.OAuth == nil {
		d.OAuth = oauth.NewRegistry()
	}
	if d.Starter == nil {
		d.Starter = oauth.Unavailable{}
	}
	if d.Quota == nil {
		d.Quota = quota.Unavailable{}
	}
	if d.Claimer == nil {
		d.Claimer = claim.Unavailable{}
	}
	if d.Captcha == nil {
		d.Captcha = captcha.Unavailable{}
	}
	if d.Settings == nil {
		d.Settings = settings.NewCache(d.Store, d.Configured)
	}

	a := &API{d: d}
	mux := http.NewServeMux()

	// 鉴权**在路由匹配之后**：实测靶机对未知路径一律 404 `{"detail":"Not Found"}`，
	// 即便不带凭证也不返回 401；只有命中已知路由才校验凭证。所以这里把
	// guard 包在每个具体 handler 上，而不是包整棵子树。
	protected := func(h http.HandlerFunc) http.HandlerFunc {
		if d.Guard == nil {
			return h
		}
		return func(w http.ResponseWriter, r *http.Request) {
			if res := d.Guard.Authorize(r); !res.OK {
				httpx.WriteDetail(w, res.Status, res.Detail)
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("GET /admin/api/verify", protected(a.handleVerify))

	mux.HandleFunc("GET /admin/api/accounts", protected(a.handleAccountsList))
	mux.HandleFunc("POST /admin/api/accounts", protected(a.handleAccountsAdd))
	mux.HandleFunc("DELETE /admin/api/accounts", protected(a.handleAccountsDelete))
	mux.HandleFunc("PUT /admin/api/accounts/{account_id}", protected(a.handleAccountEdit))
	mux.HandleFunc("POST /admin/api/accounts/{account_id}/enabled", protected(a.handleAccountEnabled))
	mux.HandleFunc("POST /admin/api/accounts/{account_id}/fingerprint/rotate", protected(a.handleFingerprintRotate))
	mux.HandleFunc("POST /admin/api/accounts/refresh", protected(a.handleAccountsRefreshAll))
	mux.HandleFunc("POST /admin/api/accounts/{account_id}/refresh", protected(a.handleAccountRefresh))

	mux.HandleFunc("GET /admin/api/status", protected(a.handleStatus))

	mux.HandleFunc("POST /admin/api/login/start", protected(a.handleLoginStart))
	mux.HandleFunc("GET /admin/api/login/poll/{flow_id}", protected(a.handleLoginPoll))

	mux.HandleFunc("GET /admin/api/claim/preview", protected(a.handleClaimPreview))
	mux.HandleFunc("POST /admin/api/claim", protected(a.handleClaim))
	mux.HandleFunc("GET /admin/api/claim/captcha-config", protected(a.handleCaptchaConfig))
	mux.HandleFunc("POST /admin/api/claim/manual", protected(a.handleClaimManual))

	mux.HandleFunc("GET /admin/api/settings", protected(a.handleSettingsGet))
	mux.HandleFunc("PUT /admin/api/settings", protected(a.handleSettingsPut))

	mux.HandleFunc("GET /admin/api/export", protected(a.handleExport))
	mux.HandleFunc("POST /admin/api/import", protected(a.handleImport))

	mux.HandleFunc("GET /admin/api/monitoring", protected(a.handleMonitoring))
	mux.HandleFunc("POST /admin/api/monitoring/clear", protected(a.handleMonitoringClear))

	// 兜底：未知的 /admin/api/* 也走 `{"detail":"Not Found"}`（实测靶机如此），
	// 不能用 net/http 默认的 text/plain 404。
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteDetail(w, http.StatusNotFound, "Not Found")
	})

	a.mux = mux
	return a
}

// ServeHTTP 实现 http.Handler。
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.mux.ServeHTTP(w, r) }

// Ring 暴露监控环形缓冲（server 把网关请求写进去）。
func (a *API) Ring() *reqlog.Ring { return a.d.Ring }

// ── 请求解析工具 ────────────────────────────────────────────

// readJSON 读请求体到 dst。空体按零值处理（POST/PUT 的空体不是错误：
// `22-monitoring-clear.POST.json` 就没有请求体）。
func (a *API) readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		httpx.WriteDetail(w, http.StatusBadRequest, "读取请求体失败")
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	if err := json.Unmarshal(body, dst); err != nil {
		httpx.WriteDetail(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return false
	}
	return true
}

// decodeRawObject 把请求体解成「键 → 原始值」，用于需要区分「键缺失 / 键存在」
// 的部分写语义（settings 的 PUT 正是如此）。
func (a *API) decodeRawObject(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		httpx.WriteDetail(w, http.StatusBadRequest, "读取请求体失败")
		return nil, false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return map[string]json.RawMessage{}, true
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		httpx.WriteDetail(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return nil, false
	}
	return raw, true
}

// notImplemented 统一「需要上游，本阶段未实现」的报错。
//
// 用 501 而不是 200 + 假结果：本项目的纪律是「未实现的协议一律明确报错并附原因」。
func notImplemented(w http.ResponseWriter, why string) {
	httpx.WriteDetail(w, http.StatusNotImplemented, "尚未实现："+why)
}

// okFail 是 `{ok, fail}` 计数对象（键顺序：ok 在前，取自样本）。
type okFail struct {
	OK   int64 `json:"ok"`
	Fail int64 `json:"fail"`
}

// okResponse 是 `{"ok":true}`（多条路由的成功体）。
type okResponse struct {
	OK bool `json:"ok"`
}

var okTrue = okResponse{OK: true}
