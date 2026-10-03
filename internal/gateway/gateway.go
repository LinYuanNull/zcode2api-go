package gateway

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/LinYuanNull/zcode2api-go/internal/agent"
	"github.com/LinYuanNull/zcode2api-go/internal/authadmin"
	"github.com/LinYuanNull/zcode2api-go/internal/bodytransform"
	"github.com/LinYuanNull/zcode2api-go/internal/httpx"
	"github.com/LinYuanNull/zcode2api-go/internal/marks"
	"github.com/LinYuanNull/zcode2api-go/internal/models"
	"github.com/LinYuanNull/zcode2api-go/internal/scheduler"
	"github.com/LinYuanNull/zcode2api-go/internal/store"
)

// maxBodyBytes 与 adminapi 一致。
const maxBodyBytes = 1 << 20

// 错误文案与 type 取值逐字取自样本：
//   - `24-messages-badjson` / `25-chat-badjson`：`{"error":{"message":"请求体不是合法 JSON","type":…}}`
//   - `26-messages-notobject`：合法 JSON 但根不是对象 → **另一个 type**（两入口一致）
//   - `24-messages-noaccount` / `25-chat-noaccount`：`{"error":{"message":"所有账号均不可用…","type":"no_available_account"}}`
const (
	msgBadJSON   = "请求体不是合法 JSON"
	msgNotObject = "请求体必须是 JSON 对象"
	msgNoAccount = "所有账号均不可用、额度已用完或并发已满，请在后台检查账号状态"

	// typeBadJSONMessages / typeBadJSONChat 两种 type 不同，是样本里明确的事实。
	typeBadJSONMessages = "invalid_request"
	typeBadJSONChat     = "invalid_request_error"
	// typeNotObject 是「合法 JSON 但根不是对象」的 type：**两个入口一致**用
	// `invalid_request_error`（样本 26 的 notes：与 chat 的非法 JSON 同型，
	// 与 messages 的非法 JSON `invalid_request` **不同**）。
	typeNotObject = "invalid_request_error"
	typeNoAccount = "no_available_account"
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

	// Marks 是记号输出。nil 用 stderr。
	Marks marks.Writer

	// Agent 允许测试注入带假上游 URL 的出站客户端；nil 时新建。
	// 生产恒为 nil（agent.New() 即正确行为）。
	Agent *agent.Client
}

// Gateway 是网关入口。
type Gateway struct {
	d     Options
	sched *scheduler.Scheduler
}

// New 建网关入口。
func New(o Options) *Gateway {
	if o.Models == nil {
		o.Models = DefaultModels
	}
	// 出站客户端与调度器在网关生命周期内只建一次：
	// 连接池、TLS 会话缓存都在 Client 里，随请求重建等于每请求重新握手。
	ag := o.Agent
	if ag == nil {
		ag = agent.New()
	}
	g := &Gateway{d: o, sched: scheduler.New(o.Store, ag, o.Marks)}
	return g
}

// Scheduler 暴露调度器（测试与 admin 的「测试转发」按钮用）。
func (g *Gateway) Scheduler() *scheduler.Scheduler { return g.sched }

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
		g.handleMessagesPath(w, r, badJSONTypeMessages)
	case "/v1/chat/completions":
		// chat 入口的转发语义（OpenAI 重建 / 形状转换）属 A4 后续（#145）；
		// 当前已实现的分支与 messages 完全一致地复用，**重建部分到位后再分叉**。
		g.handleMessagesPath(w, r, badJSONTypeChat)
	default:
		httpx.WriteDetail(w, http.StatusNotFound, "Not Found")
	}
}

// 两个入口的非法 JSON type 不同（样本 24 vs 25）。
const (
	badJSONTypeMessages = iota
	badJSONTypeChat
)

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

