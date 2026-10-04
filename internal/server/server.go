// Package server 把各层装配成一个 HTTP 服务（A3）。
//
// 路由形状是从**运行中的靶机实测**出来的，不是猜的：
//
//	GET  /meta                  → 200 `{"version":"…"}`（**只有 version 一个键**）
//	GET  /                      → 307 → /admin → 307 → /admin/login
//	GET  /admin/{page}          → 面板页面（见 pages 包）
//	GET  /static/{path}         → 面板静态资源
//	*    /admin/api/{route}     → 管理 API（22 路由，见 adminapi 包）
//	*    /v1/{route}           → 网关（3 路由，见 gateway 包）
//
// 两条装配纪律：
//
//  1. **鉴权在路由匹配之后**：实测靶机对 `/admin/api/nope`（不带凭证）返回
//     404 `{"detail":"Not Found"}`，只有命中已知路由才校验凭证。所以鉴权被包在
//     adminapi 的**每个** handler 上，而不是包整棵 `/admin/api/` 子树。
//  2. **密钥实时读取**（`AdminKey` / `GatewayKey` 是函数而不是字符串）：
//     `PUT /admin/api/settings` 改完密码后，下一次请求就必须按新值鉴权。
//
// 服务本身不含任何业务逻辑，只做装配 —— 这样 A4 接入真实转发时，
// 只需替换 gateway 包，装配层不用动。
package server

import (
	"fmt"
	"net/http"
	"os"

	"github.com/LinYuanNull/zcode2api-go/internal/adminapi"
	"github.com/LinYuanNull/zcode2api-go/internal/agent"
	"github.com/LinYuanNull/zcode2api-go/internal/authadmin"
	"github.com/LinYuanNull/zcode2api-go/internal/buildinfo"
	"github.com/LinYuanNull/zcode2api-go/internal/captcha"
	"github.com/LinYuanNull/zcode2api-go/internal/claim"
	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/gateway"
	"github.com/LinYuanNull/zcode2api-go/internal/httpx"
	"github.com/LinYuanNull/zcode2api-go/internal/oauth"
	"github.com/LinYuanNull/zcode2api-go/internal/pages"
	"github.com/LinYuanNull/zcode2api-go/internal/quota"
	"github.com/LinYuanNull/zcode2api-go/internal/reqlog"
	"github.com/LinYuanNull/zcode2api-go/internal/settings"
	"github.com/LinYuanNull/zcode2api-go/internal/store"
)

// Config 是服务装配参数。
//
// 只有 `Store` 是必需的；其余留空都取默认值（默认值一律是「显式未实现」或
// 保守取值，不会静默放宽鉴权）。
type Config struct {
	// Store 是账号池句柄（必需）。
	Store *store.Store

	// AdminKey / GatewayKey 返回**当前**生效的密钥（实时读，不缓存）。
	// 为 nil 时从 Store 的 settings 快照读。
	AdminKey   func() string
	GatewayKey func() string

	// Configured 是本次运行配置给定的设置初值（`ZCODE_*` 环境变量）。
	// 两个用途：作为缺项回落值，以及 `admin_key_is_default` 的比较基准
	//（见 settings.Configured）。
	Configured settings.Configured

	// PanelDir 是管理面板静态资源目录（其下应有 admin/ css/ js/）。
	// 为空时面板页面走占位说明页，管理 API 不受影响。
	PanelDir string

	// Version 覆盖 `/meta` 与页面 `{{APP_VERSION}}` 的版本号；空串用 buildinfo.Version。
	Version string

	// Models 覆盖 `/v1/models` 的模型表；nil 用 gateway.DefaultModels。
	Models []gateway.Model

	// MonitoringKeep 覆盖请求监控环形容量；<=0 用 constants.MonitoringKeep。
	MonitoringKeep int

	// ── 以下为可选接缝（A5 已按要求接上，A6 待接）。
	//
	// Sessions 是管理面登录两条路由的实现。nil 时**默认接真实实现**
	// （`oauth.NewService` + 真实出站客户端）—— 这不是「未实现」，
	// A5 已把 OAuth 设备码链路接通；只有需要上游但当前不可达时才报错。
	Sessions oauth.Sessions

	// Quota 为 nil 时**默认接真实实现**（A5-3）：三条并发 + active 门控 +
	// `quota_refresh_interval` 窗口快照。它的 `200 成功体未采样` 分支会显式报错
	// （501），绝不伪造 quota/plan。显式传入时用传入的（测试用）。
	Quota quota.Refresher

	// Claimer 为 nil 时**默认接真实实现**（A5-4）：读账号池的已存状态，
	// 失效账号产出旁录样本那一行 / 那一条（零出站）；成功路径未采样 ⇒ 501。
	// 显式传入时用传入的（测试用）。
	Claimer claim.Claimer

	// Captcha 为 nil 时**默认接真实实现**（A6）：配置来自公开目录
	// `client/configs`（拉不到回落静态默认值），求解用系统已装浏览器
	// 「用时现解」（不常驻预解池，见 captcha.Manager 的包注释）。
	// 显式传入时用传入的（测试用 —— 契约回放注入 Static 以免 CI 依赖网络/浏览器）。
	Captcha captcha.Provider
}

