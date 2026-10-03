// Package contract_test 是 A3 的**强契约测试**：把 `docs/contract/` 里的样本
// 按采样器（`tools/samplecontract`）**完全相同的顺序**回放一遍，逐项比对响应。
//
// 为什么「顺序」是判据的一部分：样本是在一台**有状态**的靶机上按「先建后删」
// 的顺序采出来的 —— `02-accounts-one` 之所以有一条账号，是因为前面刚 `POST` 过；
// `23-models-ok` 之所以能拿到模型表，是因为上一步把 `gateway_key` 置成了已知值。
// 打乱顺序回放，比对的就不是同一件事了。
//
// 比对强度分三档，逐条在 `steps` 里显式声明：
//
//  1. **逐字节**（默认）：状态码 + 响应体紧凑化后逐字节相同。
//     JSON 对象语义上无序，但**键顺序是可观测事实**（靶机用 Python
//     `json.dumps` 保插入序），所以这里的比对走保序节点树，键顺序不同即失败。
//  2. **`volatile` 豁免**：随机量与时间戳（账号 id、指纹、`ts`、`created_at`、
//     `secret`…）不可能复现，逐条列出「允许取值不同、但**键名/键序/类型**仍须一致」
//     的叶子路径。
//  3. **`divergence` 已知分歧**：需要上游调用的分支（OAuth 发起、验证码配置）
//     在 A3 显式报错，与样本的成功响应必然不同。这类**不跳过** —— 改为精确断言
//     我们自己返回的状态码与响应体前缀，把「分歧」也钉成契约。
//
// 空池依赖：全部网关样本都要求「账号池无可用账号」（否则会走转发链路，属 A4）。
// 本测试的步骤顺序天然满足 —— 唯一的可用账号在 `05-accounts-delete-ok` 被删掉，
// 而网关样本在其之后。**不要重排 steps。**
package contract_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/server"
	"github.com/LinYuanNull/zcode2api-go/internal/settings"
	"github.com/LinYuanNull/zcode2api-go/internal/store"
)

const (
	contractRoot = "../../docs/contract"

	// 与采样时一致的两个密钥。样本里它们是 `<redacted:bearer>`，
	// 所以必须由步骤表显式给出（见 step.bearer 的说明）。
	adminKey   = "1234"
	gatewayKey = "contract-gw-key"
	wrongKey   = "wrong-key"
)

// sampledConfig 复现**采样时靶机的配置初值**，回放测试用它建库。
//
// ⚠️ 这里必须逐项复现，不能直接用 constants 的回落值：
//
//	ZCODE_ADMIN_KEY=1234            （采样机 .env）
//	ZCODE_CLAIM_ROUND_INTERVAL=0    （采样机 .env）← 常量回落值是 3600
//	ZCODE_GATEWAY_KEY               （未设，且该变量在靶机上本来就不生效）
//
// `claim_round_interval` 是最容易踩的一个：样本 `17-settings-get.GET.json` 记的是
// **采样环境的取值 0**，而代码回落值是 3600（A3 移走 `.env` 后实测）。
// 样本是「环境 + 代码」的共同产物，回放就得把环境也摆回去。
var sampledConfig = settings.NewConfigured(
	adminKey,
	constants.DefaultGatewayKey,
	constants.DefaultQuotaRefreshInterval,
	constants.DefaultAccountConcurrency,
	0, // 采样机 .env 的 ZCODE_CLAIM_ROUND_INTERVAL
)

const (
	// 样本里的账号 id 占位符（采样器落盘前做的精确替换）。
	idPlaceholder = "<redacted:account-id>"
	// 步骤表里引用「上一步刚生成的账号 id」的写法。
	dynID = "%ID%"
)

// bearer 取值：样本的 Authorization 头一律被脱敏成 `<redacted:bearer>`，
// 光看样本分不出「后台密钥 / 网关 Key / 错 Key」，必须由步骤表指定。
const (
	bearerNone  = ""
	bearerAdmin = "admin"
	bearerGW    = "gw"
	bearerWrong = "wrong"
)

