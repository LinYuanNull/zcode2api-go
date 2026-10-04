// A5-3：额度查询真实落地。
//
// 全部语义来自 `docs/contract/outbound-admin/observations.md` 第四节（实测）：
//
//  1. **一次刷新 = 三条并发**（`zcode-plan/usage` + `billing/current` +
//     `billing/balance`），同批**共用同一个 `X-Request-Id`**（不同批不同）。
//  2. **只对 `status=active` 的 JWT 账号查上游**；转 `invalid` 后**永不再查**。
//  3. 凭据失效的状态码**三个端点各不相同**（`usage` → 404，两条 billing → 401），
//     所以「凭据失效」必须**按端点分别判定**，不能只认 401。
//  4. `refresh` 的响应有**两种形态**，判据是**键名**（`result` vs `message`）。
//
// 一条**硬纪律**：凭据有效时那三条的 `200` 成功体**本轮未采样**，
// 因此「真的查到了」这条分支**必须显式报错**，绝不伪造 `quota` / `plan` / `plans`
// （见 observations.md 第九节第 3 条与 PROVENANCE.md）。
package quota

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/LinYuanNull/zcode2api-go/internal/agent"
	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/identity"
	"github.com/LinYuanNull/zcode2api-go/internal/models"
)

// ErrSuccessShapeUnsampled 表示「上游三条都返回了 2xx」，而这一分支的响应结构
// 本轮**没有采到**（采样机的假 JWT 签名无效，只能诱发 401/404）。
//
// 之所以不能近似处理：把上游 `data` 原样塞进 `quota` / `plan` / `plans`
// 是**猜映射**（哪个端点对应哪个字段、要不要换算，全不知道）。
// 猜错的表现是面板上配额数字整体错位 —— 比明确报错危险得多。
var ErrSuccessShapeUnsampled = errors.New(
	"上游额度查询成功（2xx），但成功响应结构未采样：无法把它映射到账号的 quota/plan/plans，" +
		"见 docs/contract/outbound-admin/observations.md 第九节第 3 条")

// Upstream 是额度查询所需的上游能力。`*agent.Client` **结构上**满足它。
//
// 三条路径**按设计都不读响应体**返回（`resp.Body` 未读）—— 状态码由本层分派，
// 错误体自己用 `agent.ReadBody` 解压（出站头显式发了 `Accept-Encoding`，
// 所以 Go 的透明解压不生效）。
type Upstream interface {
	GetPlanUsage(ctx context.Context, jwt, requestID string, fp identity.QuotaFingerprint) (*http.Response, error)
	GetBillingCurrent(ctx context.Context, jwt, requestID string, fp identity.QuotaFingerprint) (*http.Response, error)
	GetBillingBalance(ctx context.Context, jwt, requestID string, fp identity.QuotaFingerprint) (*http.Response, error)
}

// Accounts 是额度刷新需要的账号池能力。
//
// 为什么不是「只传账号进去、把结果还回来」：刷新会**改写账号状态**
// （凭据失效 ⇒ `status=invalid` + `last_error` + `last_checked_at`），
// 而状态机没有别的落点 —— `store` 是唯一真源，中间层不能自己保存一份。
type Accounts interface {
	Get(id string) (models.Account, bool)
	Put(a models.Account) error
	List() []models.Account
}

// Service 是 A5-3 的真实实现。
type Service struct {
	up    Upstream
	accts Accounts

	// interval 是 `quota_refresh_interval`（秒）。**进程启动时快照**
	// （observations.md 4.4 末段）：运行期改设置不影响本进程，与靶机一致。
	// 置 0 表示不设窗口 —— `active` 账号每次都真查。
	interval int64

	now          func() float64
	newRequestID func() string
}

// NewService 装配。`interval` 取 `settings.QuotaRefreshInterval` 的启动快照。
func NewService(up Upstream, accts Accounts, interval int64) *Service {
	if up == nil {
		up = UnavailableUpstream{}
	}
	return &Service{
		up:           up,
		accts:        accts,
		interval:     interval,
		now:          models.EpochNow,
		newRequestID: agent.NewRequestID,
	}
}

// SetClock 注入时钟（测试用）。返回值便于链式调用。
func (s *Service) SetClock(now func() float64) *Service {
	s.now = now
	return s
}

// SetRequestID 注入 `X-Request-Id` 生成器（测试用；生产用 `agent.NewRequestID`）。
func (s *Service) SetRequestID(f func() string) *Service {
	s.newRequestID = f
	return s
}

// UnavailableUpstream 是 Upstream 的占位实现：明确报错，不伪造额度。
type UnavailableUpstream struct{}

