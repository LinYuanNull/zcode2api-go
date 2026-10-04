package captcha

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Options 是 Manager 的可调项。零值取默认。
type Options struct {
	// Source 拉客户配置；nil 表示只用 Default（离线/测试）。
	Source ConfigSource
	// Solver 执行求解；nil 时 Manager.Acquire 会报「未装配求解器」。
	Solver Solver

	// CacheTTL 是配置缓存时长，默认 600s（与上游 CAPTCHA_CONFIG_CACHE_TTL 一致）。
	CacheTTL time.Duration
	// FailureCacheTTL 是**失败**结果的缓存时长，默认 60s。
	//
	// 这是本实现相对上游的一处**有意偏离**，理由是实测的代价：上游失败时
	// 不落缓存，于是每次请求都会重试一次 15s 的上游超时 —— 在断网/被墙的
	// 环境里，打开领取面板就会卡满 15s。这里给失败一个较短的 TTL：
	// 响应体与上游完全相同（都是 Default），只是把恢复延迟上界从「下一次请求」
	// 放宽到 60s。
	FailureCacheTTL time.Duration
	// FetchTimeout 是单次拉配置的上游超时，默认 15s（与上游 httpx timeout=15 一致）。
	FetchTimeout time.Duration

	// Retries 是求解重试次数，默认 4（与上游 CAPTCHA_SOLVE_RETRIES 一致）。
	Retries int
	// RetryDelay 是重试间隔，默认 3s（与上游一致）。
	RetryDelay time.Duration

	// Logf 记录过程（可为 nil）。
	Logf func(format string, args ...any)
	// Now 返回当前时间（可注入，测试用）。
	Now func() time.Time
}

