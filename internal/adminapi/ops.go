package adminapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/httpx"
	"github.com/LinYuanNull/zcode2api-go/internal/models"
	"github.com/LinYuanNull/zcode2api-go/internal/reqlog"
)

// ── GET /admin/api/verify ───────────────────────────────────
type verifyResponse struct {
	Status string `json:"status"`
}

func (a *API) handleVerify(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, verifyResponse{Status: "ok"})
}

// ── GET /admin/api/status ───────────────────────────────────
//
// 键顺序取自样本 `03-status.GET.json`：providers, gateway_key_set, quota_pool。
// （注意 SPEC.md 的表格把它写成 gateway_key_set 在前 —— 那是重采样前的字典序，
// 以样本为准。）
type statusResponse struct {
	Providers     []string        `json:"providers"`
	GatewayKeySet bool            `json:"gateway_key_set"`
	QuotaPool     json.RawMessage `json:"quota_pool"`
}

func (a *API) handleStatus(w http.ResponseWriter, _ *http.Request) {
	s, err := a.d.Settings.Get()
	if err != nil {
		httpx.WriteDetail(w, http.StatusInternalServerError, err.Error())
		return
	}
	pool := models.NewOrderedObject()
	for _, p := range constants.Providers {
		pool.Set(p, a.usableCount(p))
	}
	raw, err := pool.Bytes()
	if err != nil {
		httpx.WriteDetail(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, statusResponse{
		Providers:     constants.Providers,
		GatewayKeySet: s.GatewayKey != "",
		QuotaPool:     raw,
	})
}

// usableCount 统计某 provider 当前**可参与网关调度**的账号数。
//
// ⚠️ 「可用」的判据**未采样**：样本里 quota_pool 全为 0（当时账号池为空），
// 无法区分「总账号数」与「启用且 active 的账号数」。这里取后者（调度语义上更自然），
// 已登记在 PROVENANCE.md，等有非空样本后校准。
func (a *API) usableCount(provider string) int64 {
	var n int64
	for _, acc := range a.d.Store.List() {
		if acc.Provider == provider && acc.Usable() {
			n++
		}
	}
	return n
}

// ── GET /admin/api/export ───────────────────────────────────
//
// 键顺序取自样本 `19-export.GET.json`：version, exported_at, providers。
// `providers` 的键是 provider 枚举序，且**空 provider 也要出现**（样本里 bigmodel 为 []）。
type exportEntry struct {
	Name   string `json:"name"`
	Mode   string `json:"mode"`
	Secret string `json:"secret"`
}

type exportResponse struct {
	Version    int             `json:"version"`
	ExportedAt float64         `json:"exported_at"`
	Providers  json.RawMessage `json:"providers"`
}

func (a *API) handleExport(w http.ResponseWriter, _ *http.Request) {
	accts := a.d.Store.List()
	prov := models.NewOrderedObject()
	for _, p := range constants.Providers {
		entries := make([]exportEntry, 0)
		for _, acc := range accts {
			if acc.Provider != p {
				continue
			}
			entries = append(entries, exportEntry{
				Name:   acc.Name,
				Mode:   acc.Mode,
				Secret: acc.Credential(),
			})
		}
		if err := prov.Set(p, entries); err != nil {
			httpx.WriteDetail(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	raw, err := prov.Bytes()
	if err != nil {
		httpx.WriteDetail(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, exportResponse{
		Version:    1,
		ExportedAt: a.d.Now(),
		Providers:  raw,
	})
}

// ── POST /admin/api/import ──────────────────────────────────
//
// 请求体 `{providers:{<provider>:[{name, secret, mode?}]}}`；成功体 `{count}`。
// `mode` 在样本里没出现（样本条目只有 name/secret），但导出会带 `mode`，
// 导入时接受它才能让「导出 → 导入」往返保真；缺省时按凭据形态判定。
type importEntry struct {
	Name   string `json:"name"`
	Mode   string `json:"mode"`
	Secret string `json:"secret"`
}

type importRequest struct {
	Providers map[string][]importEntry `json:"providers"`
}

type importResponse struct {
	Count int64 `json:"count"`
}

func (a *API) handleImport(w http.ResponseWriter, r *http.Request) {
	var req importRequest
	if !a.readJSON(w, r, &req) {
		return
	}
	for p := range req.Providers {
		if !constants.IsProvider(p) {
			httpx.WriteDetail(w, http.StatusBadRequest, "不支持的 provider")
			return
		}
	}
	// 按枚举序处理：map 迭代顺序随机，固定顺序才能让 count 与默认名可复现。
	var count int64
	for _, p := range constants.Providers {
		for _, e := range req.Providers[p] {
			secret := strings.TrimSpace(e.Secret)
			if secret == "" {
				continue
			}
			if _, exists := a.d.Store.FindByCredential(p, secret); exists {
				continue
			}
			acc, err := models.NewAPIKeyAccount(
				p, strings.TrimSpace(e.Name), secret,
				a.d.Store.CountByProvider(p), a.d.Fp.Next(), a.d.Now(),
			)
			if err != nil {
				httpx.WriteDetail(w, http.StatusBadRequest, err.Error())
				return
			}
			// 导入条目可带显式 `mode`（导出会写它，往返要保真）；缺省时按凭据形态判，
			// 与新增路径同一条规则。
			if e.Mode == "" {
				applyCredential(&acc, secret)
			} else {
				switch e.Mode {
				case constants.ModeJWT:
					tok := secret
					acc.Mode = constants.ModeJWT
					acc.JWTToken = &tok
					acc.APIKey = nil
				case constants.ModeAPIKey:
					// NewAPIKeyAccount 已是 apiKey。
				default:
					httpx.WriteDetail(w, http.StatusBadRequest, "不支持的 mode: "+e.Mode)
					return
				}
			}
			if err := a.d.Store.Put(acc); err != nil {
				httpx.WriteDetail(w, http.StatusInternalServerError, err.Error())
				return
			}
			count++
		}
	}
	httpx.WriteJSON(w, http.StatusOK, importResponse{Count: count})
}

// ── GET /admin/api/monitoring ───────────────────────────────
//
// 键顺序取自样本 `21-monitoring.GET.json`：entries, keep。
type monitoringResponse struct {
	Entries []reqlog.Entry `json:"entries"`
	Keep    int            `json:"keep"`
}

func (a *API) handleMonitoring(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, monitoringResponse{
		Entries: a.d.Ring.Entries(),
		Keep:    a.d.Ring.Keep(),
	})
}

// ── POST /admin/api/monitoring/clear ────────────────────────
func (a *API) handleMonitoringClear(w http.ResponseWriter, _ *http.Request) {
	a.d.Ring.Clear()
	httpx.WriteJSON(w, http.StatusOK, okTrue)
}

// detectMode 判 mode：**仅 zai 且凭据恰好含 2 个点** → jwt，否则 apiKey。
//
// 判据是实测出来的，不是猜的（对运行中的靶机逐条探测，见 SPEC.md「补充实测」）：
//
//	provider  token 形态            → mode
//	zai       a.b.c / ..            → jwt      （只要恰好 2 个点，内容不校验）
//	zai       a.b / a               → apiKey
//	zai       a.b.c.d               → apiKey   （3 个点不算）
//	bigmodel  任意（含合法 JWT）      → apiKey   ← provider 是硬条件
//
// 注意两处反直觉：① **不看内容**，`..` 也算 jwt（不做 base64/JSON 解析）；
// ② **provider 参与判定** —— 同一串 JWT 给 bigmodel 会存成 apiKey。
func detectMode(provider, token string) string {
	if provider == constants.ProviderZAI && strings.Count(token, ".") == 2 {
		return constants.ModeJWT
	}
	return constants.ModeAPIKey
}