// Server 是装配好的 HTTP 服务。
type Server struct {
	handler  http.Handler
	api      *adminapi.API
	gateway  *gateway.Gateway
	panel    *pages.Handler
	ring     *reqlog.Ring
	settings *settings.Cache
	guard    *authadmin.Guard
	version  string

	// quotaSvc 是默认装配出来的额度服务（`Config.Quota` 显式给了就是 nil）。
	// 只有「启动自刷」需要它的具体类型（`RefreshActives`）——
	// 请求路径一律走 `quota.Refresher` 接口。
	quotaSvc *quota.Service
}

// New 装配服务。Config.Store 为 nil 时 panic —— 装配错误必须在启动时暴露，
// 不能等到第一个请求。
func New(cfg Config) *Server {
	if cfg.Store == nil {
		panic("server: Config.Store 不能为空")
	}
	version := cfg.Version
	if version == "" {
		version = buildinfo.Version
	}
	keep := cfg.MonitoringKeep
	if keep <= 0 {
		keep = constants.MonitoringKeep
	}

	cache := settings.NewCache(cfg.Store, cfg.Configured)

	// 密钥默认从设置快照读。读失败时返回空串 ⇒ 管理面鉴权一律 401
	// （fail-closed；见 authadmin.Guard：空 key 永不匹配）。
	adminKey := cfg.AdminKey
	if adminKey == nil {
		adminKey = func() string {
			s, err := cache.Get()
			if err != nil {
				return ""
			}
			return s.AdminKey
		}
	}
	gatewayKey := cfg.GatewayKey
	if gatewayKey == nil {
		gatewayKey = func() string {
			s, err := cache.Get()
			if err != nil {
				return ""
			}
			return s.GatewayKey
		}
	}

	guard := authadmin.New(adminKey)
	ring := reqlog.New(keep)

	// 登录链路默认接真实上游（A5-2）：会话表 + 出站客户端。
	//
	// 出站客户端是**同一条**走环境代理的实例（A5 实测：管理侧出站含 billing 也走
	// `HTTPS_PROXY`，不存在直连分支）。配置显式给了 Sessions 就用它（测试用）。
	sessions := cfg.Sessions
	if sessions == nil {
		sessions = oauth.NewService(oauth.NewRegistry(), agent.New())
	}

	// 额度查询（A5-3）：默认接真实实现。`quota_refresh_interval` 取**启动快照**
	// （实测：该值在进程启动时快照，运行期改设置不生效，observations.md 4.4）。
	var quotaSvc *quota.Service
	quotaRefresher := cfg.Quota
	if quotaRefresher == nil {
		var interval int64
		if s, err := cache.Get(); err == nil {
			interval = s.QuotaRefreshInterval
		}
		quotaSvc = quota.NewService(agent.New(), cfg.Store, interval)
		quotaRefresher = quotaSvc
	}

	// 领取（A5-4）：默认接真实实现。它**只读账号池的已存状态**（实测
	// `claim/preview` / `claim` / `claim/manual` 三条零出站），失效账号走有样本的
	// 分支；真正要打上游的成功路径未采样 ⇒ 显式 501，绝不伪造领取结果。
	claimer := cfg.Claimer
	if claimer == nil {
		claimer = claim.NewService(cfg.Store)
	}

	// 验证码（A6）：默认接真实实现。配置来自公开目录（同一条走环境代理的出站
	// 客户端，A5 实测管理侧出站无直连分支），拉不到回落静态默认值；求解走
	// `captcha.CDPSolver`（系统已装浏览器 + 用时现解）。
	//
	// 配置读取本身**不触网就能装配**：NewManager 延迟到第一次 Config()/Acquire()
	// 才去拉，所以这里构造不会拖慢启动，也不会因离线上游而启动失败。
	captchaProvider := cfg.Captcha
	if captchaProvider == nil {
		captchaProvider = captcha.NewManager(captcha.Options{
			Source: agent.New(),
			Solver: captcha.NewCDPSolver(captcha.SolveOptions{Logf: captchaLogf}),
			Logf:   captchaLogf,
		})
	}

	api := adminapi.New(adminapi.Deps{
		Store:      cfg.Store,
		Guard:      guard,
		Ring:       ring,
		Sessions:   sessions,
		Quota:      quotaRefresher,
		Claimer:    claimer,
		Captcha:    captchaProvider,
		Settings:   cache,
		Configured: cfg.Configured,
	})

	gw := gateway.New(gateway.Options{
		Store:      cfg.Store,
		GatewayKey: gatewayKey,
		Models:     cfg.Models,
	})

	panel := &pages.Handler{Dir: cfg.PanelDir, Version: version}

	mux := http.NewServeMux()
	mux.HandleFunc("/meta", metaHandler(version))
	mux.Handle("/admin/api/", api)
	mux.Handle("/v1/", gw)
	// 兜底：面板宿主自己处理 `/`、`/admin`、`/admin/{page}`、`/static/*`，
	// 其余一律 404 `{"detail":"Not Found"}`（与靶机同形）。
	mux.Handle("/", panel)

	return &Server{
		handler:  mux,
		api:      api,
		gateway:  gw,
		panel:    panel,
		ring:     ring,
		settings: cache,
		guard:    guard,
		version:  version,
		quotaSvc: quotaSvc,
	}
}