func (o Options) withDefaults() Options {
	if o.CacheTTL <= 0 {
		o.CacheTTL = 600 * time.Second
	}
	if o.FailureCacheTTL <= 0 {
		o.FailureCacheTTL = 60 * time.Second
	}
	if o.FetchTimeout <= 0 {
		o.FetchTimeout = fetchTimeout
	}
	if o.Retries <= 0 {
		o.Retries = 4
	}
	if o.RetryDelay <= 0 {
		o.RetryDelay = 3 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Manager 提供验证码配置（实现 Provider）并按需求解凭据。
//
// # 为什么是「用时现解」而不是预解池
//
// 上游在后台维护一个 MIN=1 / MAX=2 的预解池（热路径永不等待），代价是
// **常驻的求解进程**：真浏览器解一次要 10–40s，池要在请求到来前就备好货。
//
// 本实现不这么做，因为**领取频率是每天一次**（上游自己的 `claim_round_interval`
// 默认 3600s，而套餐是每日限领）：为了一天一两次求解，常驻几百 MB 的浏览器 +
// 一个长期开着的调试端口不划算。计划文档也明确允许「Chromium 不必常驻，
// 可实现为用时现解」。
//
// 由此产生的三处**未实现**（都已在 PROVENANCE.md 登记，不是遗漏）：
//
//   - 预解池本身（`POOL_MIN=1` / `POOL_MAX=2`）：没有后台预热，池就永远是空的。
//   - token TTL（`CAPTCHA_TOKEN_TTL=95s`）：TTL 只对「池里躺着待用的 token」有意义，
//     现解现用的 token 从产出到交给调用方是同一瞬间，没有过期窗口。
//   - `invalidate()`（上游在收到 3007 时清池）：没有池可清。
//
// 自动领取真正接通、且实测到「一轮里需要连续解多次」之后，再按那时的观测
// 决定是否补上预热池 —— 现在补就是凭猜测。
type Manager struct {
	opts Options
	src  ConfigSource
	// solver 为 nil 时 Acquire 报错（而不是让调用方拿到空串）。
	solver Solver

	// solveSem 容量 1：求解重（起浏览器），并发调用必须串行，
	// 否则一次领取轮会同时拉起 N 个浏览器。
	solveSem chan struct{}

	mu        sync.Mutex
	cached    Config
	cachedAt  time.Time
	haveCache bool
	cacheOK   bool
	cachedErr error
	lastErr   string
}

// NewManager 建验证码管理器。
//
// 不返回 error：装配缺项一律推迟到实际用到时再报错（`Acquire` 缺求解器会报
// 「未装配求解器」）。这样「只查配置」的路径不会被求解器的缺项拖成启动失败。
func NewManager(opts Options) *Manager {
	opts = opts.withDefaults()
	return &Manager{
		opts:     opts,
		src:      opts.Source,
		solver:   opts.Solver,
		solveSem: make(chan struct{}, 1),
	}
}

// Config 实现 Provider：返回验证码 SDK 初始化参数。
//
// 这是管理面 `GET /admin/api/claim/captcha-config` 的响应体。取值来自公开目录，
// 拉不到时回落 `Default`（与上游「异常就返回 CAPTCHA_DEFAULTS」一致）。
//
// **无 ctx**：接口就是这样定义的三处之一（样本里的 handler 也不带请求态参数）；
// 内部用独立的 15s 超时，不跟着请求一起被取消。
func (m *Manager) Config() Config {
	cfg, _ := m.resolveConfig(context.Background())
	return cfg
}

// Acquire 取一枚可用的验证码凭据（`X-Aliyun-Captcha-Verify-Param` 的值）。
//
// 现解一次；失败按 `Retries` 重试。没有浏览器时**不重试**（重试不会变出浏览器）。
func (m *Manager) Acquire(ctx context.Context) (string, error) {
	if m.solver == nil {
		return "", errors.New("验证码求解器未装配")
	}
	select {
	case m.solveSem <- struct{}{}:
		defer func() { <-m.solveSem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	cfg, err := m.resolveConfig(ctx)
	if err != nil {
		// 配置拉不到不是致命错误：求解需要的是四个 SDK 参数，Default 就有。
		m.logf("拉取验证码配置失败，使用默认值求解: %v", err)
	}
	cfg = cfg.withFallbacks()

	var lastErr error
	for attempt := 1; attempt <= m.opts.Retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		param, err := m.solver.Solve(ctx, cfg)
		switch {
		case err == nil && strings.TrimSpace(param) != "":
			if attempt > 1 {
				m.logf("验证码求解成功（第 %d/%d 次尝试）", attempt, m.opts.Retries)
			}
			m.setLastErr("")
			return strings.TrimSpace(param), nil
		case err == nil:
			// 求解器没报错但也没给凭据：按超时处理（上游同样把「未产出」当失败）。
			err = ErrSolveTimeout
		}
		lastErr = err
		m.setLastErr(err.Error())
		m.logf("第 %d/%d 次求解未果: %v", attempt, m.opts.Retries, err)

		if errors.Is(err, ErrNoBrowser) {
			break // 没浏览器，重试多少次都一样
		}
		if attempt < m.opts.Retries {
			if err := sleepCtx(ctx, m.opts.RetryDelay); err != nil {
				return "", err
			}
		}
	}
	return "", fmt.Errorf("验证码求解失败: %w", lastErr)
}

// LastError 返回最近一次求解失败的原因（空串表示上次成功）。
func (m *Manager) LastError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

// Browser 返回求解器将要使用的浏览器路径（空串表示自动探测也没找到）。
// 供 CLI 自检用 —— 「验证码解不出来」第一个要看的就是它指到哪去了。
func (m *Manager) Browser() string {
	if s, ok := m.solver.(*CDPSolver); ok {
		if s.opts.Executable != "" {
			return s.opts.Executable
		}
	}
	return findBrowser()
}

func (m *Manager) setLastErr(msg string) {
	m.mu.Lock()
	m.lastErr = msg
	m.mu.Unlock()
}

func (m *Manager) logf(format string, args ...any) {
	if m.opts.Logf != nil {
		m.opts.Logf(format, args...)
	}
}

// resolveConfig 返回缓存中的配置，必要时拉一次。
//
// 成功与失败**都落缓存**（TTL 不同）：两者对调用方的可见结果都是「一个
// Config 值」，区别只在过期时间。见 Options.FailureCacheTTL 的说明。
func (m *Manager) resolveConfig(ctx context.Context) (Config, error) {
	m.mu.Lock()
	ttl := m.opts.CacheTTL
	if !m.cacheOK {
		ttl = m.opts.FailureCacheTTL
	}
	if m.haveCache && m.opts.Now().Sub(m.cachedAt) < ttl {
		cfg, err := m.cached, m.cachedErr
		m.mu.Unlock()
		return cfg, err
	}
	m.mu.Unlock()

	fctx, cancel := context.WithTimeout(ctx, m.opts.FetchTimeout)
	defer cancel()
	cfg, err := m.fetchConfig(fctx)

	m.mu.Lock()
	m.cached, m.cachedAt, m.haveCache, m.cacheOK, m.cachedErr = cfg, m.opts.Now(), true, err == nil, err
	m.mu.Unlock()
	return cfg, err
}

// sleepCtx 可被取消的等待。
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
