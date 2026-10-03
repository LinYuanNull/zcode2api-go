// Package oauth 账号登录：OAuth 设备码流程。
//
// 语义来自实测样本（`docs/contract/admin/11..12` 与 `docs/contract/outbound-admin/`）：
//
//   - `11-login-start.POST.json`：成功返回 `{flow_id, authorize_url, expires_in}`，
//     `expires_in` **固定 300**；上游不可达 → 502 `{detail:"登录初始化失败: …"}`。
//   - `12-login-poll-unknown.GET.json`：**未知 flow_id 返回 200 `{status:"expired"}`**，
//     不是 404。
//   - **`flow_id` 就是上游 `oauth/cli/init` 返回的那个**（32 位 hex，实测三值全等，
//     outbound-admin 3.6）⇒ 会话标识的所有权在**上游**，本包必须沿用而不是另生成。
//   - 轮询**逐次都打上游**（没有本地时间门控）；上游报错后不再打（outbound-admin 3.5）。
package oauth

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// 轮询状态取值。依据：`12-login-poll-unknown.GET.json` 的 notes。
const (
	StatusPending = "pending"
	StatusReady   = "ready"
	StatusFailed  = "failed"
	StatusExpired = "expired"
)

// DefaultExpiresIn 是发起登录时给客户端展示的有效期（秒）。
// 依据：`11-login-start.POST.json` 的 `expires_in` = 300，且实测是**固定值**
// （上游回的是 `expires_at` 时间戳，管理侧换算成固定 300，outbound-admin 3.4）。
const DefaultExpiresIn = 300

// TokenLen 是 OAuth 请求 Bearer 的长度（64 位小写 hex）。
// 依据：outbound-admin 3.1/3.2 —— 该令牌**进程内随机生成、不落盘**，
// 且与上游回传的 `poll_token` 同值。
const TokenLen = 64

// Account 是登录成功后落库的账号信息。
type Account struct {
	Name   string
	Token  string
	Mode   string
	Label  string
	Expire time.Time
}

// Flow 是一次登录会话。
type Flow struct {
	ID        string
	Label     string
	Status    string
	Message   string
	Account   *Account
	ExpiresAt time.Time

	// Token 是这条 flow 的 OAuth Bearer（64 hex）。init 与后续 poll **复用同一个值**。
	Token string
	// PollIntervalSec 是上游给的轮询建议间隔（本样本 2）。**不是出站门控**，
	// 实测只作展示用；留着是为了把它放进会话状态、便于诊断。
	PollIntervalSec int
	// UpstreamStopped 表示这条 flow 的上游轮询已被停掉（某次出站失败后）。
	// 实测：出站失败后管理侧不再打上游，直接回缓存状态（outbound-admin 3.5）。
	UpstreamStopped bool
}

// Registry 是内存里的登录会话表。
type Registry struct {
	mu    sync.Mutex
	flows map[string]*Flow
	now   func() time.Time
}

// NewRegistry 建一个空表。
func NewRegistry() *Registry {
	return &Registry{flows: map[string]*Flow{}, now: time.Now}
}

// SetClock 注入时钟（测试用）。
func (r *Registry) SetClock(now func() time.Time) { r.now = now }

// NewFlowID 生成 32 位 hex 的标识（**仅测试与兜底用**）。
//
// 生产路径的 `flow_id` 来自上游（outbound-admin 3.6），不要用它顶替。
func NewFlowID() string { return hexOf(16) }

// NewToken 生成一条 flow 的 OAuth Bearer（64 位小写 hex）。
//
// 与 `poll_token` 同值、**不落盘** —— 它只活在内存里的 Flow 上。
func NewToken() string { return hexOf(TokenLen / 2) }

func hexOf(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 在正常系统上不会失败；真失败时给一个**可预测**的会话令牌
		// 比直接报错更危险，所以 panic。
		panic("oauth: 无法读取随机源: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// Put 登记（或覆盖）一条会话。
func (r *Registry) Put(f *Flow) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f.Status == "" {
		f.Status = StatusPending
	}
	if f.ExpiresAt.IsZero() {
		f.ExpiresAt = r.now().Add(DefaultExpiresIn * time.Second)
	}
	r.flows[f.ID] = f
}

// Lookup 取一条会话的**副本**（调用方改不到表内的状态）。
func (r *Registry) Lookup(id string) (Flow, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.flows[id]
	if !ok {
		return Flow{}, false
	}
	return *f, true
}

// StopUpstream 停掉一条会话的上游轮询。
func (r *Registry) StopUpstream(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.flows[id]; ok {
		f.UpstreamStopped = true
	}
}

// SetStatus 改一条会话的状态。
func (r *Registry) SetStatus(id, status, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.flows[id]; ok {
		f.Status = status
		f.Message = message
	}
}

// SetExpiry 更新一个会话的有效期（发起成功后按上游给的 expires_in 校准）。
func (r *Registry) SetExpiry(id string, seconds int) {
	if seconds <= 0 {
		seconds = DefaultExpiresIn
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.flows[id]; ok {
		f.ExpiresAt = r.now().Add(time.Duration(seconds) * time.Second)
	}
}

// Expire 判定一条会话是否已过期，过期则落状态。
//
// **未知 id 与已过期一律 `expired`**（HTTP 200），这是样本明确的行为：
// UI 靠 `expired` 收尾，不靠 404。
func (r *Registry) Expire(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.flows[id]
	if !ok {
		return true
	}
	if f.Status != StatusReady && f.Status != StatusFailed && !r.now().Before(f.ExpiresAt) {
		f.Status = StatusExpired
	}
	return f.Status == StatusExpired
}

// Complete 把一个会话标记为成功（授权完成，账号已入池）。
func (r *Registry) Complete(id string, acc Account) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.flows[id]
	if !ok {
		return false
	}
	a := acc
	f.Account = &a
	f.Status = StatusReady
	return true
}

// Fail 把一个会话标记为失败。
func (r *Registry) Fail(id, message string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.flows[id]
	if !ok {
		return false
	}
	f.Status = StatusFailed
	f.Message = message
	return true
}

// Forget 移除一个会话（测试与清理用）。
func (r *Registry) Forget(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.flows, id)
}
