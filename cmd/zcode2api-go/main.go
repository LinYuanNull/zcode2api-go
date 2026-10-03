// Command zcode2api-go 是 ZCode 网关的独立纯 Go 实现。
//
// 子命令：
//
//	serve          启动网关与管理面板（默认，等价于不带子命令）
//	login          账号登录（OAuth 设备码流程）
//	claim          立即执行一次套餐领取
//	set-admin-key  设置管理面板密码
//	version        打印版本
//	help           显示帮助
//
// 分期方案见 ModelMux 仓库的 docs/zcode-native-port-plan.md。
// 当前进度：A0 骨架 ✅ / A1 契约固化 ✅ / A2 账号池与存储 ✅ / A3 管理 API ✅ /
// A4 转发链路 ✅ / **A5 登录链路 ✅（额度、领取待做）**；A6 验证码、A7 发版待做。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/LinYuanNull/zcode2api-go/internal/appdir"
	"github.com/LinYuanNull/zcode2api-go/internal/buildinfo"
	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/gateway"
	"github.com/LinYuanNull/zcode2api-go/internal/server"
	"github.com/LinYuanNull/zcode2api-go/internal/settings"
	"github.com/LinYuanNull/zcode2api-go/internal/store"
)

const usage = `zcode2api-go —— ZCode 网关（独立纯 Go 实现）

用法:
  zcode2api-go [子命令] [参数]

子命令:
  serve           启动网关与管理面板（默认）
  login           账号登录（OAuth 设备码流程）
  claim           立即执行一次套餐领取
  set-admin-key   设置管理面板密码
  version         打印版本
  help            显示本帮助

不带子命令时等价于 serve。

serve 的参数（带 $ 的项可用环境变量给默认值；上游 serve 没有任何命令行参数，
配置全走环境变量，所以下面凡带 $ 的都按「同名环境变量」对齐上游语义）:
  --host          监听地址（$ZCODE_HOST，默认 127.0.0.1）
  --port          监听端口（$ZCODE_PORT，默认 3000）
  --data-dir      数据目录（$ZCODE_DATA_DIR，默认 <exe>/data）
  --panel-dir     管理面板静态资源目录
                  （$ZCODE_PANEL_DIR / $ZCODE_FRONTEND_DIR，默认 <exe>/frontend）
  --admin-key     首启写入的后台密码（$ZCODE_ADMIN_KEY；之后以库为准）
  --gateway-key   网关 Key（**本项目扩展**，上游无对应环境变量；留空 = 不校验）
  --models        逗号分隔的模型表，覆盖内置常量

  $ZCODE_QUOTA_REFRESH_INTERVAL  首启写入的额度刷新间隔（默认 1800）
  $ZCODE_ACCOUNT_CONCURRENCY     首启写入的账号并发（默认 2）
  $ZCODE_CLAIM_ROUND_INTERVAL    首启写入的领取轮次间隔（默认 3600）

  以上四项（admin_key / 三个整数）都是**首启默认值**：库里有值就以库为准。

set-admin-key 的用法:
  zcode2api-go set-admin-key <新密码> [--data-dir 目录]
`

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "login":
		err = runLogin(args)
	case "claim":
		err = runClaim(args)
	case "set-admin-key":
		err = runSetAdminKey(args)
	case "version", "-v", "--version":
		fmt.Printf("zcode2api-go %s\n", buildinfo.Version)
		return
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n\n%s", cmd, usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "zcode2api-go: %v\n", err)
		os.Exit(1)
	}
}

// ── serve ───────────────────────────────────────────────────

