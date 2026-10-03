package adminapi

import (
	"net/http"
	"strings"

	"github.com/LinYuanNull/zcode2api-go/internal/httpx"
)

// ── POST /admin/api/login/start ─────────────────────────────
//
// 键顺序取自样本 `11-login-start.POST.json`：flow_id, authorize_url, expires_in。
//
// 错误分支按样本 notes：上游不可达 → **502** `{"detail":"登录初始化失败: …"}`。
// 没有可用上游时走的就是这条 —— 明确报错，不伪造 authorize_url。
type loginStartRequest struct {
	Label string `json:"label"`
}

type loginStartResponse struct {
	FlowID       string `json:"flow_id"`
	AuthorizeURL string `json:"authorize_url"`
	ExpiresIn    int    `json:"expires_in"`
}

func (a *API) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	var req loginStartRequest
	if !a.readJSON(w, r, &req) {
		return
	}
	label := strings.TrimSpace(req.Label)

	// flow_id 由**上游**决定（实测三值全等，outbound-admin 3.6），所以这里不再本地
	// 生成会话再映射。失败时不留下半截会话：`Start` 只在拿到 flow_id 与
	// authorize_url 之后才登记。
	flowID, url, expiresIn, err := a.d.Sessions.Start(r.Context(), label)
	if err != nil {
		httpx.WriteDetail(w, http.StatusBadGateway, "登录初始化失败: "+err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, loginStartResponse{
		FlowID:       flowID,
		AuthorizeURL: url,
		ExpiresIn:    expiresIn,
	})
}

// ── GET /admin/api/login/poll/{flow_id} ─────────────────────
//
// 未知 flow_id 与已过期都返回 200 `{"status":"expired"}`（样本
// `12-login-poll-unknown.GET.json` 明确：**不是 404**）。`failed` 附 `message`。
//
// 轮询会**逐次打上游**（实测没有本地时间门控，outbound-admin 3.5），所以这里
// 用请求自己的 ctx —— 客户端断开时应当立刻放弃这次上游调用。
type loginPollResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

func (a *API) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	status, message, _ := a.d.Sessions.Poll(r.Context(), r.PathValue("flow_id"))
	httpx.WriteJSON(w, http.StatusOK, loginPollResponse{Status: status, Message: message})
}
