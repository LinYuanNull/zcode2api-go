package captcha

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LinYuanNull/zcode2api-go/internal/captcha/cdp"
)

// 这个文件里的用例**默认全部跳过**，因为它们要真起一个系统浏览器、还真联网到
// o.alicdn.com 去加载阿里云验证码 SDK —— 放进 CI 会变成随机失败源。
//
// 用环境变量显式打开：
//
//	ZCODE_CAPTCHA_LIVE=1   go test ./internal/captcha/ -run Live -v   # 真解一次
//	ZCODE_CAPTCHA_PROBE=1  go test ./internal/captcha/ -run Probe -v  # 定协议形状
//
// 前者是 A6 的真机验收依据；后者是一次性的协议探针，留着是因为它记录了一个
// 只可能来自真实浏览器的结论（`Emulation.setUserAgentOverride` 到底收哪些形状），
// 而这个结论光看 CDP 文档看不出来。

// liveSolver 建一个用系统浏览器、日志接到 t 的求解器。
func liveSolver(t *testing.T) *CDPSolver {
	t.Helper()
	exe := findBrowser()
	if exe == "" {
		t.Skipf("系统里没找到浏览器（找过: %s）", browserHint())
	}
	t.Logf("使用浏览器: %s", exe)
	return NewCDPSolver(SolveOptions{Executable: exe, Logf: t.Logf})
}

// TestLiveSolve 真解一次验证码，断言拿到了非空凭据。
func TestLiveSolve(t *testing.T) {
	if os.Getenv("ZCODE_CAPTCHA_LIVE") != "1" {
		t.Skip("设 ZCODE_CAPTCHA_LIVE=1 才跑（需要真浏览器 + 出网到 o.alicdn.com）")
	}
	s := liveSolver(t)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	param, err := s.Solve(ctx, Default)
	if err != nil {
		t.Fatalf("求解失败: %v", err)
	}
	if strings.TrimSpace(param) == "" {
		t.Fatal("求解返回了空凭据")
	}
	t.Logf("凭据长度 %d：%s", len(param), param)
}

