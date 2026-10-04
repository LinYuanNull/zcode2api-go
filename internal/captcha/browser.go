package captcha

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// 浏览器定位与启动参数。
//
// # 来源策略：**不随包分发任何浏览器**
//
// 无痕验证的判定目标就是「识别是不是真浏览器」，所以不存在「又小又能过验证」
// 的内核 —— 项目铁律也禁止随包分发第三方二进制。因此求解器只用**系统已装**
// 的浏览器：Windows 上优先 Edge（本机与绝大多数 Windows 都有），其次 Chrome；
// Linux/macOS 按各发行版常见路径探测。
//
// 换浏览器只影响「有没有」，不影响「对不对」：链路对浏览器版本没有契约依赖。

// envChromium 是可覆盖浏览器路径的环境变量。
//
// **名字与上游一致**（上游 `settings.CHROMIUM_PATH` 读同名变量）：这是运维
// 已经会写的那个变量，换名字只会让人以为配置没生效。
const envChromium = "ZCODE_CHROMIUM_PATH"

// envHeadless 覆盖 headless 参数（默认 `--headless=new`）。
//
// 为什么留这个口子：Chrome 132 把 **old headless 从主二进制移除了**，
// `--headless=true` 的时代结束了；而不同发行版/版本对 `--headless=new` 的支持
// 也有差异。排查「浏览器起来了但拿不到码」时，第一个要试的就是换这一项，
// 不该为此重新编译。置为 `off` 表示不带任何 headless 参数（需要显示器）。
const envHeadless = "ZCODE_BROWSER_HEADLESS"

// defaultHeadlessArg 是默认的 headless 参数。
const defaultHeadlessArg = "--headless=new"

// findBrowser 按「环境变量 → Edge → Chrome」顺序找一个可用的浏览器。
//
// 返回空串表示没找到（调用方据此报「未找到浏览器」，而不是把空路径扔给
// exec 得到一个难懂的错误）。
func findBrowser() string {
	if p := strings.TrimSpace(os.Getenv(envChromium)); p != "" {
		if isExecutable(p) {
			return p
		}
		// 显式配了却不存在 ⇒ 仍然继续探测，但调用方会在错误里带上这个路径
		// （见 Solve 的错误信息），让人一眼看出是配置指错了。
	}
	for _, p := range browserCandidates() {
		if isExecutable(p) {
			return p
		}
	}
	return ""
}

// browserCandidates 返回各平台常见安装路径（Edge 在前）。
func browserCandidates() []string {
	switch runtime.GOOS {
	case "windows":
		var out []string
		for _, base := range windowsInstallRoots() {
			out = append(out,
				filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe"),
				filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"),
			)
		}
		// 逐 base 已按 Edge→Chrome 排序，但不同 base 之间要整体保持 Edge 优先，
		// 所以再分成两轮做一次稳定重排。
		edge := filterByBase(out, "msedge.exe")
		chrome := filterByBase(out, "chrome.exe")
		return append(edge, chrome...)
	case "darwin":
		return []string{
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	default:
		return []string{
			// Edge 优先（与 Windows 侧一致）
			"/usr/bin/microsoft-edge",
			"/usr/bin/microsoft-edge-stable",
			"/opt/microsoft/msedge/msedge",
			// 其次 Chrome / Chromium
			"/usr/local/bin/chromium",
			"/usr/local/bin/google-chrome",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
			"/usr/bin/google-chrome",
			"/usr/bin/google-chrome-stable",
		}
	}
}

// windowsInstallRoots 返回 Windows 上浏览器的三个安装根。
func windowsInstallRoots() []string {
	var out []string
	for _, name := range []string{"ProgramFiles(x86)", "ProgramFiles", "LOCALAPPDATA"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// filterByBase 取出以 base 结尾的候选，保持原序。
func filterByBase(paths []string, base string) []string {
	var out []string
	for _, p := range paths {
		if strings.EqualFold(filepath.Base(p), base) {
			out = append(out, p)
		}
	}
	return out
}

// isExecutable 判断路径存在且是普通文件。
func isExecutable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// browserArgs 组装启动参数。
//
// 与上游 Node 求解器的参数**刻意不同**：那边是 Linux 小内存容器专用的
// （`--single-process --no-zygote --renderer-process-limit=1` + oom 看门狗），
// 在 Windows 桌面机上不仅无益，`--single-process` 还会让渲染进程与浏览器进程
// 合一，反而更容易被风控看出异常。这里只保留跨平台有意义的部分。
func browserArgs(port int, userDataDir, headless string) []string {
	args := []string{
		// 调试端点必须只绑回环：CDP 不带鉴权，绑 0.0.0.0 等于把浏览器交给局域网。
		"--remote-debugging-address=127.0.0.1",
		"--remote-debugging-port=" + strconv.Itoa(port),
		// 独立 profile：复用日常 profile 会带上用户的扩展、缓存与登录态，
		// 既是隐私问题，也会让「同一台机器第二次求解」因为缓存而行为不一致。
		"--user-data-dir=" + userDataDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--disable-default-apps",
		"--disable-component-update",
		"--disable-background-networking",
		"--disable-sync",
		"--disable-gpu",
		"--disable-software-rasterizer",
		"--no-sandbox",
		"--lang=zh-CN",
		"--window-size=1280,720",
	}
	switch headless {
	case "":
		args = append(args, defaultHeadlessArg)
	case "off":
		// 不带 headless 参数（需要真实显示器）
	default:
		args = append(args, headless)
	}
	// 末尾必须是 URL，否则浏览器会开默认新标签页（可能不是我们可控的目标）。
	return append(args, "about:blank")
}

// headlessArg 解析 headless 参数取值（环境变量优先）。
func headlessArg() string {
	if v := strings.TrimSpace(os.Getenv(envHeadless)); v != "" {
		return v
	}
	return defaultHeadlessArg
}

// browserHint 返回给人看的诊断串（找不到浏览器时用来解释「找过哪些位置」）。
func browserHint() string {
	var b strings.Builder
	fmt.Fprintf(&b, "$%s=%q", envChromium, strings.TrimSpace(os.Getenv(envChromium)))
	for _, p := range browserCandidates() {
		b.WriteString(", " + p)
	}
	return b.String()
}
