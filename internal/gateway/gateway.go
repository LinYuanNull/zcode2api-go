// Package gateway 转发网关：调度器、并发槽、SSE 透传、错误分类与冷却。
//
// 对应计划中的 A4 阶段。这是全项目最难的一期，asyncio 单线程原子语义必须在
// Go 里重新论证，不能凭感觉移植。
//
// **A3 只实现「不需要上游」的那部分**：鉴权、模型表、以及三条样本已证实的
// 错误分支（400 非法 JSON / 401 缺凭证 / 403 凭证错 / 503 无可用账号）。
// 真正的转发（含 SSE）留到 A4 —— 有可用账号时**显式报错**，不假装成功。
//
// 出站代理语义（A4 必守，先记在这里）：`/v1/messages` 走环境代理，
// billing / 验证码**强制直连**，否则触发上游风控。
package gateway

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/LinYuanNull/zcode2api-go/internal/authadmin"
	"github.com/LinYuanNull/zcode2api-go/internal/httpx"
	"github.com/LinYuanNull/zcode2api-go/internal/models"
	"github.com/LinYuanNull/zcode2api-go/internal/store"
)

// maxBodyBytes 与 adminapi 一致。
const maxBodyBytes = 1 << 20

// 错误文案与 type 取值逐字取自样本：
//   - `24-messages-badjson` / `25-chat-badjson`：`{"error":{"message":"请求体不是合法 JSON","type":…}}`
//   - `24-messages-noaccount` / `25-chat-noaccount`：`{"error":{"message":"所有账号均不可用…","type":"no_available_account"}}`
const (
	msgBadJSON   = "请求体不是合法 JSON"
	msgNoAccount = "所有账号均不可用、额度已用完或并发已满，请在后台检查账号状态"

	// typeBadJSONMessages / typeBadJSONChat 两种 type 不同，是样本里明确的事实。
	typeBadJSONMessages = "invalid_request"
	typeBadJSONChat     = "invalid_request_error"
	typeNoAccount       = "no_available_account"
)

// Model 是 `/v1/models` 里的一项（Anthropic 风格：`type` 而非 `object`）。
type Model struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

// DefaultModels 是模型表。
//
// 依据：`23-models-ok.GET.json` 的 notes 明确「模型表为编译期常量
// （app/constants.AVAILABLE_MODELS）」，且实测靶机的 `/v1/models` 恰好返回这两条
// （已对运行中的靶机复核，不是采样截断）。用 `--models` 可覆盖。
var DefaultModels = []Model{
	{ID: "GLM-5.3-Flash", Type: "model", DisplayName: "GLM-5.3-Flash", CreatedAt: "2025-01-01T00:00:00Z"},
	{ID: "GLM-5.3", Type: "model", DisplayName: "GLM-5.3", CreatedAt: "2025-01-01T00:00:00Z"},
}

// Options 是 Gateway 的依赖。
type Options struct {
	Store *store.Store

	// GatewayKey 返回当前网关 Key；空串表示**不校验**（`.env.example` 的语义）。
	GatewayKey func() string

	Models []Model
}

// Gateway 是网关入口。
type Gateway struct {
	d Options
}

// New 建网关入口。
func New(o Options) *Gateway {
	if o.Models == nil {
		o.Models = DefaultModels
	}
	return &Gateway{d: o}
}

// errorBody / errorEnvelope 是网关域的错误体 `{"error":{"message","type"}}`。
// **不可与 admin 域的 `{"detail":…}` 混用**（SPEC.md 第三部分）。
type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type modelsResponse struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// ServeHTTP 实现 http.Handler（挂在 /v1/ 子树下）。
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/models":
		if r.Method != http.MethodGet {
			g.methodNotAllowed(w)
			return
		}
		if !g.authorize(w, r) {
			return
		}
		httpx.WriteJSON(w, http.StatusOK, modelsResponse{Object: "list", Data: g.d.Models})
	case "/v1/messages":
		g.handleMessages(w, r, typeBadJSONMessages)
	case "/v1/chat/completions":
		g.handleMessages(w, r, typeBadJSONChat)
	default:
		httpx.WriteDetail(w, http.StatusNotFound, "Not Found")
	}
}

func (g *Gateway) methodNotAllowed(w http.ResponseWriter) {
	w.Header().Set("Allow", "GET")
	httpx.WriteDetail(w, http.StatusMethodNotAllowed, "Method Not Allowed")
}

// authorize 校验网关 Key。
//
// 三条分支的文案逐字取自样本：缺凭证 401 `缺少 API Key`、凭证错 403 `API Key 无效`。
// 网关 Key 为空串时不校验（`.env.example`：「网关 API Key 留空 = 不校验」）。
func (g *Gateway) authorize(w http.ResponseWriter, r *http.Request) bool {
	want := g.d.GatewayKey()
	if want == "" {
		return true
	}
	token, ok := authadmin.Bearer(r)
	if !ok {
		httpx.WriteDetail(w, http.StatusUnauthorized, "缺少 API Key")
		return false
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(want)) != 1 {
		httpx.WriteDetail(w, http.StatusForbidden, "API Key 无效")
		return false
	}
	return true
}

// handleMessages 处理两个转发入口。A3 覆盖：鉴权 → JSON 合法性 → 账号可用性。
func (g *Gateway) handleMessages(w http.ResponseWriter, r *http.Request, badJSONType string) {
	if r.Method != http.MethodPost {
		g.methodNotAllowed(w)
		return
	}
	if !g.authorize(w, r) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil || !json.Valid(bytes.TrimSpace(body)) {
		httpx.WriteJSON(w, http.StatusBadRequest, errorEnvelope{errorBody{msgBadJSON, badJSONType}})
		return
	}
	if !g.hasUsableAccount() {
		httpx.WriteJSON(w, http.StatusServiceUnavailable, errorEnvelope{errorBody{msgNoAccount, typeNoAccount}})
		return
	}
	// 有可用账号 ⇒ 必须真正转发（调度、body 变换、SSE），属 A4。
	httpx.WriteDetail(w, http.StatusNotImplemented,
		"尚未实现：转发链路（调度/body 变换/SSE 透传）属于 A4 阶段")
}

func (g *Gateway) hasUsableAccount() bool {
	for _, a := range g.d.Store.List() {
		if a.Usable() {
			return true
		}
	}
	return false
}

// ModelsFromList 把逗号分隔的模型名解析成模型表（`--models` 用）。
func ModelsFromList(csv string) []Model {
	parts := strings.Split(csv, ",")
	out := make([]Model, 0, len(parts))
	for _, p := range parts {
		id := strings.TrimSpace(p)
		if id == "" {
			continue
		}
		out = append(out, Model{ID: id, Type: "model", DisplayName: id, CreatedAt: "2025-01-01T00:00:00Z"})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// 保证 Models 与 models 包的账号判定口径一致（编译期约束，防止将来漂移）。
var _ = models.Account.Usable