func runServe(args []string) error {
	fs := newFlagSet("serve")
	var (
		host       = fs.String("host", "", "监听地址（$ZCODE_HOST）")
		port       = fs.Int("port", 0, "监听端口（$ZCODE_PORT）")
		dataDir    = fs.String("data-dir", "", "数据目录（$ZCODE_DATA_DIR）")
		panelDir   = fs.String("panel-dir", "", "面板资源目录（$ZCODE_PANEL_DIR）")
		adminKey   = fs.String("admin-key", "", "首启写入的后台密码（$ZCODE_ADMIN_KEY）")
		gatewayKey = fs.String("gateway-key", "", "网关 Key（本项目扩展，无对应环境变量）")
		models     = fs.String("models", "", "逗号分隔的模型表")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	// 设置初值：环境变量 → constants 回落值。
	//
	// 上游 `serve` 没有任何命令行参数，配置全走环境变量；这四个 `ZCODE_*`
	// 都实测过「首启写库、之后以库为准」（observations.md #12）。
	// `ZCODE_GATEWAY_KEY` **不在这里** —— 三条独立探测都证明它不生效。
	configured := settings.NewConfigured(
		envOr(*adminKey, "ZCODE_ADMIN_KEY", ""),
		*gatewayKey,
		int64(envInt(0, "ZCODE_QUOTA_REFRESH_INTERVAL", constants.DefaultQuotaRefreshInterval)),
		int64(envInt(0, "ZCODE_ACCOUNT_CONCURRENCY", constants.DefaultAccountConcurrency)),
		int64(envInt(0, "ZCODE_CLAIM_ROUND_INTERVAL", constants.DefaultClaimRoundInterval)),
	)

	dir, err := appdir.DataDir(*dataDir)
	if err != nil {
		return err
	}
	st, err := store.Open(dir+string(os.PathSeparator)+"accounts.db",
		store.WithInitialSettings(configured),
	)
	if err != nil {
		return err
	}
	defer st.Close()

	cfg := server.Config{
		Store:      st,
		Configured: configured,
		PanelDir:   appdir.PanelDir(*panelDir),
	}
	if m := gateway.ModelsFromList(*models); m != nil {
		cfg.Models = m
	}
	srv := server.New(cfg)

	addr := net.JoinHostPort(envOr(*host, "ZCODE_HOST", "127.0.0.1"), strconv.Itoa(envInt(*port, "ZCODE_PORT", 3000)))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", addr, err)
	}

	banner(ln.Addr().String(), srv.Version(), dir, cfg.PanelDir)

	httpSrv := &http.Server{Handler: srv.Handler()}

	// 收到中断信号就优雅退出：先停止收新请求，再关库。
	// 顺序要紧 —— 反过来会让正在处理的请求撞上已关闭的库。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		fmt.Fprintln(os.Stderr, "\n正在停止…")
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// banner 打印启动信息。
//
// 形状对齐上游（同样的方框与三行内容），但**内容是本实现自己的** ——
// 版本号取自 buildinfo，且额外打印数据目录与面板目录：这两个路径搞错
// 是最难排查的部署问题（见 appdir 的包注释），启动时就说清楚。
func banner(addr, version, dataDir, panelDir string) {
	if panelDir == "" {
		panelDir = "（未配置，面板页面显示说明页）"
	}
	fmt.Printf(`
============================================
  zcode2api-go %s · Go（独立实现）
  后台管理  http://%s/admin/login
  对话端点  http://%s/v1/messages
  数据目录  %s
  面板目录  %s
============================================

`, version, addr, addr, dataDir, panelDir)
}

// ── set-admin-key ───────────────────────────────────────────

func runSetAdminKey(args []string) error {
	fs := newFlagSet("set-admin-key")
	dataDir := fs.String("data-dir", "", "数据目录（$ZCODE_DATA_DIR）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return errors.New("用法: zcode2api-go set-admin-key <新密码>")
	}
	key := strings.TrimSpace(rest[0])
	if key == "" {
		// 与接口层同一条规则（observations.md #9）。
		return errors.New("后台密码不能为空")
	}

	dir, err := appdir.DataDir(*dataDir)
	if err != nil {
		return err
	}
	st, err := store.Open(dir + string(os.PathSeparator) + "accounts.db")
	if err != nil {
		return err
	}
	defer st.Close()

	if err := st.SetMeta("admin_key", key); err != nil {
		return err
	}
	fmt.Printf("后台密码已更新（库: %s）\n", st.Path())
	return nil
}

// ── 尚未实现的子命令（A5 余额 / 领取、A6）───────────────────

// runLogin 尚未接通。OAuth 服务层（`internal/oauth`）与两条管理 API 已可用，
// 面板登录就是走它们；缺的是 **`ready` 之后把凭据落库成账号** 这一步 ——
// 它需要真实账号走完整授权才能采到，属未覆盖分支（见
// docs/contract/outbound-admin/observations.md 第九节），所以这里不假装能登录。
func runLogin(_ []string) error {
	return errors.New("login 子命令尚未接通：OAuth 链路已由管理面板承载（/admin/login），" +
		"`ready` 之后凭据落库成账号这一步未采样，见 docs/contract/outbound-admin/observations.md 第九节")
}

func runClaim(_ []string) error {
	return errors.New("claim 尚未实现：套餐领取需要额度/领取/验证码链路，属于 A5/A6 阶段")
}

// ── 参数工具 ────────────────────────────────────────────────

// newFlagSet 建一个「用法写到 stderr、不自动 os.Exit」的 FlagSet。
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// envOr 取值顺序：flag → 环境变量 → 默认值。
func envOr(flagVal, envName, def string) string {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
		return v
	}
	return def
}

// envInt 同 envOr，但值是整数；不可解析时回落默认值（上游 `_int()` 同语义）。
func envInt(flagVal int, envName string, def int) int {
	if flagVal != 0 {
		return flagVal
	}
	raw := strings.TrimSpace(os.Getenv(envName))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}