type step struct {
	file string

	bearer string

	// body 是**采样器实际发出的**请求体（逐字）。
	//
	// 为什么不直接用样本里的 `request.body`：样本对请求体也做了脱敏，
	// 敏感键（`tokens` / `secret` / `gateway_key` / `admin_key` /
	// `captcha_verify_param`）的值一律变成 `<redacted:string>` ——
	// 那是「取值不可知」，不是「取值是空」。所以真实请求体只能照抄采样器源码。
	body string

	volatile   []string
	divergence string
	// divergence 非空时：精确断言我们的状态码与响应体前缀。
	wantStatus int
	wantBody   string
}

// steps 是采样顺序（`tools/samplecontract/main.go` 的 run()），**不可重排**。
var steps = []step{
	{file: "admin/01-verify-ok.GET.json", bearer: bearerAdmin},
	{file: "admin/01-verify-unauthorized.GET.json", bearer: bearerNone},

	{file: "admin/02-accounts-empty.GET.json", bearer: bearerAdmin,
		volatile: []string{"ts"}},

	{file: "admin/03-status.GET.json", bearer: bearerAdmin},

	{file: "admin/04-accounts-add-badprovider.POST.json", bearer: bearerAdmin,
		body: `{"provider":"nope","tokens":["x"]}`},
	{file: "admin/04-accounts-add-notokens.POST.json", bearer: bearerAdmin,
		body: `{"provider":"zai","tokens":[]}`},
	{file: "admin/04-accounts-add-ok.POST.json", bearer: bearerAdmin,
		body:     `{"provider":"zai","tokens":["contract-sample-token"],"name":"contract-sample"}`,
		volatile: []string{"ids[0]"}},

	{file: "admin/02-accounts-one.GET.json", bearer: bearerAdmin,
		volatile: []string{
			"ts",
			"accounts[0].id", "accounts[0].created_at",
			"accounts[0].token_masked", "accounts[0].install_id",
			"accounts[0].fingerprint.platform", "accounts[0].fingerprint.arch",
			"accounts[0].fingerprint.os_version", "accounts[0].fingerprint.language",
			"accounts[0].fingerprint.timezone", "accounts[0].fingerprint.screen",
			"accounts[0].fingerprint.device_mid",
		}},

	{file: "admin/06-account-edit-ok.PUT.json", bearer: bearerAdmin,
		body: `{"name":"contract-renamed"}`},
	{file: "admin/06-account-edit-404.PUT.json", bearer: bearerAdmin,
		body: `{"name":"x"}`},

	{file: "admin/07-account-enabled-ok.POST.json", bearer: bearerAdmin,
		body: `{"enabled":false}`},
	{file: "admin/07-account-enabled-404.POST.json", bearer: bearerAdmin,
		body: `{"enabled":true}`},

	{file: "admin/08-account-fingerprint-ok.POST.json", bearer: bearerAdmin,
		volatile: []string{
			"fingerprint.platform", "fingerprint.arch", "fingerprint.os_version",
			"fingerprint.language", "fingerprint.timezone", "fingerprint.screen",
			"fingerprint.device_mid",
		}},
	{file: "admin/08-account-fingerprint-404.POST.json", bearer: bearerAdmin},

	{file: "admin/09-accounts-refresh-all.POST.json", bearer: bearerAdmin,
		body: `{"all":true}`},

	{file: "admin/10-account-refresh-nonjwt.POST.json", bearer: bearerAdmin},
	{file: "admin/10-account-refresh-404.POST.json", bearer: bearerAdmin},

	// ── 已知分歧 ①：OAuth 发起需要打上游（A5）────────────────
	// 样本当时上游可达，采到 200 + authorize_url；A3 的 Starter 是
	// `oauth.Unavailable`，按「未实现必须明确报错」返回 502。
	{file: "admin/11-login-start.POST.json", bearer: bearerAdmin,
		body:       `{"label":"contract"}`,
		divergence: "OAuth 发起属 A5：A3 显式返回 502「登录初始化失败: …」，不伪造 authorize_url",
		wantStatus: http.StatusBadGateway,
		wantBody:   `{"detail":"登录初始化失败: `},

	{file: "admin/12-login-poll-unknown.GET.json", bearer: bearerAdmin},

	{file: "admin/13-claim-preview-empty.GET.json", bearer: bearerAdmin},
	{file: "admin/14-claim-empty.POST.json", bearer: bearerAdmin, body: `{}`},

	// ── 已知分歧 ②：验证码配置来自上游验证码服务（A6）────────
	// 样本采到的是上游当时下发的真实 scene_id / prefix；A3 的 Provider 是
	// `captcha.Unavailable`，返回同形的空配置（键序一致，取值全空）。
	{file: "admin/15-claim-captcha-config.GET.json", bearer: bearerAdmin,
		divergence: "验证码配置来自上游验证码服务（A6）：A3 返回同形空配置",
		wantStatus: http.StatusOK,
		wantBody:   `{"enabled":false,"scene_id":"","region":"","prefix":""}`},

	{file: "admin/16-claim-manual-missing-id.POST.json", bearer: bearerAdmin,
		body: `{"captcha_verify_param":"x"}`},
	{file: "admin/16-claim-manual-404.POST.json", bearer: bearerAdmin,
		body: `{"account_id":"00000000-0000-0000-0000-000000000000","captcha_verify_param":"x"}`},

	// `admin_key_masked` / `gateway_key_masked` 是采样器的**脱敏占位符**
	// （`tools/samplecontract/main.go` 的 `maskedKeys`），不是靶机的真实取值 ——
	// 掩码值形如 `ab12…`，与随机后缀一样不可复现。键名/键序/类型仍逐字比对。
	{file: "admin/17-settings-get.GET.json", bearer: bearerAdmin,
		volatile: []string{"admin_key_masked", "gateway_key_masked"}},

	{file: "admin/18-settings-put-empty-key.PUT.json", bearer: bearerAdmin,
		body: `{"admin_key":""}`},
	{file: "admin/18-settings-put-ok.PUT.json", bearer: bearerAdmin,
		body: `{"gateway_key":"contract-gw-key","quota_refresh_interval":1800,"account_concurrency":2,"claim_round_interval":0}`},

	{file: "admin/19-export.GET.json", bearer: bearerAdmin,
		volatile: []string{"exported_at", "providers.zai[0].secret"}},

	{file: "admin/20-import-ok.POST.json", bearer: bearerAdmin,
		body: `{"providers":{"zai":[{"name":"contract-import","secret":"contract-import-secret"}]}}`},

	{file: "admin/21-monitoring.GET.json", bearer: bearerAdmin},
	{file: "admin/22-monitoring-clear.POST.json", bearer: bearerAdmin},

	{file: "admin/05-accounts-delete-ok.DELETE.json", bearer: bearerAdmin,
		body: `["` + dynID + `"]`},
	{file: "admin/05-accounts-delete-none.DELETE.json", bearer: bearerAdmin,
		body: `["00000000-0000-0000-0000-000000000000"]`},

	// ── 网关（gateway_key 已在上面的 settings-put-ok 里置为 contract-gw-key）──
	{file: "gateway/23-models-noauth.GET.json", bearer: bearerNone},
	{file: "gateway/23-models-wrongkey.GET.json", bearer: bearerWrong},
	{file: "gateway/23-models-ok.GET.json", bearer: bearerGW},

	{file: "gateway/24-messages-noauth.POST.json", bearer: bearerNone,
		body: `{"model":"glm-4.6","max_tokens":16,"messages":[]}`},
	{file: "gateway/24-messages-badjson.POST.json", bearer: bearerGW,
		body: `not-json`},

	// ── A4 更新：503「无可用账号」已由真实调度链路命中 ──
	//
	// 采样顺序在 `20-import-ok` 往池里放了一条**active** 账号（靶机实测：导入即
	// `status=active` / `enabled=true`），所以这两条样本在靶机上命中的不是「空池」，
	// 而是**「调度器真的挑了该账号 → 上游不可达 → 冷却 → 切换无果」**。
	// A4 接通调度后本实现走的就是同一条链路（回放环境无上游 ⇒ 传输层失败 ⇒
	// 账号冷却 ⇒ 池空 ⇒ 503），响应体与样本同形，**不再是分歧**。
	// 「空池」分支仍由 `TestGatewayNoAccountEmptyPool` 逐字节钉住。
	{file: "gateway/24-messages-noaccount.POST.json", bearer: bearerGW,
		body: `{"model":"glm-4.6","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`},

	{file: "gateway/25-chat-noauth.POST.json", bearer: bearerNone,
		body: `{"model":"glm-4.6","messages":[]}`},
	{file: "gateway/25-chat-badjson.POST.json", bearer: bearerGW,
		body: `not-json`},
	{file: "gateway/25-chat-noaccount.POST.json", bearer: bearerGW,
		body: `{"model":"glm-4.6","messages":[{"role":"user","content":"hi"}]}`},

	{file: "admin/18-settings-put-reset-gwkey.PUT.json", bearer: bearerAdmin,
		body: `{"gateway_key":""}`},
}

