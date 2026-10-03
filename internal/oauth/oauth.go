// Package oauth 账号登录：OAuth 设备码流程。
//
// A3 落地的是**流程登记表**（发起 → 轮询 → 完成/失败/过期），因为它的语义
// 可以从 A1 样本逐条证实：
//
//   - `11-login-start.POST.json`：成功返回 `{flow_id, authorize_url, expires_in}`，
//     `flow_id` 为 32 位 hex，`expires_in` = 300；上游不可达 → 502 `{detail:"登录初始化失败: …"}`。
//   - `12-login-poll-unknown.GET.json`：**未知 flow_id 返回 200 `{status:"expired"}`**，
//     不是 404；已知 flow 的其余取值是 `pending` / `ready` / `failed`（`failed` 附 `message`）。
//
// **发起**这一步要打上游（拿设备码 / 授权地址），属 A5；这里只定义 `Starter`
// 接口与「未配置」默认实现，保证路由可用、语义可测，且不会假装成功。
package oauth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
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
// 依据：`11-login-start.POST.json` 的 `expires_in` = 300。
const DefaultExpiresIn = 300

// ErrUpstreamUnavailable 表示「发起登录需要上游调用，本阶段未实现」。
var ErrUpstreamUnavailable = errors.New("登录初始化需要上游 OAuth 调用（属于 A5 阶段）")

// Account 是登录成功后落库的账号信息。
type Account struct {
	Name   string
	Token  string
	Mode   string
	Label  string
	Expire time.Time
}

// Starter 发起一次设备码登录，返回给用户打开的授权地址与有效期（秒）。
//
// `flowID` 由本包的 Registry 生成后传入 —— 会话标识的所有权在本地（它是
// 轮询用的键），上游只需要知道「要给哪个会话授权」。expiresIn <= 0 时调用方
// 回落到 DefaultExpiresIn。
type Starter interface {
	Start(flowID, label string) (authorizeURL string, expiresIn int, err error)
}

// Unavailable 是 Starter 的占位实现：明确报错，不伪造 authorize_url。
type Unavailable struct{}

// Start 实现 Starter。
func (Unavailable) Start(string, string) (string, int, error) {
	return "", 0, ErrUpstreamUnavailable
}

// Flow 是一次登录会话。
type Flow struct {
	ID        string
	Label     string
	Status    string
	Message   string
	Account   *Account
	ExpiresAt time.Time
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

// NewFlowID 生成 32 位 hex 的 flow_id（与样本里 `flow_id` 的形态一致）。
func NewFlowID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("oauth: 无法读取随机源: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// Create 登记一个新会话。
func (r *Registry) Create(label string, expiresIn time.Duration) *Flow {
	if expiresIn <= 0 {
		expiresIn = DefaultExpiresIn * time.Second
	}
	f := &Flow{
		ID:        NewFlowID(),
		Label:     label,
		Status:    StatusPending,
		ExpiresAt: r.now().Add(expiresIn),
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flows[f.ID] = f
	return f
}

// Put 直接登记一个指定 id 的会话（测试用，便于断言固定 flow_id）。
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

// Poll 查询一个会话的状态。
//
// **未知 id 与已过期一律返回 `expired`**（HTTP 200），这是样本明确的行为：
// UI 靠 `expired` 收尾，不靠 404。
func (r *Registry) Poll(id string) (status, message string, acc *Account) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.flows[id]
	if !ok {
		return StatusExpired, "", nil
	}
	if f.Status != StatusReady && f.Status != StatusFailed && !r.now().Before(f.ExpiresAt) {
		f.Status = StatusExpired
	}
	return f.Status, f.Message, f.Account
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
