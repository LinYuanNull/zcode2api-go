package captcha

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件是**离线**单测：不起浏览器、不出网。
// 要真机验证求解（起浏览器 + 连 o.alicdn.com）见 live_test.go，
// 用 `ZCODE_CAPTCHA_LIVE=1` 打开。

// ── 配置：样本一致性 ────────────────────────────────────────

// Default 的序列化结果必须**逐字节**等于样本 `15-claim-captcha-config.GET.json`
// 的响应体。键序由 struct 字段顺序决定，所以这条断言同时守住「键顺序」。
func TestDefaultSerializesToSampleBody(t *testing.T) {
	const sample = `{"enabled":true,"scene_id":"11xygtvd","region":"cn","prefix":"no8xfe"}`
	b, err := json.Marshal(Default)
	if err != nil {
		t.Fatalf("序列化 Default 失败: %v", err)
	}
	if string(b) != sample {
		t.Errorf("Default 与样本不符:\n got %s\nwant %s", b, sample)
	}
}

func TestStaticProvider(t *testing.T) {
	s := Static{C: Config{SceneID: "x"}}
	if got := s.Config(); got.SceneID != "x" {
		t.Errorf("Static.Config() = %+v", got)
	}
}

// ── 客户配置：解析与回落 ────────────────────────────────────

// stubSource 是一个可编排的 ConfigSource：按次序回放响应或错误。
type stubSource struct {
	mu    sync.Mutex
	calls int

	// status/body 用于「有响应」的情形；err 优先（直接当成传输失败）。
	status int
	body   string
	err    error

	// onCall 可选：每次调用时覆盖上面的取值（按第 N 次调用算，从 1 开始）。
	onCall func(n int) (status int, body string, err error)
}

func (s *stubSource) GetConfigs(_ context.Context) (*http.Response, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()

	status, body, err := s.status, s.body, s.err
	if s.onCall != nil {
		status, body, err = s.onCall(n)
	}
	if err != nil {
		return nil, err
	}
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}, nil
}

func (s *stubSource) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// 真实夹具里 `data.configs.captcha` 的取值与 Default 完全一致
// （管理面样本 `15-*` 就是它的四个字段）。所以「解析成功」与「回落默认」
// 在这里产出同一个结果 —— 断言时必须**同时**检查 err 才能区分两条路径。
func TestFetchConfigParsesClientConfigs(t *testing.T) {
	const body = `{"data":{"configs":{"captcha":` +
		`{"enabled":true,"prefix":"no8xfe","region":"cn","sceneId":"11xygtvd","skip_model_request":true}}}}`
	m := NewManager(Options{Source: &stubSource{body: body}})

	got, err := m.fetchConfig(context.Background())
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got != Default {
		t.Errorf("解析结果 = %+v, want %+v", got, Default)
	}
}

func TestFetchConfigTrimsWhitespace(t *testing.T) {
	const body = `{"data":{"configs":{"captcha":` +
		`{"enabled":true,"prefix":" p ","region":" cn ","sceneId":" s "}}}}`
	m := NewManager(Options{Source: &stubSource{body: body}})

	got, err := m.fetchConfig(context.Background())
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	want := Config{Enabled: true, SceneID: "s", Region: "cn", Prefix: "p"}
	if got != want {
		t.Errorf("解析结果 = %+v, want %+v", got, want)
	}
}

func TestFetchConfigFallsBackToDefault(t *testing.T) {
	cases := []struct {
		name   string
		source ConfigSource
	}{
		{"传输失败", &stubSource{err: errors.New("断网")}},
		{"非 200", &stubSource{status: http.StatusBadGateway, body: "upstream down"}},
		{"不是 JSON", &stubSource{body: "<html>502</html>"}},
		{"JSON 里没有 captcha 对象", &stubSource{body: `{"data":{"configs":{"other":{}}}}`}},
		{"没有 data 字段", &stubSource{body: `{}`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := NewManager(Options{Source: c.source})
			got, err := m.fetchConfig(context.Background())
			if err == nil {
				t.Fatal("期望报错（供日志用），但 err == nil")
			}
			if got != Default {
				t.Errorf("失败时应回落 Default，得到 %+v", got)
			}
		})
	}
}