// ── 样本结构 ────────────────────────────────────────────────

type sample struct {
	Route   string `json:"route"`
	Method  string `json:"method"`
	Request struct {
		Query   map[string]string `json:"query"`
		Headers map[string]string `json:"headers"`
		Body    json.RawMessage   `json:"body"`
	} `json:"request"`
	Response struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    json.RawMessage   `json:"body"`
	} `json:"response"`
	Notes string `json:"notes"`
}

func loadSample(t *testing.T, file string) sample {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(contractRoot, filepath.FromSlash(file)))
	if err != nil {
		t.Fatalf("读取样本失败: %v", err)
	}
	var s sample
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("样本不是合法 JSON: %v", err)
	}
	return s
}

// ── 回放 ────────────────────────────────────────────────────

func TestA3SampleReplay(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "accounts.db"),
		store.WithInitialSettings(sampledConfig))
	if err != nil {
		t.Fatalf("打开账号库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := server.New(server.Config{Store: st, Configured: sampledConfig})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// accID 是回放中我们**自己**生成的账号 id；样本里它是占位符，
	// 比对与后续请求都要先替换回真实值。
	var accID string

	for _, sp := range steps {
		sp := sp
		t.Run(sp.file, func(t *testing.T) {
			smp := loadSample(t, sp.file)

			route := strings.ReplaceAll(smp.Route, idPlaceholder, accID)
			body := strings.ReplaceAll(sp.body, dynID, accID)

			status, ct, raw := do(t, ts.URL, sp, smp, route, body)

			if sp.divergence != "" {
				if status != sp.wantStatus {
					t.Fatalf("已知分歧的状态码不符：want %d got %d\n%s\n  原因: %s",
						sp.wantStatus, status, raw, sp.divergence)
				}
				if !strings.HasPrefix(string(raw), sp.wantBody) {
					t.Fatalf("已知分歧的响应体不符：\n  want 前缀 %s\n  got      %s\n  原因: %s",
						sp.wantBody, raw, sp.divergence)
				}
				t.Logf("已知分歧（%s）：%d %s", sp.divergence, status, raw)
				return
			}

			if status != smp.Response.Status {
				t.Fatalf("状态码不符：样本 %d，实现 %d\n  样本 notes: %s\n  实现响应体: %s",
					smp.Response.Status, status, smp.Notes, raw)
			}
			// 响应头：样本只保留 Content-Type，这里也逐字比。
			if wantCT := smp.Response.Headers["Content-Type"]; wantCT != "" && wantCT != ct {
				t.Errorf("Content-Type 不符：样本 %q，实现 %q", wantCT, ct)
			}

			want, err := decodeNode(smp.Response.Body)
			if err != nil {
				t.Fatalf("样本响应体解析失败: %v", err)
			}
			got, err := decodeNode(raw)
			if err != nil {
				t.Fatalf("实现响应体不是合法 JSON: %v\n%s", err, raw)
			}
			vol := map[string]bool{}
			for _, p := range sp.volatile {
				vol[p] = true
			}
			compareNode(t, "", want, got, vol)

			// 捕获新建账号的 id，供后续步骤替换占位符。
			if sp.file == "admin/04-accounts-add-ok.POST.json" {
				if id := firstStringAt(got, "ids", "0"); id != "" {
					accID = id
					t.Logf("本次回放生成的账号 id = %s", accID)
				} else {
					t.Fatal("未能从新增响应里取到账号 id")
				}
			}
		})
	}

	// 收尾不变式：最后一步把网关 Key 置回空串（采样器的「复原」动作），
	// 确认它真的落到了库里 —— 否则下一次采样会带着残留状态。
	gwKey, _, err := st.GetMeta("gateway_key")
	if err != nil {
		t.Fatalf("读取 gateway_key 失败: %v", err)
	}
	if gwKey != "" {
		t.Errorf("收尾后 gateway_key 应为空串，实际 %q", gwKey)
	}
}

// TestGatewayNoAccountEmptyPool 单独钉住「账号池无可用账号」的 503 形态。
//
// 为什么要单开一个测试：`steps` 的采样顺序里 `20-import-ok` 往池里放了一条 active
// 账号，所以那两条 `*-noaccount` 样本在**靶机上**命中的是「调度器尝试该账号 → 401 →
// 耗尽」的分支（见 steps 里 divergence ③ 的实测记录），需要 A4 才能复现。
// 而「空池」分支是 A3 真正实现的那条 —— 它必须与样本**逐字节同形**，
// 否则一旦 A4 落地，两种输入会给出两种不同的错误体。
//
// 比对走保序节点树（键序 + 取值 + 类型），与主回放同一把尺子。
func TestGatewayNoAccountEmptyPool(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "accounts.db"),
		store.WithInitialSettings(sampledConfig))
	if err != nil {
		t.Fatalf("打开账号库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if n := len(st.List()); n != 0 {
		t.Fatalf("本测试要求全新空池，实际有 %d 条账号", n)
	}

	srv := server.New(server.Config{Store: st, Configured: sampledConfig})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	files := []string{
		"gateway/24-messages-noaccount.POST.json",
		"gateway/25-chat-noaccount.POST.json",
	}
	for _, file := range files {
		file := file
		t.Run(file, func(t *testing.T) {
			smp := loadSample(t, file)

			req, err := http.NewRequest(smp.Method, ts.URL+smp.Route, bytes.NewReader(smp.Request.Body))
			if err != nil {
				t.Fatalf("构造请求失败: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+gatewayKey)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("请求失败: %v", err)
			}
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("读取响应失败: %v", err)
			}
			raw = bytes.TrimSpace(raw)

			if resp.StatusCode != smp.Response.Status {
				t.Fatalf("状态码不符：样本 %d，实现 %d\n  实现响应体: %s",
					smp.Response.Status, resp.StatusCode, raw)
			}
			if wantCT := smp.Response.Headers["Content-Type"]; wantCT != "" &&
				wantCT != resp.Header.Get("Content-Type") {
				t.Errorf("Content-Type 不符：样本 %q，实现 %q",
					wantCT, resp.Header.Get("Content-Type"))
			}

			want, err := decodeNode(smp.Response.Body)
			if err != nil {
				t.Fatalf("样本响应体解析失败: %v", err)
			}
			got, err := decodeNode(raw)
			if err != nil {
				t.Fatalf("实现响应体不是合法 JSON: %v\n%s", err, raw)
			}
			compareNode(t, "", want, got, nil)
		})
	}
}