// GetPlanUsage 实现 Upstream。
func (UnavailableUpstream) GetPlanUsage(context.Context, string, string, identity.QuotaFingerprint) (*http.Response, error) {
	return nil, ErrUpstreamUnavailable
}

// GetBillingCurrent 实现 Upstream。
func (UnavailableUpstream) GetBillingCurrent(context.Context, string, string, identity.QuotaFingerprint) (*http.Response, error) {
	return nil, ErrUpstreamUnavailable
}

// GetBillingBalance 实现 Upstream。
func (UnavailableUpstream) GetBillingBalance(context.Context, string, string, identity.QuotaFingerprint) (*http.Response, error) {
	return nil, ErrUpstreamUnavailable
}

// Refresh 刷新一个账号的额度。判定顺序**不能换**（每一条都对应实测事实）：
//
//  1. **非 active 的 JWT 账号一律不出站**，回 CACHED。
//     `invalid` 是实测（转 invalid 后永不再查）；`disabled` / `cooling` / `exhausted`
//     是「只对 active 查上游」这条门控的推论（见 PROVENANCE 的推断清单）。
//  2. **active 且命中 `quota_refresh_interval` 窗** ⇒ 不出站，回 CACHED。
//  3. 否则**真查上游**（三条并发）：凭据失效 ⇒ 落 `invalid` 后回 FRESH；
//     三条全 2xx ⇒ `ErrSuccessShapeUnsampled`（不伪造）。
func (s *Service) Refresh(acc models.Account) (Result, error) {
	if !SupportsRefresh(acc) {
		// 非 JWT 账号不走上游：样本 `10-account-refresh-nonjwt.POST.json` 的
		// 200 + `{ok:false,message}` 由调用方直接装配，这里只是兜底。
		return Result{OK: false, Message: NonJWTMessages}, nil
	}
	if acc.Status != constants.StatusActive {
		return s.cached(acc), nil
	}
	if s.withinWindow(acc) {
		return s.cached(acc), nil
	}
	return s.probe(acc)
}

// cached 组装「未出站」的 CACHED 形态。
//
// `message` 取账号自己存的 `last_error`。
//
// ⚠️ **推断**：真实采到 CACHED 形态的那次（`refresh#2`）账号已经 `invalid`，
// 所以 `message` 恰好等于 `InvalidCredentialMessage`；而「`active` 但命中时间窗」
// 这一条只采到了**行为**（不出站），**文案未采样**。这里统一用账号的 `last_error`
// 是最保守的选择：它是网关自己写的、可解释的一句话，而不是新造一句文案。
func (s *Service) cached(acc models.Account) Result {
	msg := ""
	if acc.LastError != nil {
		msg = *acc.LastError
	}
	a := acc
	return Result{OK: false, Message: msg, Account: &a}
}

// withinWindow 报告一个 active 账号是否还在 `quota_refresh_interval` 窗内。
//
// 窗口的**起算点**取 `last_checked_at`（observations.md 第九节第 6 条明确
// 「窗口从哪一刻起算」未钉死；账号上只有这一个可用的时间戳，且它正是每次查询
// 都会刷新的一次记录）。`interval <= 0` 表示不设窗。
func (s *Service) withinWindow(acc models.Account) bool {
	if s.interval <= 0 || acc.LastCheckedAt == nil {
		return false
	}
	return s.now()-*acc.LastCheckedAt < float64(s.interval)
}

// probeStatus 是三条并发探测中一条的可观测结果。
type probeStatus struct {
	status int
	body   []byte
	err    error
}

// probe 真查上游：三条并发、共用同一个 `X-Request-Id`。
func (s *Service) probe(acc models.Account) (Result, error) {
	jwt := acc.Credential()
	if jwt == "" {
		return Result{}, errors.New("账号没有可用的 JWT 凭据")
	}
	fp := quotaFingerprint(acc.Fingerprint)
	requestID := s.newRequestID()

	ctx := context.Background()
	var (
		wg      sync.WaitGroup
		results [3]probeStatus
	)
	calls := [3]func(context.Context, string, string, identity.QuotaFingerprint) (*http.Response, error){
		s.up.GetPlanUsage,
		s.up.GetBillingCurrent,
		s.up.GetBillingBalance,
	}
	for i, call := range calls {
		wg.Add(1)
		go func(i int, call func(context.Context, string, string, identity.QuotaFingerprint) (*http.Response, error)) {
			defer wg.Done()
			results[i] = doProbe(ctx, call, jwt, requestID, fp)
		}(i, call)
	}
	wg.Wait()

	invalid, err := classifyProbes(results)
	if err != nil {
		return Result{}, err
	}
	if !invalid {
		// 三条都是 2xx ⇒ 成功体未采样。**不伪造**。
		return Result{}, ErrSuccessShapeUnsampled
	}

	// 凭据失效：落状态机后再回 FRESH 形态（实测：`account.status` 转 `invalid`、
	// `last_error` 写 `凭证失效，请重新授权`、`last_checked_at` 刷新）。
	now := s.now()
	msg := InvalidCredentialMessage
	updated := acc
	updated.Status = constants.StatusInvalid
	updated.LastError = &msg
	updated.LastCheckedAt = &now
	if s.accts != nil {
		if err := s.accts.Put(updated); err != nil {
			return Result{}, fmt.Errorf("写入账号状态失败: %w", err)
		}
	}
	out := updated
	return Result{OK: false, Failure: InvalidCredentialMessage, Account: &out}, nil
}

