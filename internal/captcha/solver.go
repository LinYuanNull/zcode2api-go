package captcha

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LinYuanNull/zcode2api-go/internal/captcha/cdp"
)

// Solver 解一次验证码，返回可放进 `X-Aliyun-Captcha-Verify-Param` 的凭据。
type Solver interface {
	Solve(ctx context.Context, cfg Config) (string, error)
}

// ErrNoBrowser 表示系统里没找到可用的浏览器（求解无法进行，但不该当成上游故障）。
var ErrNoBrowser = errors.New("未找到可用的浏览器")

// ErrSolveTimeout 表示求解超时（页面内未在时限内产出凭据）。
var ErrSolveTimeout = errors.New("验证码求解超时")

// SolveOptions 是 CDPSolver 的可调项。零值取默认。
type SolveOptions struct {
	// Executable 覆盖浏览器路径；空表示自动探测（$ZCODE_CHROMIUM_PATH → Edge → Chrome）。
	Executable string
	// UserAgent 覆盖 UA；空表示用 win32-x64 档案（见 solverUA）。
	UserAgent string
	// Timeout 是**单次**求解的总预算。
	Timeout time.Duration
	// PageGate 是页面内的兜底超时（SDK 既不回调成功也不回调失败时的出口）。
	PageGate time.Duration
	// ReadyTimeout 是等「文档就绪 + SDK 加载完成」的时限。
	ReadyTimeout time.Duration
	// LaunchTimeout 是等 DevTools 端点起来的时限。
	LaunchTimeout time.Duration
	// Logf 记录过程（可为 nil）。
	Logf func(format string, args ...any)
}

func (o SolveOptions) withDefaults() SolveOptions {
	if o.Timeout <= 0 {
		// 单次求解预算。上游 Node 侧是 launch 90s + protocol 60s + 页面 20s 的层层
		// 叠加（最坏可到分钟级）；这里给一个统一预算，够用且更容易解释超时。
		o.Timeout = 120 * time.Second
	}
	if o.PageGate <= 0 {
		o.PageGate = 20 * time.Second // 与上游页面内 setTimeout(20000) 一致
	}
	if o.ReadyTimeout <= 0 {
		o.ReadyTimeout = 20 * time.Second // 与上游 waitForFunction 的 20000ms 一致
	}
	if o.LaunchTimeout <= 0 {
		o.LaunchTimeout = 30 * time.Second
	}
	if o.UserAgent == "" {
		o.UserAgent = solverUA
	}
	return o
}

// solverUA 是求解会话的 UA。
//
// **刻意不用「真实浏览器版本」**：这条 UA 要与账号档案（win32-x64）以及
// billing/claim 链路的指纹头保持同一套说法。无痕验证看的是「这个会话的各项
// 信号是否自洽」，一个自称 Edge 154 的 UA 配一套声称 win32-x64 的接口头，
// 比「版本略旧但处处一致」更容易被标记。
//
// 与 CDP 的 `Emulation.setUserAgentOverride` 配合使用：页面侧读到的
// `navigator.userAgent` 与 `userAgentData` 都是被覆盖后的值，因此从页面自身
// 看也不存在矛盾。
const solverUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/127.0.0.0 Safari/537.36"

// CDPSolver 用系统浏览器 + 自写 CDP 客户端解一次验证码。
//
// 生命周期是「**用时现解**」：一次求解 = 起一个独立 profile 的 headless 浏览器
// → 打开求解页 → 取凭据 → 关目标 → 浏览器自行退出。**没有常驻浏览器**：
// 领取频率是每天一次，常驻进程既浪费几百 MB 内存，也会在无人值守的机器上
// 长期占用一个调试端口。
type CDPSolver struct {
	opts SolveOptions
}

// NewCDPSolver 建求解器。
func NewCDPSolver(opts SolveOptions) *CDPSolver {
	return &CDPSolver{opts: opts.withDefaults()}
}