// do 按步骤与样本发一次请求，返回状态码、Content-Type 与响应体。
func do(t *testing.T, base string, sp step, smp sample, route, body string) (int, string, []byte) {
	t.Helper()

	full := base + route
	if len(smp.Request.Query) > 0 {
		q := url.Values{}
		for k, v := range smp.Request.Query {
			q.Set(k, v)
		}
		full += "?" + q.Encode()
	}

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(smp.Method, full, reader)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	switch sp.bearer {
	case bearerAdmin:
		req.Header.Set("Authorization", "Bearer "+adminKey)
	case bearerGW:
		req.Header.Set("Authorization", "Bearer "+gatewayKey)
	case bearerWrong:
		req.Header.Set("Authorization", "Bearer "+wrongKey)
	case bearerNone:
		// 样本里这一条**没有** Authorization 头，这里也一个都不带。
	default:
		t.Fatalf("步骤表里的 bearer 取值非法: %q", sp.bearer)
	}

	// 与样本比对「请求头集合」：样本只记录 Authorization 与 Content-Type，
	// 两者在步骤表 + body 里已确定，这里做一次自检。
	if _, hasAuth := smp.Request.Headers["Authorization"]; hasAuth != (sp.bearer != bearerNone) {
		t.Fatalf("步骤表的 bearer 与样本不一致：样本 hasAuth=%v，步骤表 bearer=%q",
			hasAuth, sp.bearer)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), bytes.TrimSpace(raw)
}