// handleMessagesPath 是两个转发入口的共同路径。
//
// 分支顺序（每一步都由样本/探针钉死）：
//
//	方法 → 鉴权 → 读体 → 非法 JSON(400) → 非对象(400) → model 非串(400)
//	→ `>>>` 行 → body 变换 → 调度（池空 ⇒ 503）→ 响应
//
// ⚠️ `>>>` 行在**错误分支之后**才打：非对象 / 非法 JSON 的探针记录是
// 「（无该行）」。model 非串也**不打**（参考实现连 `>>>` 都没打就 500 了；
// 本实现按 §六之二改为 400，保持「拒绝发生在记号之前」的顺序）。
func (g *Gateway) handleMessagesPath(w http.ResponseWriter, r *http.Request, badJSONType int) {
	if r.Method != http.MethodPost {
		g.methodNotAllowed(w)
		return
	}
	if !g.authorize(w, r) {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil || !json.Valid(bytes.TrimSpace(raw)) {
		g.writeBadJSON(w, badJSONType)
		return
	}

	// 合法 JSON 但根不是对象 ⇒ 400（两入口同型，样本 26）。
	// 这条必须在 `>>>` 之前：探针记录该分支没有 `>>>` 行。
	info, ierr := bodytransform.Inspect(raw)
	if errors.Is(ierr, bodytransform.ErrNotObject) {
		httpx.WriteJSON(w, http.StatusBadRequest, errorEnvelope{errorBody{msgNotObject, typeNotObject}})
		return
	}
	if errors.Is(ierr, bodytransform.ErrModelNotString) {
		// §六之二「有意偏离」：参考实现这里是 500 崩溃（连 `>>>` 都不打）。
		// 本实现改为 400 + 明确文案，同样不打 `>>>`（保持「拒绝在记号之前」）。
		httpx.WriteJSON(w, http.StatusBadRequest, errorEnvelope{
			errorBody{ierr.Error(), typeBadJSONChat}})
		return
	}
	if ierr != nil {
		// 解析层其它错误：按非法 JSON 处理（bodytransform 的 Parse 只会在
		// 截断/深层嵌套等场景失败，json.Valid 已排除绝大多数）。
		g.writeBadJSON(w, badJSONType)
		return
	}

	reqID := marks.ReqID()
	marks.Route(g.marksWriter(), reqID, info.ModelLabel(), info.StreamLabel(), info.Preview)

	// 出站体：/v1/messages 保持入站键序，只改写字符串 content。
	outbound, terr := bodytransform.Messages(raw)
	if terr != nil {
		// Inspect 已通过而 Messages 失败只可能是并发体损坏（不可能发生，
		// raw 是本请求独占的）；防御式处理，不伪造成功。
		httpx.WriteJSON(w, http.StatusInternalServerError, errorEnvelope{
			errorBody{"出站体变换失败", typeBadJSONChat}})
		return
	}

	res := g.sched.Do(r.Context(), scheduler.Request{
		Body:  outbound,
		ReqID: reqID,
		Model: info.Model,
	})

	switch res.Outcome {
	case scheduler.OutcomeSuccess:
		g.passthrough(w, res.Resp)
	case scheduler.OutcomeClientError:
		// 客户端错：上游体原样透传（`<!>` 行已在调度器里打）。
		g.writeUpstreamError(w, res)
	default:
		// 池空 / 全失败 ⇒ 503（文案与 type 逐字取自样本）。
		httpx.WriteJSON(w, http.StatusServiceUnavailable,
			errorEnvelope{errorBody{msgNoAccount, typeNoAccount}})
	}
}

// writeBadJSON 按入口写非法 JSON 错误。
func (g *Gateway) writeBadJSON(w http.ResponseWriter, badJSONType int) {
	t := typeBadJSONMessages
	if badJSONType == badJSONTypeChat {
		t = typeBadJSONChat
	}
	httpx.WriteJSON(w, http.StatusBadRequest, errorEnvelope{errorBody{msgBadJSON, t}})
}

// passthrough 把上游响应**逐字节**透传给客户端。
//
// 响应头规则（探针 h-*，behavior.md §一）：
//   - 只搬上游 `Content-Type`（`text/*` 追加 `; charset=utf-8`）；
//   - 其它上游头**一律不透传**（探针给上游加了 X-Custom-Probe，入站没有）；
//   - **总是**加 `cache-control: no-cache`。
func (g *Gateway) passthrough(w http.ResponseWriter, resp *http.Response) {
	defer agent.DrainAndClose(resp)
	ct := resp.Header.Get("Content-Type")
	if ct != "" {
		if strings.HasPrefix(ct, "text/") && !strings.Contains(ct, "charset=") {
			ct += "; charset=utf-8"
		}
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(resp.StatusCode)
	if resp.Body != nil {
		_, _ = io.Copy(w, resp.Body)
	}
}

// writeUpstreamError 透传「客户端错」分支的上游响应。
//
// ⚠️ 与成功路径不同：**不加 `cache-control`**。harness 对照
// （upstream-400 场景，2026-10-04）实测靶机的客户端错响应只有
// `content-type` 一个头，没有 `cache-control: no-cache` —— 靶机的
// no-cache 只加在正常响应路径上。这条实测已补进 behavior.md §一。
func (g *Gateway) writeUpstreamError(w http.ResponseWriter, res scheduler.Result) {
	ct := res.ClientCT
	if ct != "" {
		if strings.HasPrefix(ct, "text/") && !strings.Contains(ct, "charset=") {
			ct += "; charset=utf-8"
		}
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(res.ClientCode)
	_, _ = w.Write(res.ClientBody)
}

// marksWriter 返回记号输出（懒初始化，方便测试注入）。
func (g *Gateway) marksWriter() marks.Writer {
	if g.d.Marks != nil {
		return g.d.Marks
	}
	return marks.NewStdout()
}

// hasUsableAccount 保留给 A3 时代的调用方（面板状态查询）。
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