func (s *CDPSolver) logf(format string, args ...any) {
	if s.opts.Logf != nil {
		s.opts.Logf(format, args...)
	}
}

// Solve 跑完整条链路。失败时返回的错误会带上浏览器 stderr 的尾部，
// 否则「浏览器起来了但什么都没发生」这类问题无从下手。
func (s *CDPSolver) Solve(ctx context.Context, cfg Config) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()

	exe := s.opts.Executable
	if exe == "" {
		exe = findBrowser()
	}
	if exe == "" {
		return "", fmt.Errorf("%w（找过: %s）", ErrNoBrowser, browserHint())
	}
	s.logf("求解器使用浏览器: %s", exe)

	// 临时目录要先建：求解页与浏览器 profile 都放里面，进程退出后整棵删掉。
	workDir, err := os.MkdirTemp("", "zcode2api-captcha-")
	if err != nil {
		return "", fmt.Errorf("创建求解临时目录失败: %w", err)
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	pageURL, err := writeSolverPage(workDir)
	if err != nil {
		return "", err
	}
	profileDir := filepath.Join(workDir, "profile")

	proc, err := s.startBrowser(ctx, exe, workDir, profileDir)
	if err != nil {
		return "", err
	}
	// 注册在 RemoveAll 之后 ⇒ LIFO 先执行：浏览器必须先死，profile 目录才删得掉。
	defer proc.close()

	param, err := s.drive(ctx, proc.client, pageURL, cfg)
	if err != nil {
		return "", fmt.Errorf("%w（浏览器 stderr 尾部: %s）", err, proc.stderr.tail(12))
	}
	if strings.TrimSpace(param) == "" {
		return "", fmt.Errorf("%w：页面未产出验证码凭据（浏览器 stderr 尾部: %s）", ErrSolveTimeout, proc.stderr.tail(12))
	}
	return strings.TrimSpace(param), nil
}

// browserProc 是一次求解期间那个「临时浏览器」：进程 + 调试会话 + 排空的 stderr。
type browserProc struct {
	client *cdp.Client
	stderr *lineRing

	// httpBase 是 DevTools 的 HTTP 根（`http://127.0.0.1:端口`），
	// 留着是为了排查：`/json/protocol` 能问出这个浏览器**实际支持**的命令与参数。
	httpBase string

	cmd    *exec.Cmd
	exited chan struct{}
}

// startBrowser 起一个 headless 浏览器、等 DevTools HTTP 端点就绪、连上浏览器级
// WebSocket。返回的 browserProc 必须 close()（幂等）。
//
// 抽成独立方法有两个理由：`Solve` 因此只剩「建目录 → 起浏览器 → 走 7 条命令」；
// 以及真机测试（`ZCODE_CAPTCHA_LIVE=1`）能复用同一套启动参数，避免「测的不是
// 跑的那套参数」。
func (s *CDPSolver) startBrowser(ctx context.Context, exe, workDir, profileDir string) (*browserProc, error) {
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建浏览器 profile 目录失败: %w", err)
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, exe, browserArgs(port, profileDir, headlessArg())...)
	cmd.Dir = workDir
	// 浏览器 stderr 必须持续排空：管道写满后子进程会阻塞在写 stderr 上，
	// 表现为「进程还在、但什么都不发生」。
	stderr := &lineRing{max: 60}
	pipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("接浏览器 stderr 失败: %w", err)
	}
	cmd.Stdout = io.Discard
	// 超时/取消时先 Kill，再给 3 秒让 Wait 收尸；否则管道不会关，Wait 会挂住。
	cmd.WaitDelay = 3 * time.Second

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动浏览器失败（%s）: %w", exe, err)
	}
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = cmd.Wait()
	}()
	go drainLines(pipe, stderr)

	p := &browserProc{stderr: stderr, httpBase: fmt.Sprintf("http://127.0.0.1:%d", port), cmd: cmd, exited: exited}

	wsURL, err := waitDevTools(ctx, p.httpBase, exited, s.opts.LaunchTimeout)
	if err != nil {
		p.close()
		return nil, fmt.Errorf("%w（浏览器 stderr 尾部: %s）", err, stderr.tail(12))
	}

	client, err := cdp.Dial(ctx, wsURL)
	if err != nil {
		p.close()
		return nil, fmt.Errorf("连接 DevTools 失败: %w", err)
	}
	p.client = client
	return p, nil
}