// ── 保序 JSON 节点树 ────────────────────────────────────────
//
// 为什么不用 `map[string]any`：Go 的 map 无序，编码时会按字典序输出 ——
// 键顺序恰好是本项目最要紧的那条契约（A2 已经因为「按字典序写」吃过一次亏）。
// 所以这里解析成保序节点树，比对时逐键按序比。

type node struct {
	kind byte // '{' 对象、'[' 数组、's' 字符串、'n' 数字、'b' 布尔、'z' null
	lit  string
	keys []string
	vals []*node
}

func decodeNode(raw []byte) (*node, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("响应体为空")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	n, err := parseNode(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("响应体有尾随内容")
	}
	return n, nil
}

func parseNode(dec *json.Decoder) (*node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := &node{kind: '{'}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("对象键不是字符串: %v", kt)
				}
				v, err := parseNode(dec)
				if err != nil {
					return nil, err
				}
				n.keys = append(n.keys, k)
				n.vals = append(n.vals, v)
			}
			if _, err := dec.Token(); err != nil { // '}'
				return nil, err
			}
			return n, nil
		case '[':
			n := &node{kind: '['}
			for dec.More() {
				v, err := parseNode(dec)
				if err != nil {
					return nil, err
				}
				n.vals = append(n.vals, v)
			}
			if _, err := dec.Token(); err != nil { // ']'
				return nil, err
			}
			return n, nil
		}
		return nil, fmt.Errorf("意外的分隔符: %v", t)
	case string:
		return &node{kind: 's', lit: t}, nil
	case json.Number:
		return &node{kind: 'n', lit: t.String()}, nil
	case bool:
		return &node{kind: 'b', lit: fmt.Sprint(t)}, nil
	case nil:
		return &node{kind: 'z'}, nil
	}
	return nil, fmt.Errorf("意外的 Token: %v", tok)
}