// doProbe 打一条端点，把状态码与**解压后的**错误体读出来。
func doProbe(
	ctx context.Context,
	call func(context.Context, string, string, identity.QuotaFingerprint) (*http.Response, error),
	jwt, requestID string,
	fp identity.QuotaFingerprint,
) probeStatus {
	resp, err := call(ctx, jwt, requestID, fp)
	if err != nil {
		return probeStatus{err: err}
	}
	// 非 2xx 才需要体（用于诊断）；2xx 的体**绝不解析**（结构未采样）。
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		agent.DrainAndClose(resp)
		return probeStatus{status: resp.StatusCode}
	}
	body, rerr := agent.ReadBody(resp)
	if rerr != nil {
		return probeStatus{status: resp.StatusCode, err: rerr}
	}
	return probeStatus{status: resp.StatusCode, body: body}
}

// classifyProbes 把三条探测结果收敛成「凭据失效 / 三条都成功」二选一。
//
// 「凭据失效」的判据**按端点分别取**（observations.md 4.3 实测三码不一致）：
//
//	zcode-plan/usage         → 404
//	zcode-plan/billing/current → 401
//	zcode-plan/billing/balance → 401
//
// 因此这里接受 **401 或 404**，但**必须把它钉到具体端点**上：只有
// 「第 0 条 404、第 1/2 条 401」这种形状才与实测一致；其它组合（例如三条都 500、
// 或 usage 回 401）属**未采样**，一律走 error 分支由上层显式报错。
func classifyProbes(r [3]probeStatus) (invalid bool, err error) {
	for i, p := range r {
		if p.err != nil {
			return false, fmt.Errorf("额度查询出站失败（端点 %d）: %w", i, p.err)
		}
	}
	usageInvalid := r[0].status == http.StatusNotFound
	billingInvalid := r[1].status == http.StatusUnauthorized && r[2].status == http.StatusUnauthorized
	switch {
	case usageInvalid && billingInvalid:
		return true, nil
	case is2xx(r[0].status) && is2xx(r[1].status) && is2xx(r[2].status):
		return false, nil // 交给调用方报 ErrSuccessShapeUnsampled
	default:
		// 其余组合（含「usage 成功但 billing 401」这种半成功）本轮**未采样** ——
		// 报错而不是挑一条近似分支走。**同时把三条状态码带出来**，便于诊断。
		return false, fmt.Errorf("未采样的额度查询状态组合: usage=%d current=%d balance=%d"+
			"（已采样的是 usage=404 + billing=401/401，见 observations.md 4.3）",
			r[0].status, r[1].status, r[2].status)
	}
}

func is2xx(code int) bool { return code >= 200 && code <= 299 }

// quotaFingerprint 把账号的设备档案投影成额度查询的指纹头（少一个 `screen`）。
func quotaFingerprint(f models.Fingerprint) identity.QuotaFingerprint {
	return identity.QuotaFingerprint{
		Platform:  f.Platform,
		Arch:      f.Arch,
		OSVersion: f.OSVersion,
		Language:  f.Language,
		Timezone:  f.Timezone,
		DeviceMID: f.DeviceMID,
	}
}

// RefreshActives 对池里**全部 `status=active` 的 JWT 账号**各刷一次，
// 返回实际尝试的账号数。供「启动自刷」触发点使用（observations.md 4.4 第 2 条）。
//
// **故意是同步的**：调用方（`main`）自己决定放不放 goroutine。把并发藏进库里
// 会让「启动时到底发了几条出站」变得不可观测，而这条正是要验收的事实。
func (s *Service) RefreshActives() int {
	if s.accts == nil {
		return 0
	}
	n := 0
	for _, acc := range s.accts.List() {
		if !SupportsRefresh(acc) || acc.Status != constants.StatusActive {
			continue
		}
		n++
		_, _ = s.Refresh(acc) // 启动自刷的结果不对外暴露（靶机的启动自刷也无人接收）
	}
	return n
}