// Source 为 nil 时直接用 Default，且**不报错**（离线模式不是故障）。
func TestFetchConfigWithoutSourceIsDefaultNoError(t *testing.T) {
	m := NewManager(Options{})
	got, err := m.fetchConfig(context.Background())
	if err != nil {
		t.Fatalf("nil Source 不该报错: %v", err)
	}
	if got != Default {
		t.Errorf("got %+v, want Default", got)
	}
}

// withFallbacks 是**逐字段**回落，不是整对象回落。
func TestWithFallbacksIsPerField(t *testing.T) {
	got := Config{Enabled: false, SceneID: "", Region: "us", Prefix: ""}.withFallbacks()
	want := Config{Enabled: false, SceneID: Default.SceneID, Region: "us", Prefix: Default.Prefix}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	// Enabled=false 是**有意义的取值**，不该被回落成 true。
	if got.Enabled {
		t.Error("Enabled=false 被改成了 true（回落不该碰布尔位）")
	}
}

// ── Manager：配置缓存 ───────────────────────────────────────

func TestManagerCachesSuccess(t *testing.T) {
	src := &stubSource{body: `{"data":{"configs":{"captcha":{"sceneId":"abc"}}}}`}
	now := time.Now()
	m := NewManager(Options{Source: src, CacheTTL: 600 * time.Second, Now: func() time.Time { return now }})

	for i := 0; i < 3; i++ {
		if got := m.Config(); got.SceneID != "abc" {
			t.Fatalf("第 %d 次 Config() = %+v", i, got)
		}
	}
	if src.Calls() != 1 {
		t.Errorf("TTL 内应只拉一次，实际 %d 次", src.Calls())
	}

	now = now.Add(601 * time.Second)
	_ = m.Config()
	if src.Calls() != 2 {
		t.Errorf("过了 TTL 应重新拉，实际 %d 次", src.Calls())
	}
}

// 失败**也落缓存**（本实现相对上游的有意偏离）：断网时不该每次请求都卡满超时。
func TestManagerCachesFailureWithShorterTTL(t *testing.T) {
	src := &stubSource{err: errors.New("断网")}
	now := time.Now()
	m := NewManager(Options{
		Source:          src,
		CacheTTL:        600 * time.Second,
		FailureCacheTTL: 60 * time.Second,
		Now:             func() time.Time { return now },
	})

	if got := m.Config(); got != Default {
		t.Fatalf("拉不到时应给 Default，得到 %+v", got)
	}
	_ = m.Config()
	if src.Calls() != 1 {
		t.Errorf("失败结果在 FailureCacheTTL 内应只拉一次，实际 %d 次", src.Calls())
	}

	// 失败缓存比成功缓存短：60s 后必须重试（若误用 CacheTTL 就不会重试）。
	now = now.Add(61 * time.Second)
	_ = m.Config()
	if src.Calls() != 2 {
		t.Errorf("过了 FailureCacheTTL 应重试，实际 %d 次", src.Calls())
	}

	// 恢复后立刻拿到新值。
	src.mu.Lock()
	src.err = nil
	src.body = `{"data":{"configs":{"captcha":{"sceneId":"fresh"}}}}`
	src.mu.Unlock()
	now = now.Add(61 * time.Second)
	if got := m.Config(); got.SceneID != "fresh" {
		t.Errorf("上游恢复后应拿到新值，得到 %+v", got)
	}
}

// ── Manager：求解与重试 ─────────────────────────────────────

// stubSolver 按脚本回放结果。
type stubSolver struct {
	mu      sync.Mutex
	calls   int
	results []struct {
		param string
		err   error
	}
}

