package oauth

import (
	"context"
	"errors"

	"github.com/LinYuanNull/zcode2api-go/internal/agent"
	"github.com/LinYuanNull/zcode2api-go/internal/constants"
)

// ErrUpstreamUnavailable 表示「发起登录需要上游调用，而当前没有可用的上游」。
var ErrUpstreamUnavailable = errors.New("登录初始化需要上游 OAuth 调用")

// Upstream 是 OAuth 链路所需的上游能力。`*agent.Client` **结构上**满足它
// （签名只涉及 context 与 agent 的值类型）。
type Upstream interface {
	OAuthInit(ctx context.Context, token, provider string) (agent.OAuthFlow, error)
	OAuthPoll(ctx context.Context, token, flowID string) (string, error)
}

// UnavailableUpstream 是 Upstream 的占位实现：明确报错，不伪造登录结果。
type UnavailableUpstream struct{}

// OAuthInit 实现 Upstream。
func (UnavailableUpstream) OAuthInit(context.Context, string, string) (agent.OAuthFlow, error) {
	return agent.OAuthFlow{}, ErrUpstreamUnavailable
}

// OAuthPoll 实现 Upstream。
func (UnavailableUpstream) OAuthPoll(context.Context, string, string) (string, error) {
	return "", ErrUpstreamUnavailable
}

// Sessions 是管理面 login 两条路由依赖的全部能力。
type Sessions interface {
	// Start 发起一次设备码登录，返回**上游给的** flow_id、授权地址与有效期（秒）。
	Start(ctx context.Context, label string) (flowID, authorizeURL string, expiresIn int, err error)
	// Poll 轮询一个会话。**未知 / 过期一律 `expired`**（不是错误，见包注释）。
	Poll(ctx context.Context, flowID string) (status, message string, acc *Account)
}

// Service 把「会话表」与「上游」缝在一起。
type Service struct {
	reg *Registry
	up  Upstream
}

// NewService 装配。`reg` 为 nil 时新建；`up` 为 nil 时用 `UnavailableUpstream`
// （于是 `Start` 明确报错，而 `Poll` 对未知 flow 仍按样本回 `expired`）。
func NewService(reg *Registry, up Upstream) *Service {
	if reg == nil {
		reg = NewRegistry()
	}
	if up == nil {
		up = UnavailableUpstream{}
	}
	return &Service{reg: reg, up: up}
}

// Registry 暴露会话表（测试与诊断用）。
func (s *Service) Registry() *Registry { return s.reg }

// Start 发起一次设备码登录。
//
// **`flow_id` 用上游返回的那个**（outbound-admin 3.6 实测：管理侧返回的 id 与上游
// init 的 `flow_id`、上游 poll 路径里的 id 三值全等）。所以这里不再是「本地生成
// 再映射」——本地只生成 **Bearer 令牌**（它不落盘、也不对外暴露）。
//
// `expiresIn` 恒为 `DefaultExpiresIn`：上游回的是 `expires_at` 时间戳，管理侧换算成
// 固定 300 秒（outbound-admin 3.4）。
func (s *Service) Start(ctx context.Context, label string) (string, string, int, error) {
	token := NewToken()
	flow, err := s.up.OAuthInit(ctx, token, constants.ProviderZAI)
	if err != nil {
		return "", "", 0, err
	}
	// 缺字段就明确报错：少一个 `authorize_url` 的会话是打不开的，
	// 与其把它登记成僵尸 flow，不如当场失败。
	if flow.FlowID == "" {
		return "", "", 0, errors.New("上游未返回 flow_id")
	}
	if flow.AuthorizeURL == "" {
		return "", "", 0, errors.New("上游未返回 authorize_url")
	}
	// 这里**不**显式写 `ExpiresAt` —— 交给 `Registry.Put` 按**表内时钟**补
	//（`DefaultExpiresIn` 秒）。这样测试注入时钟后，发起与过期判定用的是同一个时钟；
	// 若在 Start 里写 `time.Now()`，注入的时钟就对不上，过期分支将不可测。
	s.reg.Put(&Flow{
		ID:              flow.FlowID,
		Label:           label,
		Status:          StatusPending,
		Token:           token,
		PollIntervalSec: flow.PollIntervalSec,
	})
	return flow.FlowID, flow.AuthorizeURL, DefaultExpiresIn, nil
}

// Poll 轮询一个会话。
//
// 三层判定，顺序不能换：
//  1. **未知 id** ⇒ 本地 `expired`（实测：管理侧不出站，见 outbound-admin 3.5）；
//  2. **已过期 / 终态** ⇒ 直接回状态，不打上游；
//  3. **上游轮询**：逐次都打（**没有**按 `poll_interval_sec` 的时间门控）；
//     一旦某次出站失败，就**停掉该 flow 的上游轮询**并回缓存的最后状态。
func (s *Service) Poll(ctx context.Context, flowID string) (string, string, *Account) {
	f, ok := s.reg.Lookup(flowID)
	if !ok {
		return StatusExpired, "", nil
	}
	if f.Status == StatusReady || f.Status == StatusFailed {
		return f.Status, f.Message, f.Account
	}
	if s.reg.Expire(flowID) {
		return StatusExpired, "", nil
	}
	if f.UpstreamStopped || f.Token == "" {
		return f.Status, f.Message, f.Account // 回缓存的最后状态
	}

	status, err := s.up.OAuthPoll(ctx, f.Token, f.ID)
	if err != nil {
		s.reg.StopUpstream(flowID)
		return f.Status, f.Message, f.Account
	}
	if status == "" {
		return f.Status, f.Message, f.Account
	}
	// `ready` / `failed` / 上游将来新增的取值一律**原样透传**：本层不该吞掉上游
	// 的状态词汇表（UI 才是消费者，新增取值应当浮出来而不是被映射成过期）。
	//
	// ⚠️ **未覆盖**：`ready` 之后「把凭据取回来并落库成账号」这一步没有采到
	// （需要真实账号走完整授权，见 outbound-admin 第九节），所以这里**只透传状态**，
	// 不伪造账号 —— 完成态的落库属后续步骤。
	s.reg.SetStatus(flowID, status, "")
	return status, "", nil
}