// close 关调试连接并确保浏览器退出（幂等）。
func (p *browserProc) close() {
	if p == nil {
		return
	}
	if p.client != nil {
		p.client.Close()
	}
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	// 收尾顺序：先关调试连接（目标关掉后浏览器通常会自行退出），再兜底 Kill。
	_ = p.cmd.Process.Kill()
	select {
	case <-p.exited:
	case <-time.After(5 * time.Second):
	}
}

// drive 在已连上的会话里走完 7 条命令。
func (s *CDPSolver) drive(ctx context.Context, client *cdp.Client, pageURL string, cfg Config) (string, error) {
	target, err := client.CreateTarget(ctx, "")
	if err != nil {
		return "", fmt.Errorf("创建页面目标失败: %w", err)
	}
	// 目标一定关：它是这次求解唯一的额外资源，留在浏览器里会让浏览器不退出。
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.CloseTarget(closeCtx, target)
	}()

	session, err := client.AttachToTarget(ctx, target)
	if err != nil {
		return "", fmt.Errorf("附着页面目标失败: %w", err)
	}

	// 顺序要紧：补丁与 UA 覆盖都必须**早于**导航，否则只作用于下一次文档加载。
	if err := client.AddScriptOnNewDocument(ctx, session, antiDetectJS); err != nil {
		return "", fmt.Errorf("注入反探测补丁失败: %w", err)
	}
	if err := client.SetUserAgentOverride(ctx, session, uaOverride(s.opts.UserAgent)); err != nil {
		return "", fmt.Errorf("覆盖 UA 失败: %w", err)
	}

	if _, err := client.Navigate(ctx, session, pageURL); err != nil {
		return "", fmt.Errorf("打开求解页失败: %w", err)
	}
	if err := s.waitReady(ctx, client, session); err != nil {
		return "", err
	}
	s.logf("求解页就绪，开始无痕验证")

	// 页面内兜底超时留 5s 余量，保证它先于本层超时落定 —— 否则我们拿到的是
	// 「CDP 调用超时」，看不到页面到底回调了什么。
	gate := s.opts.PageGate
	evalCtx, cancel := context.WithTimeout(ctx, gate+5*time.Second)
	defer cancel()

	param, err := client.EvaluateString(evalCtx, session, solveExpr(cfg, int(gate/time.Millisecond)), true)
	if err != nil {
		return "", fmt.Errorf("执行求解表达式失败: %w", err)
	}
	return param, nil
}

// waitReady 等文档加载完 + SDK 全局函数就位。
func (s *CDPSolver) waitReady(ctx context.Context, client *cdp.Client, session string) error {
	readyCtx, cancel := context.WithTimeout(ctx, s.opts.ReadyTimeout)
	defer cancel()

	if err := poll(readyCtx, 100*time.Millisecond, func() (bool, error) {
		state, err := client.EvaluateString(readyCtx, session, exprReadyState, false)
		if err != nil {
			return false, err
		}
		return state == "complete", nil
	}); err != nil {
		return fmt.Errorf("求解页未在 %s 内加载完成: %w", s.opts.ReadyTimeout, err)
	}

	if err := poll(readyCtx, 200*time.Millisecond, func() (bool, error) {
		typ, err := client.EvaluateString(readyCtx, session, exprSDKType, false)
		if err != nil {
			return false, err
		}
		return typ == "function", nil
	}); err != nil {
		// 这条错误最值得写清楚：SDK 是从 alicdn 远程加载的，
		// 「能打开求解页但 SDK 一直不到位」几乎都是出网被挡。
		return fmt.Errorf("阿里验证码 SDK 未在 %s 内加载完成（求解页需能直连 o.alicdn.com）: %w",
			s.opts.ReadyTimeout, err)
	}
	return nil
}