func (s *stubSolver) Solve(_ context.Context, _ Config) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if len(s.results) == 0 {
		return "", errors.New("stubSolver 没有更多预设结果")
	}
	r := s.results[0]
	if len(s.results) > 1 {
		s.results = s.results[1:]
	}
	return r.param, r.err
}

func (s *stubSolver) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestAcquireRetriesUntilSuccess(t *testing.T) {
	sol := &stubSolver{results: []struct {
		param string
		err   error
	}{
		{"", ErrSolveTimeout},
		{"", ErrSolveTimeout},
		{"  token-1  ", nil},
	}}
	m := NewManager(Options{Solver: sol, Retries: 4, RetryDelay: time.Millisecond})

	got, err := m.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}
	if got != "token-1" {
		t.Errorf("凭据应被 TrimSpace，得到 %q", got)
	}
	if sol.Calls() != 3 {
		t.Errorf("应重试到第 3 次，实际 %d 次", sol.Calls())
	}
	if m.LastError() != "" {
		t.Errorf("成功后 LastError 应为空，得到 %q", m.LastError())
	}
}

// 没浏览器时**不重试**：重试不会变出浏览器来。
func TestAcquireDoesNotRetryWhenNoBrowser(t *testing.T) {
	sol := &stubSolver{results: []struct {
		param string
		err   error
	}{{"", ErrNoBrowser}}}
	m := NewManager(Options{Solver: sol, Retries: 4, RetryDelay: time.Millisecond})

	_, err := m.Acquire(context.Background())
	if !errors.Is(err, ErrNoBrowser) {
		t.Fatalf("err = %v, want 包装 ErrNoBrowser", err)
	}
	if sol.Calls() != 1 {
		t.Errorf("ErrNoBrowser 不该重试，实际 %d 次", sol.Calls())
	}
}

// 求解器不报错但也不给凭据 ⇒ 按超时处理（上游同样把「未产出」当失败）。
func TestAcquireTreatsEmptyParamAsFailure(t *testing.T) {
	sol := &stubSolver{results: []struct {
		param string
		err   error
	}{{"", nil}, {"   ", nil}}}
	m := NewManager(Options{Solver: sol, Retries: 2, RetryDelay: time.Millisecond})

	_, err := m.Acquire(context.Background())
	if !errors.Is(err, ErrSolveTimeout) {
		t.Fatalf("err = %v, want 包装 ErrSolveTimeout", err)
	}
	if sol.Calls() != 2 {
		t.Errorf("应重试满 2 次，实际 %d 次", sol.Calls())
	}
}

func TestAcquireWithoutSolver(t *testing.T) {
	m := NewManager(Options{})
	if _, err := m.Acquire(context.Background()); err == nil {
		t.Fatal("没求解器时应报错，不能返回空凭据")
	}
}

