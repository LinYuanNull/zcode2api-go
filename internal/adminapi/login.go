package adminapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/LinYuanNull/zcode2api-go/internal/httpx"
	"github.com/LinYuanNull/zcode2api-go/internal/oauth"
)

// ── POST /admin/api/login/start ─────────────────────────────
//
// 键顺序取自样本 `11-login-start.POST.json`：flow_id, authorize_url, expires_in。
//
// 错误分支按样本 notes：上游不可达 → **502** `{"detail":"登录初始化失败: …"}`。
// A3 的 `oauth.Unavailable` 走的就是这条 —— 明确报错，不伪造 authorize_url。
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

	// 会话先登记、再打上游：flow_id 由本地生成（它是轮询的键），
	// 上游失败时把它撤掉，避免留下永远 pending 的僵尸会话。
	flow := a.d.OAuth.Create(label, oauth.DefaultExpiresIn*time.Second)
	url, expiresIn, err := a.d.Starter.Start(flow.ID, label)
	if err != nil {
		a.d.OAuth.Forget(flow.ID)
		httpx.WriteDetail(w, http.StatusBadGateway, "登录初始化失败: "+err.Error())
		return
	}
	a.d.OAuth.SetExpiry(flow.ID, expiresIn)
	if expiresIn <= 0 {
		expiresIn = oauth.DefaultExpiresIn
	}
	httpx.WriteJSON(w, http.StatusOK, loginStartResponse{
		FlowID:       flow.ID,
		AuthorizeURL: url,
		ExpiresIn:    expiresIn,
	})
}

// ── GET /admin/api/login/poll/{flow_id} ─────────────────────
//
// 未知 flow_id 与已过期都返回 200 `{"status":"expired"}`（样本
// `12-login-poll-unknown.GET.json` 明确：**不是 404**）。`failed` 附 `message`。
type loginPollResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

func (a *API) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	status, message, _ := a.d.OAuth.Poll(r.PathValue("flow_id"))
	httpx.WriteJSON(w, http.StatusOK, loginPollResponse{Status: status, Message: message})
}
