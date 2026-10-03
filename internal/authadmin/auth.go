// Package authadmin 管理面鉴权：常数时间比较 + IP 失败锁定。
//
// 覆盖 A1 样本可证实的部分 + 计划要求的加固部分：
//
//   - **缺凭证 → 401 `{"detail":"缺少鉴权凭证"}`**：A1 样本
//     `01-verify-unauthorized.GET.json` 逐字可证。
//   - **密钥比对用常数时间比较**：计划 §5.2 A3 明确要求（`crypto/subtle`）。
//   - **IP 失败锁定**：计划 §5.2 A3 明确要求；但**阈值与窗口未采样**
//     （A1 未采任何 429 分支），因此这里的取值是**本实现的加固选择**，
//     不是复刻，已登记在 PROVENANCE.md。
//   - **错误密钥的响应体未采样**（A1 只采了「缺凭证」）。本实现沿用 admin 域
//     统一的 `{"detail":…}` 形态，而不是 gateway 域的 `{"error":…}`。
//
// 密钥通过 `key func() string` **实时读取**，不是启动时快照 —— 这样
// `PUT /admin/api/settings` 改完密码立刻按新值鉴权（A2 observations #23 的
// 「密码以库为准」在 HTTP 层的必然推论）。
package authadmin

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 加固参数（未采样，见包注释）。
const (
	DefaultMaxFailures = 10
	DefaultFailWindow  = 5 * time.Minute
	DefaultLockFor     = 5 * time.Minute
)

// Result 是一次鉴权的结论。
type Result struct {
	OK     bool
	Status int    // OK=false 时的 HTTP 状态码
	Detail string // OK=false 时给用户的说明（admin 域形态）
}

// Guard 是管理面鉴权器。
type Guard struct {
	// Key 返回当前生效的后台密码。每次鉴权都调用一次。
	Key func() string

	MaxFailures int
	FailWindow  time.Duration
	LockFor     time.Duration

	// Now 可注入时钟（测试用）；为 nil 时用 time.Now。
	Now func() time.Time

	mu       sync.Mutex
	failures map[string]*failState
}

type failState struct {
	count    int
	first    time.Time
	lockedTo time.Time
}

// New 建一个鉴权器。
func New(key func() string) *Guard {
	return &Guard{
		Key:         key,
		MaxFailures: DefaultMaxFailures,
		FailWindow:  DefaultFailWindow,
		LockFor:     DefaultLockFor,
		failures:    map[string]*failState{},
	}
}

func (g *Guard) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Authorize 校验一次请求。
func (g *Guard) Authorize(r *http.Request) Result {
	ip := ClientIP(r)
	if locked, until := g.locked(ip); locked {
		return Result{Status: http.StatusTooManyRequests,
			Detail: "鉴权失败次数过多，请于 " + until.Format("15:04:05") + " 后重试"}
	}

	token, ok := Bearer(r)
	if !ok {
		// 缺凭证不记账：扫描器/健康检查不带头是常态，记账会把正常用户锁掉。
		return Result{Status: http.StatusUnauthorized, Detail: "缺少鉴权凭证"}
	}
	want := g.Key()
	if subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1 && want != "" {
		g.reset(ip)
		return Result{OK: true}
	}
	g.recordFailure(ip)
	return Result{Status: http.StatusUnauthorized, Detail: "鉴权失败"}
}

// Bearer 取 `Authorization: Bearer <token>`。
//
// 头缺失、方案不是 Bearer、或 token 为空 → ok=false。
//
// 导出是因为**网关面用的是同一套头**：`/v1/*` 的样本请求头同样是
// `Authorization: <redacted:bearer>`（见 docs/contract/gateway/*.json），
// 只是缺凭证时回 401 `{"detail":"缺少 API Key"}`、凭证错时回 403。
func Bearer(r *http.Request) (string, bool) {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if h == "" {
		return "", false
	}
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

func (g *Guard) locked(ip string) (bool, time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.failures[ip]
	if !ok {
		return false, time.Time{}
	}
	now := g.now()
	if st.lockedTo.After(now) {
		return true, st.lockedTo
	}
	// 锁已过期，且失败窗口也过期 → 清掉记录。
	if now.Sub(st.first) > g.FailWindow {
		delete(g.failures, ip)
	}
	return false, time.Time{}
}

func (g *Guard) recordFailure(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	st, ok := g.failures[ip]
	if !ok || now.Sub(st.first) > g.FailWindow {
		g.failures[ip] = &failState{count: 1, first: now}
		return
	}
	st.count++
	if st.count >= g.MaxFailures {
		st.lockedTo = now.Add(g.LockFor)
	}
}

func (g *Guard) reset(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.failures, ip)
}

// ClientIP 取请求来源 IP。
//
// 只信 `RemoteAddr`：本服务是**本地单机**网关，前面没有反向代理，
// 采信 `X-Forwarded-For` 等于让客户端自带伪造的 IP 绕过失败锁定。
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