// uaOverride 把 UA 字符串扩成与之一致的元数据。
//
// 元数据必须与 UA 自洽：UA 说 Windows NT 10.0; Win64; x64，元数据就得是
// platform=Windows / platformVersion=10.0.0 / architecture=x86 / bitness=64。
// 只改 UA 字符串而不改元数据，会让页面上同时存在两套互相矛盾的说法。
func uaOverride(ua string) cdp.UAOverride {
	return cdp.UAOverride{
		UserAgent:      ua,
		AcceptLanguage: "zh-CN,zh;q=0.9,en;q=0.8",
		Platform:       "Win32",
		UserAgentMetadata: &cdp.UserAgentMetadata{
			Brands: []cdp.UserAgentBrand{
				{Brand: "Not)A;Brand", Version: "99"},
				{Brand: "Chromium", Version: "127"},
				{Brand: "Google Chrome", Version: "127"},
			},
			FullVersion:     "127.0.6533.89",
			Platform:        "Windows",
			PlatformVersion: "10.0.0",
			Architecture:    "x86",
			Bitness:         "64",
			Mobile:          false,
			Wow64:           false,
		},
	}
}

// waitDevTools 轮询 DevTools HTTP 端点直到它就绪，或浏览器先退出。
func waitDevTools(ctx context.Context, base string, exited <-chan struct{}, timeout time.Duration) (string, error) {
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var last string
	err := poll(pollCtx, 150*time.Millisecond, func() (bool, error) {
		select {
		case <-exited:
			return false, errors.New("浏览器在 DevTools 端点就绪前就退出了")
		default:
		}
		ws, err := cdp.BrowserWSURL(pollCtx, base)
		if err != nil {
			last = err.Error()
			return false, nil
		}
		last = ""
		_ = ws
		return true, nil
	})
	if err != nil {
		if last != "" {
			return "", fmt.Errorf("等待 DevTools 端点超时（最后一次错误: %s）", last)
		}
		return "", fmt.Errorf("等待 DevTools 端点失败: %w", err)
	}
	return cdp.BrowserWSURL(ctx, base)
}

// poll 以固定间隔重复 f 直到它返回 true、报错、或 ctx 结束。
func poll(ctx context.Context, interval time.Duration, f func() (bool, error)) error {
	for {
		done, err := f()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval + time.Duration(rand.Int63n(int64(interval/2+1)))):
		}
	}
}

// freePort 让内核分配一个空闲端口，随后交给浏览器。
//
// 存在的窗口期风险（close 之后被别人抢走）极小，而且被抢走时表现为
// 「DevTools 端点一直不通」→ 本次求解失败并重试，不会串到别的进程上。
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("分配空闲端口失败: %w", err)
	}
	defer ln.Close()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("分配空闲端口失败: 不是 TCP 地址")
	}
	return addr.Port, nil
}

// lineRing 是浏览器 stderr 的定长环形缓冲（只留尾部若干行）。
//
// 必须是**持续排空**的：管道缓冲区写满之后子进程会阻塞在写 stderr 上，
// 表现为浏览器「卡死」。所以无论用不用得上，读循环都要一直跑。
type lineRing struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func (r *lineRing) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
}

// tail 返回最后 n 行的合并串（用于错误信息）。空时返回「（无输出）」。
func (r *lineRing) tail(n int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.lines) == 0 {
		return "（无输出）"
	}
	start := len(r.lines) - n
	if start < 0 {
		start = 0
	}
	return strings.Join(r.lines[start:], " | ")
}

func drainLines(r io.Reader, sink *lineRing) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		sink.add(line)
	}
}