func TestAcquireStopsOnContextCancel(t *testing.T) {
	sol := &stubSolver{results: []struct {
		param string
		err   error
	}{{"", ErrSolveTimeout}}}
	m := NewManager(Options{Solver: sol, Retries: 4, RetryDelay: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// 配置拉不到不该拦着求解：四个 SDK 参数有静态默认值。
func TestAcquireSolvesEvenWhenConfigFetchFails(t *testing.T) {
	sol := &stubSolver{results: []struct {
		param string
		err   error
	}{{"token-2", nil}}}
	m := NewManager(Options{
		Source:  &stubSource{err: errors.New("断网")},
		Solver:  sol,
		Retries: 1,
	})

	got, err := m.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}
	if got != "token-2" {
		t.Errorf("got %q", got)
	}
}

// 编译期断言：*CDPSolver 必须满足 Solver（否则 NewManager 那条线静默失效）。
var _ Solver = (*CDPSolver)(nil)

func TestManagerBrowserReportsSolverExecutable(t *testing.T) {
	// 求解器的 Executable 为空时会走自动探测；这里只要求它不 panic、
	// 且显式指定时原样返回。自动探测的结果取决于机器，不做值断言。
	if got := NewManager(Options{Solver: NewCDPSolver(SolveOptions{Executable: "X:/fake.exe"})}).Browser(); got != "X:/fake.exe" {
		t.Errorf("Browser() = %q, want 覆盖值", got)
	}
	_ = NewManager(Options{Solver: NewCDPSolver(SolveOptions{})}).Browser()
	// 没求解器时也不该 panic。
	_ = NewManager(Options{}).Browser()
}

// ── 表达式与脚本 ────────────────────────────────────────────

func TestSolveExprContainsContractPieces(t *testing.T) {
	expr := solveExpr(Config{SceneID: "scene-1", Region: "cn", Prefix: "pfx"}, 20000)

	for _, want := range []string{
		`SceneId: "scene-1"`,                             // SDK 选项名（上游定义，不能改）
		`window.AliyunCaptchaConfig`,                     // 全局配置对象名
		`region: "cn"`,                                   // 全局配置里的 region
		`prefix: "pfx"`,                                  // 全局配置里的 prefix
		`mode: "popup"`,                                  // 上游用的模式
		`element: "#cap-holder"`,                         // 求解页里的挂载点 id
		`button: "#cap-btn"`,                             // 求解页里的按钮 id
		`startTracelessVerification`,                     // 无痕验证入口
		`captchaVerifyParam`,                             // 成功回调里取的就是这个字段
		`setTimeout(function () { done(null); }, 20000)`, // 页面内兜底超时
	} {
		if !strings.Contains(expr, want) {
			t.Errorf("求解表达式里缺少 %s\n---\n%s", want, expr)
		}
	}
}

// 配置值里带引号/反斜杠时不能破坏表达式。
func TestSolveExprQuotesConfigSafely(t *testing.T) {
	expr := solveExpr(Config{SceneID: `a"b\c`, Region: "cn", Prefix: "p"}, 1000)
	if !strings.Contains(expr, `SceneId: "a\"b\\c"`) {
		t.Errorf("SceneId 未被正确转义:\n%s", expr)
	}
}

func TestAntiDetectJSOnlyTouchesThreeProperties(t *testing.T) {
	for _, want := range []string{`"webdriver"`, `"languages"`, `"platform"`} {
		if !strings.Contains(antiDetectJS, want) {
			t.Errorf("反探测补丁里缺少 %s", want)
		}
	}
	// 只许改这三项：多改一项就多一处自相矛盾的可能。
	if n := strings.Count(antiDetectJS, "Object.defineProperty"); n != 3 {
		t.Errorf("defineProperty 出现 %d 次，应是 3 次（只改 webdriver/languages/platform）", n)
	}
}

func TestReadyStateExprs(t *testing.T) {
	if exprReadyState != "document.readyState" {
		t.Errorf("exprReadyState = %q", exprReadyState)
	}
	if exprSDKType != "typeof window.initAliyunCaptcha" {
		t.Errorf("exprSDKType = %q", exprSDKType)
	}
}

// ── 求解页 ──────────────────────────────────────────────────

// 求解页必须同时具备三样硬性输入：SDK 脚本、`#cap-holder` 挂载点、`#cap-btn` 按钮。
// 少任何一样，SDK 初始化都会失败，而失败在页面上表现为「永远不出码」。
func TestSolverPageHasRequiredPieces(t *testing.T) {
	page := string(solverPage)
	for _, want := range []string{
		"o.alicdn.com",     // SDK 来源（出网可达性是求解的前置条件）
		"AliyunCaptcha.js", // SDK 文件名
		`id="cap-holder"`,  // initAliyunCaptcha 的 element
		`id="cap-btn"`,     // initAliyunCaptcha 的 button
	} {
		if !strings.Contains(page, want) {
			t.Errorf("求解页里缺少 %s", want)
		}
	}
}

func TestWriteSolverPageWritesAndReturnsFileURL(t *testing.T) {
	dir := t.TempDir()
	u, err := writeSolverPage(dir)
	if err != nil {
		t.Fatalf("writeSolverPage 失败: %v", err)
	}
	if !strings.HasPrefix(u, "file:///") {
		t.Errorf("URL = %q，应以 file:/// 开头", u)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "solver-page.html"))
	if err != nil {
		t.Fatalf("读回求解页失败: %v", err)
	}
	if string(raw) != string(solverPage) {
		t.Error("落盘的求解页与内嵌副本不一致")
	}
}

func TestFileURL(t *testing.T) {
	// 用 FromSlash 造平台原生分隔符：Windows 上是 `D:\a\b.html`，
	// 其它平台是 `D:/a/b.html`，两种输入都应该得到同一个 file:// URL。
	cases := []struct{ in, want string }{
		{filepath.FromSlash("D:/a/b.html"), "file:///D:/a/b.html"},
		{filepath.FromSlash("/tmp/a b.html"), "file:///tmp/a b.html"},
	}
	for _, c := range cases {
		if got := fileURL(c.in); got != c.want {
			t.Errorf("fileURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ── 浏览器定位与参数 ────────────────────────────────────────

func TestFindBrowserPrefersEnv(t *testing.T) {
	// 造一个「存在且是普通文件」的假浏览器：findBrowser 只做存在性检查。
	fake := filepath.Join(t.TempDir(), "my-browser.exe")
	if err := os.WriteFile(fake, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envChromium, fake)
	if got := findBrowser(); got != fake {
		t.Errorf("findBrowser() = %q, want 环境变量指定值 %q", got, fake)
	}
}

// 环境变量指向不存在的路径时**继续探测**（不是直接失败）：真正的报错由
// Solve 带上这个路径，让人一眼看出配置指错了。
func TestFindBrowserIgnoresMissingEnvPath(t *testing.T) {
	t.Setenv(envChromium, filepath.Join(t.TempDir(), "nope.exe"))
	got := findBrowser()
	if got == filepath.Join(t.TempDir(), "nope.exe") {
		t.Fatal("不该返回不存在的路径")
	}
	// 结果要么是空串（机器上真没浏览器），要么是探测到的真实路径。
	if got != "" && !isExecutable(got) {
		t.Errorf("findBrowser() 返回了不可执行的路径: %q", got)
	}
}

func TestBrowserCandidatesOrder(t *testing.T) {
	got := browserCandidates()
	if len(got) < 2 {
		t.Fatalf("候选太少: %v", got)
	}
	if !strings.Contains(got[0], "edge") {
		t.Errorf("首个候选应是 Edge，得到 %q", got[0])
	}
	// Edge 全部排在 Chrome/Chromium 之前。
	seenChrome := false
	for _, p := range got {
		low := strings.ToLower(p)
		if strings.Contains(low, "chrome") || strings.Contains(low, "chromium") {
			seenChrome = true
			continue
		}
		if seenChrome {
			t.Errorf("Edge 候选 %q 排在 Chrome 之后", p)
		}
	}
}

func TestBrowserArgs(t *testing.T) {
	args := browserArgs(12345, `D:\prof`, "")

	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--remote-debugging-address=127.0.0.1", // 绝不绑 0.0.0.0
		"--remote-debugging-port=12345",
		`--user-data-dir=D:\prof`,
		"--no-sandbox",
		defaultHeadlessArg,
		"--lang=zh-CN",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("启动参数里缺少 %q\n%s", want, joined)
		}
	}
	// 上游那套 Linux 小内存容器专用参数**不能**出现在这里
	//（--single-process 在 Windows 上会让进程合一，更易被风控识别）。
	for _, banned := range []string{"--single-process", "--no-zygote", "--renderer-process-limit"} {
		if strings.Contains(joined, banned) {
			t.Errorf("不该带上上游的 Linux 专用参数 %q", banned)
		}
	}
	// 末尾必须是 URL，否则浏览器会开默认新标签页。
	if args[len(args)-1] != "about:blank" {
		t.Errorf("末位参数 = %q，应是 about:blank", args[len(args)-1])
	}
}

func TestBrowserArgsHeadlessSwitch(t *testing.T) {
	contains := func(args []string, s string) bool {
		for _, a := range args {
			if a == s {
				return true
			}
		}
		return false
	}

	// "" ⇒ 用默认 headless 参数
	if got := browserArgs(1, "p", ""); !contains(got, defaultHeadlessArg) {
		t.Errorf("空值应回落到 %s", defaultHeadlessArg)
	}
	// "off" ⇒ 不带任何 headless 参数
	if got := browserArgs(1, "p", "off"); contains(got, defaultHeadlessArg) ||
		contains(got, "--headless") || contains(got, "--headless=old") {
		t.Errorf("off 时不该带 headless 参数: %v", got)
	}
	// 其它值 ⇒ 原样使用
	if got := browserArgs(1, "p", "--headless=old"); !contains(got, "--headless=old") {
		t.Errorf("自定义值应原样使用: %v", got)
	}
}

func TestHeadlessArgEnvOverride(t *testing.T) {
	t.Setenv(envHeadless, "off")
	if got := headlessArg(); got != "off" {
		t.Errorf("headlessArg() = %q, want off", got)
	}
	t.Setenv(envHeadless, "  ")
	if got := headlessArg(); got != defaultHeadlessArg {
		t.Errorf("空白环境变量应回落默认值，得到 %q", got)
	}
}

func TestBrowserHintMentionsEnvVarAndCandidates(t *testing.T) {
	t.Setenv(envChromium, "X:/custom.exe")
	hint := browserHint()
	if !strings.Contains(hint, envChromium) || !strings.Contains(hint, "X:/custom.exe") {
		t.Errorf("诊断串没提到环境变量: %s", hint)
	}
}

// ── 工具件 ──────────────────────────────────────────────────

func TestLineRingKeepsTail(t *testing.T) {
	r := &lineRing{max: 3}
	for _, s := range []string{"1", "2", "3", "4", "5"} {
		r.add(s)
	}
	if got := r.tail(10); got != "3 | 4 | 5" {
		t.Errorf("tail(10) = %q, want %q", got, "3 | 4 | 5")
	}
	if got := r.tail(2); got != "4 | 5" {
		t.Errorf("tail(2) = %q", got)
	}
	if got := (&lineRing{max: 3}).tail(2); got != "（无输出）" {
		t.Errorf("空缓冲 tail = %q", got)
	}
}

func TestDrainLinesReadsAllLines(t *testing.T) {
	r := &lineRing{max: 10}
	drainLines(strings.NewReader("a\n\n  b  \nc\n"), r)
	// 空行被跳过，行首尾空白被裁掉，stderr 是 CRLF 也认。
	if got := r.tail(10); got != "a | b | c" {
		t.Errorf("drainLines 结果 = %q", got)
	}
}

func TestPollStopsOnDoneAndOnError(t *testing.T) {
	// 第 3 次返回 true。
	n := 0
	if err := poll(context.Background(), time.Millisecond, func() (bool, error) {
		n++
		return n == 3, nil
	}); err != nil {
		t.Fatalf("poll 出错: %v", err)
	}
	if n != 3 {
		t.Errorf("调用了 %d 次，应为 3", n)
	}

	// 报错立刻返回该错误。
	boom := errors.New("boom")
	if err := poll(context.Background(), time.Millisecond, func() (bool, error) {
		return false, boom
	}); !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}

	// ctx 结束也要能退出（否则轮询会在取消后一直跑到超时）。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := poll(ctx, time.Millisecond, func() (bool, error) { return false, nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestFreePort(t *testing.T) {
	p1, err := freePort()
	if err != nil {
		t.Fatalf("freePort 失败: %v", err)
	}
	p2, err := freePort()
	if err != nil {
		t.Fatalf("freePort 失败: %v", err)
	}
	if p1 <= 0 || p2 <= 0 || p1 > 65535 || p2 > 65535 {
		t.Errorf("端口越界: %d, %d", p1, p2)
	}
	if p1 == p2 {
		t.Logf("两次分配同一个端口 %d（内核复用，允许但不常见）", p1)
	}
}