// BootRefresh 执行「启动自刷」：对池里全部 `active` 的 JWT 账号各查一次额度。
// 返回实际尝试的账号数（0 表示池里没有可查的账号）。
//
// 依据：observations.md 4.4 第 2 条（触发点之一是**启动**）。
//
// 为什么由调用方决定同步 / 异步：这是**唯一的**会在无人请求时发出上游流量的动作，
// 藏进 `New()` 里就无法验收「启动到底发了几条出站」。`cmd` 用 `go` 起它。
// 显式注入过 `Config.Quota` 时返回 0（那不是本服务的额度实现，无权自刷）。
func (s *Server) BootRefresh() int {
	if s.quotaSvc == nil {
		return 0
	}
	return s.quotaSvc.RefreshActives()
}

// Handler 返回装配好的 http.Handler。
func (s *Server) Handler() http.Handler { return s.handler }

// Version 返回本进程对外报告的版本号。
func (s *Server) Version() string { return s.version }

// Ring 返回请求监控环形缓冲（A4 把网关请求写进去）。
//
// A3 阶段**不写入**任何条目：条目的字段集未采样（A1 的 monitoring 响应是空的），
// 凭 notes 里的 `id/model/stream/prompt/status/…` 编一套字段会污染契约。
// 拿到真实网关流量后再补采 —— 见 PROVENANCE.md「A3 未覆盖的分支」。
func (s *Server) Ring() *reqlog.Ring { return s.ring }

// captchaLogf 把验证码链路（拉配置、起浏览器、求解）的过程写到 stderr。
//
// 前缀与 CLI 的其它诊断一致（`zcode2api-go: …`）。**刻意不用标准库 log**：
// 它的默认 logger 会带上本机时区时间戳，而这些行只在排查「验证码解不出来」
// 时才有人看，与请求日志（reqlog）混在一起反而更难认。写成一行一事。
func captchaLogf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "zcode2api-go: captcha: "+format+"\n", args...)
}

// metaResponse 是 `GET /meta` 的响应体。**只有一个键**（实测靶机如此）。
type metaResponse struct {
	Version string `json:"version"`
}

// metaHandler 是健康/版本端点。ModelMux 的托管渠道把 `health_path` 配成它。
func metaHandler(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			httpx.WriteDetail(w, http.StatusMethodNotAllowed, "Method Not Allowed")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, metaResponse{Version: version})
	}
}