// TestLiveUAOverrideProbe 用**一个**浏览器会话试出 `Emulation.setUserAgentOverride`
// 接受哪些参数形状，逐条 t.Logf 结果。
//
// 背景：本实现第一版只发 `userAgent + acceptLanguage + platform + userAgentMetadata`
// （metadata 里没有 `fullVersionList`、`model` 是空串被 omitempty 省掉），
// 真实 Edge 回 `-32602 Invalid parameters`。CDP 文档没有说明哪一项是必需的，
// 所以这里穷举一遍、由真实浏览器裁决。
func TestLiveUAOverrideProbe(t *testing.T) {
	if os.Getenv("ZCODE_CAPTCHA_PROBE") != "1" {
		t.Skip("设 ZCODE_CAPTCHA_PROBE=1 才跑（需要真浏览器）")
	}
	s := liveSolver(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	workDir := t.TempDir()
	proc, err := s.startBrowser(ctx, s.opts.Executable, workDir, filepath.Join(workDir, "profile"))
	if err != nil {
		t.Fatalf("起浏览器失败: %v", err)
	}
	defer proc.close()

	target, err := proc.client.CreateTarget(ctx, "")
	if err != nil {
		t.Fatalf("建目标失败: %v", err)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = proc.client.CloseTarget(c, target)
	}()
	session, err := proc.client.AttachToTarget(ctx, target)
	if err != nil {
		t.Fatalf("附着目标失败: %v", err)
	}

	// 权威判据：直接问浏览器「你这条命令收哪些参数」。
	// 有了它才知道 -32602 是「某字段不合法」还是「整个字段都不存在」。
	if raw, err := httpGet(ctx, proc.httpBase+"/json/protocol"); err != nil {
		t.Logf("取协议定义失败: %v", err)
	} else if def := protocolCommandDef(raw, "Emulation", "setUserAgentOverride"); def != "" {
		t.Logf("协议定义 Emulation.setUserAgentOverride = %s", def)
	} else {
		t.Logf("协议里没有 Emulation.setUserAgentOverride（%d 字节的协议定义）", len(raw))
	}
	if raw, err := httpGet(ctx, proc.httpBase+"/json/protocol"); err == nil {
		t.Logf("协议定义 Emulation.UserAgentMetadata = %s", protocolTypeDef(raw, "Emulation", "UserAgentMetadata"))
	}

	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/127.0.0.0 Safari/537.36"
	brands := []cdp.UserAgentBrand{
		{Brand: "Not)A;Brand", Version: "99"},
		{Brand: "Chromium", Version: "127"},
		{Brand: "Google Chrome", Version: "127"},
	}

	cases := []struct {
		name string
		ua   cdp.UAOverride
	}{
		{"只给 userAgent（不碰元数据）", cdp.UAOverride{UserAgent: ua}},
		{"userAgent + acceptLanguage + platform", cdp.UAOverride{
			UserAgent: ua, AcceptLanguage: "zh-CN,zh;q=0.9,en;q=0.8", Platform: "Win32",
		}},
		{"metadata：只给五个必需项（model 为空串但要显式发）", cdp.UAOverride{
			UserAgent: ua, AcceptLanguage: "zh-CN,zh;q=0.9,en;q=0.8", Platform: "Win32",
			UserAgentMetadata: &cdp.UserAgentMetadata{
				Platform: "Windows", PlatformVersion: "10.0.0",
				Architecture: "x86", Model: "", Mobile: false,
			},
		}},
		{"metadata：必需项 + brands + fullVersionList + bitness/wow64（本实现实际发的形状）", cdp.UAOverride{
			UserAgent: ua, AcceptLanguage: "zh-CN,zh;q=0.9,en;q=0.8", Platform: "Win32",
			UserAgentMetadata: &cdp.UserAgentMetadata{
				Brands:          brands,
				FullVersionList: brands,
				FullVersion:     "127.0.6533.89",
				Platform:        "Windows",
				PlatformVersion: "10.0.0",
				Architecture:    "x86",
				Model:           "",
				Mobile:          false,
				Bitness:         "64",
				Wow64:           false,
			},
		}},
		{"metadata：必需项 + 无 brands/fullVersionList", cdp.UAOverride{
			UserAgent: ua,
			UserAgentMetadata: &cdp.UserAgentMetadata{
				FullVersion: "127.0.6533.89",
				Platform:    "Windows", PlatformVersion: "10.0.0",
				Architecture: "x86", Model: "", Mobile: false, Bitness: "64",
			},
		}},
	}

	for _, c := range cases {
		err := proc.client.SetUserAgentOverride(ctx, session, c.ua)
		if err != nil {
			t.Logf("✗ %-58s %v", c.name, err)
			continue
		}
		t.Logf("✓ %-58s 通过", c.name)
	}
}

// ── 探针用的小工具（只被上面的 Live/Probe 用例用到）────────────────

// httpGet 直接读 DevTools 的 HTTP 端点（**绝不走代理**，它只在回环上）。
func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// protocolTypeDef 从 `/json/protocol` 里摘出某个类型的属性列表（带 `*` 的必需项）。
func protocolTypeDef(raw []byte, domain, typeName string) string {
	var schema struct {
		Domains []struct {
			Domain string `json:"domain"`
			Types  []struct {
				ID         string `json:"id"`
				Properties []struct {
					Name     string `json:"name"`
					Type     string `json:"type"`
					Optional bool   `json:"optional"`
				} `json:"properties"`
			} `json:"types"`
		} `json:"domains"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return ""
	}
	for _, d := range schema.Domains {
		if d.Domain != domain {
			continue
		}
		for _, ty := range d.Types {
			if ty.ID != typeName {
				continue
			}
			var parts []string
			for _, p := range ty.Properties {
				mark := ""
				if !p.Optional {
					mark = "*"
				}
				parts = append(parts, p.Name+mark)
			}
			if len(parts) == 0 {
				return "（无属性）"
			}
			return strings.Join(parts, ", ")
		}
	}
	return ""
}

// protocolCommandDef 从 `/json/protocol` 里摘出某条命令的 `parameters` 定义，
// 拼成一行便于人读（例如 `userAgent*, acceptLanguage, platform, userAgentMetadata`，
// 带 `*` 的是必需项）。
func protocolCommandDef(raw []byte, domain, command string) string {
	var schema struct {
		Domains []struct {
			Domain   string `json:"domain"`
			Commands []struct {
				Name       string `json:"name"`
				Parameters []struct {
					Name     string `json:"name"`
					Optional bool   `json:"optional"`
				} `json:"parameters"`
			} `json:"commands"`
		} `json:"domains"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return ""
	}
	for _, d := range schema.Domains {
		if d.Domain != domain {
			continue
		}
		for _, c := range d.Commands {
			if c.Name != command {
				continue
			}
			var parts []string
			for _, p := range c.Parameters {
				mark := ""
				if !p.Optional {
					mark = "*"
				}
				parts = append(parts, p.Name+mark)
			}
			if len(parts) == 0 {
				return "（无参数）"
			}
			return strings.Join(parts, ", ")
		}
	}
	return ""
}