// compareNode 递归比对两棵节点树。path 用 `a.b[0].c` 形式，
// 与步骤表里的 volatile 写法一致。
func compareNode(t *testing.T, path string, want, got *node, vol map[string]bool) {
	t.Helper()
	if want.kind != got.kind {
		t.Errorf("%s: JSON 类型不一致\n  样本 %s\n  实现 %s",
			label(path), describe(want), describe(got))
		return
	}
	switch want.kind {
	case '{':
		if len(want.keys) != len(got.keys) {
			t.Errorf("%s: 键数量不一致\n  样本 %v\n  实现 %v",
				label(path), want.keys, got.keys)
			return
		}
		for i, k := range want.keys {
			if got.keys[i] != k {
				t.Errorf("%s: 键顺序不一致\n  样本 %v\n  实现 %v",
					label(path), want.keys, got.keys)
				return
			}
			compareNode(t, joinPath(path, k), want.vals[i], got.vals[i], vol)
		}
	case '[':
		if len(want.vals) != len(got.vals) {
			t.Errorf("%s: 数组长度不一致 want=%d got=%d",
				label(path), len(want.vals), len(got.vals))
			return
		}
		for i := range want.vals {
			compareNode(t, fmt.Sprintf("%s[%d]", path, i), want.vals[i], got.vals[i], vol)
		}
	default:
		if vol[path] {
			return
		}
		if want.lit != got.lit {
			t.Errorf("%s: 取值不一致\n  样本 %s\n  实现 %s",
				label(path), describe(want), describe(got))
		}
	}
}

func joinPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

func label(path string) string {
	if path == "" {
		return "响应体根"
	}
	return path
}

func describe(n *node) string {
	switch n.kind {
	case '{':
		return "object" + fmt.Sprint(n.keys)
	case '[':
		return fmt.Sprintf("array[%d]", len(n.vals))
	case 's':
		return fmt.Sprintf("%q", n.lit)
	case 'n':
		return n.lit
	case 'b':
		return n.lit
	}
	return "null"
}

// firstStringAt 沿键名与数组下标取值（如 ("ids","0")），取不到返回空串。
func firstStringAt(n *node, path ...string) string {
	cur := n
	for _, p := range path {
		if cur == nil {
			return ""
		}
		switch cur.kind {
		case '{':
			idx := -1
			for i, k := range cur.keys {
				if k == p {
					idx = i
					break
				}
			}
			if idx < 0 {
				return ""
			}
			cur = cur.vals[idx]
		case '[':
			var i int
			if _, err := fmt.Sscanf(p, "%d", &i); err != nil || i < 0 || i >= len(cur.vals) {
				return ""
			}
			cur = cur.vals[i]
		default:
			return ""
		}
	}
	if cur != nil && cur.kind == 's' {
		return cur.lit
	}
	return ""
}
