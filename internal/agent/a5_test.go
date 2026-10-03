package agent_test

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LinYuanNull/zcode2api-go/internal/agent"
	"github.com/LinYuanNull/zcode2api-go/internal/identity"
)

// ── 测试用假上游 ────────────────────────────────────────────
//
// 为什么用 httptest 而不是打真上游：A5 出站的**形状**（端点、头、信封解析）
// 都由 `docs/contract/outbound-admin/` 的 5 个样本钉死了，测试只需按样本回放，
// 不能依赖 z.ai 可达（会随网络/风控漂移）。真实链路的验收另有 `tools/` 抓包脚本。

// gz 返回 gzip 压缩后的字节（上游这几个端点默认回 gzip）。
func gz(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatalf("gzip 压缩失败: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip 关闭失败: %v", err)
	}
	return buf.Bytes()
}

// record 收集一次请求的可观测事实，供断言。
type record struct {
	method string
	path   string
	header http.Header
	body   []byte
}

// fakeUpstream 是覆盖 A5 五个端点的假上游。`handler` 由每个测试给出。
func fakeUpstream(t *testing.T, seen *record, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*seen = record{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newClient 建一个指向假上游的出站客户端。
func newClient(t *testing.T, base string) *agent.Client {
	t.Helper()
	c := agent.New()
	c.SetA5BaseForTest(base)
	return c
}

// ── OAuth init ──────────────────────────────────────────────

// 样本 `01-oauth-init.POST.json`（响应为 gzip 的信封，data 里 5 个字段）。
const initBody = `{"code":0,"msg":"","flow_id":"ignored","data":{"flow_id": "abc0123456789abcdef0123456789ab","poll_token": "tok","authorize_url":"https://chat.z.ai/api/oauth/authorize?client_id=c\u0026state=s","expires_at":1791063908,"poll_interval_sec":2},"logid": "x"}`

func TestOAuthInitParsesEnvelopeAndSendsMinimalHeaders(t *testing.T) {
	var seen record
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(gz(t, initBody))
	}
	srv := fakeUpstream(t, &seen, handler)

	token := strings.Repeat("ab", 32) // 64 hex
	flow, err := newClient(t, srv.URL).OAuthInit(context.Background(), token, "zai")
	if err != nil {
		t.Fatalf("OAuthInit 失败: %v", err)
	}

	// 解析结果：整数字段也要解对（`expires_at` 是时间戳，不是字符串）。
	if flow.FlowID != "abc0123456789abcdef0123456789ab" {
		t.Errorf("FlowID = %q", flow.FlowID)
	}
	if flow.PollToken != "tok" {
		t.Errorf("PollToken = %q", flow.PollToken)
	}
	if flow.ExpiresAt != 1791063908 {
		t.Errorf("ExpiresAt = %d", flow.ExpiresAt)
	}
	if flow.PollIntervalSec != 2 {
		t.Errorf("PollIntervalSec = %d", flow.PollIntervalSec)
	}
	if !strings.Contains(flow.AuthorizeURL, "client_id=c&state=s") {
		t.Errorf("AuthorizeURL 未按 JSON 语义解转义: %q", flow.AuthorizeURL)
	}

	// 请求形状：POST，body 逐字 `{"provider":"zai"}`（样本的 body_text）。
	if seen.method != http.MethodPost {
		t.Errorf("method = %s，want POST", seen.method)
	}
	if got := string(seen.body); got != `{"provider":"zai"}` {
		t.Errorf("body = %q，want {\"provider\":\"zai\"}", got)
	}

	// **极简头**：只有 Accept / Accept-Encoding / Authorization / Connection /
	// User-Agent / Content-Type（样本 3.1）。多一个 X-* 头就是偏离契约。
	wantExact := []string{
		"Accept", "Accept-Encoding", "Authorization", "Connection",
		"Content-Length", "Content-Type", "User-Agent",
	}
	assertHeaderSet(t, seen.header, wantExact)
	if got := seen.header.Get("Authorization"); got != "Bearer "+token {
		t.Errorf("Authorization = %q", got)
	}
	if got := seen.header.Get("User-Agent"); got != identity.OAuthUserAgent {
		t.Errorf("User-Agent = %q，want %q", got, identity.OAuthUserAgent)
	}
	if got := seen.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := seen.header.Get("Accept-Encoding"); got != "gzip, deflate" {
		t.Errorf("Accept-Encoding = %q（显式设值 ⇒ Go 不做透明解压，必须自解）", got)
	}
}

// ── OAuth poll ──────────────────────────────────────────────

func TestOAuthPollPathAndStatus(t *testing.T) {
	var seen record
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(gz(t, `{"code":0,"msg":"","data":{"status":"pending"},"logid": "x"}`))
	}
	srv := fakeUpstream(t, &seen, handler)

	token := strings.Repeat("cd", 32)
	status, err := newClient(t, srv.URL).OAuthPoll(context.Background(), token, "flow-123")
	if err != nil {
		t.Fatalf("OAuthPoll 失败: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %q，want pending（上游原值透传）", status)
	}
	if seen.method != http.MethodGet {
		t.Errorf("method = %s，want GET", seen.method)
	}
	if want := "/api/v1/oauth/cli/poll/flow-123"; seen.path != want {
		t.Errorf("path = %q，want %q", seen.path, want)
	}
	// poll 没有请求体 ⇒ 没有 Content-Type（样本 3.1 与 3.2 的差异）。
	if got := seen.header.Get("Content-Type"); got != "" {
		t.Errorf("poll 不应带 Content-Type，实际 %q", got)
	}
	if got := seen.header.Get("Authorization"); got != "Bearer "+token {
		t.Errorf("Authorization = %q（init 与 poll 必须复用同一 Bearer）", got)
	}
}

// ── 额度查询 ────────────────────────────────────────────────

// 样本 `03/04/05`：三条共用同一个 `X-Request-Id`，且请求头是**全套指纹头**。
func TestQuotaHeadersFullFingerprint(t *testing.T) {
	var seen record
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(gz(t, `{"code":0,"msg":"","data":{"ok":true},"logid": "x"}`))
	}
	srv := fakeUpstream(t, &seen, handler)

	fp := identity.QuotaFingerprint{
		Platform: "darwin", Arch: "arm64", OSVersion: "24.5.0",
		Language: "ja-JP", Timezone: "Asia/Tokyo", DeviceMID: "11111111-2222-4333-8444-555555555555",
	}
	reqID := agent.NewRequestID()

	resp, err := newClient(t, srv.URL).GetPlanUsage(context.Background(), "jwt-token", reqID, fp)
	if err != nil {
		t.Fatalf("GetPlanUsage 失败: %v", err)
	}
	agent.DrainAndClose(resp)

	if seen.path != "/api/v1/zcode-plan/usage" {
		t.Errorf("path = %q", seen.path)
	}
	// 关键：`Content-Type: application/json` **出现在 GET 上**（样本实测如此）。
	if got := seen.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("GET 应带 Content-Type: application/json，实际 %q", got)
	}
	want := map[string]string{
		"X-Request-Id":        reqID,
		"X-Os-Category":       "macos",
		"X-Os-Version":        "24.5.0",
		"X-Platform":          "darwin-arm64",
		"X-Release-Channel":   "stable",
		"X-Title":             "Z Code@electron",
		"X-Client-Language":   "ja-JP",
		"X-Client-Timezone":   "Asia/Tokyo",
		"X-Device-Mid":        fp.DeviceMID,
		"X-Zcode-App-Version": "3.14.4",
		"Http-Referer":        "https://zcode.z.ai",
		"Authorization":       "Bearer jwt-token",
	}
	for k, v := range want {
		if got := seen.header.Get(k); got != v {
			t.Errorf("%s = %q，want %q", k, got, v)
		}
	}
	// 额度查询用转发链路的 UA（`ZCode/3.14.4`），**不是** OAuth 的 httpx UA。
	if got := seen.header.Get("User-Agent"); got != identity.UserAgent {
		t.Errorf("User-Agent = %q，want %q", got, identity.UserAgent)
	}
}

// 凭据失效：`usage` 回 **404**（且是 gzip 的 `404 page not found` 文本）。
//
// 三条额度查询按设计**不读响应体**返回（`resp.Body` 未读）—— 状态码与错误体由
// 调用方（A5-3 的 quota 层）分派，所以这里断言的是「状态码可见 + `ReadBody`
// 能把 gzip 解成明文」，而不是「函数返回 error」。
func TestQuotaNon2xxLeavesStatusForCaller(t *testing.T) {
	var seen record
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(gz(t, "404 page not found\n"))
	}
	srv := fakeUpstream(t, &seen, handler)

	fp := identity.QuotaFingerprint{Platform: "win32", Arch: "x64"}
	resp, err := newClient(t, srv.URL).GetBillingCurrent(context.Background(), "jwt", agent.NewRequestID(), fp)
	if err != nil {
		t.Fatalf("额度查询不应把非 2xx 当传输错误（状态码要交给调用方）: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("StatusCode = %d，want 404", resp.StatusCode)
	}
	body, err := agent.ReadBody(resp)
	if err != nil {
		t.Fatalf("ReadBody 失败: %v", err)
	}
	if !strings.Contains(string(body), "404 page not found") {
		t.Errorf("ReadBody = %q（gzip 应先解压再交给调用方）", body)
	}
	if got := seen.header.Get("X-Os-Category"); got != "windows" {
		t.Errorf("win32 的 X-Os-Category = %q，want windows", got)
	}
	if got := seen.header.Get("X-Platform"); got != "win32-x64" {
		t.Errorf("X-Platform = %q，want win32-x64", got)
	}
}

// billing 的 401 是**空体且无 Content-Encoding**（与 usage 的 gzip 404 不同）。
func TestBillingUnauthorizedEmptyBody(t *testing.T) {
	var seen record
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}
	srv := fakeUpstream(t, &seen, handler)

	c := newClient(t, srv.URL)
	fp := identity.QuotaFingerprint{Platform: "darwin", Arch: "arm64"}

	for _, get := range []func() (*http.Response, error){
		func() (*http.Response, error) {
			return c.GetBillingCurrent(context.Background(), "jwt", "11111111-2222-4333-8444-555555555555", fp)
		},
		func() (*http.Response, error) {
			return c.GetBillingBalance(context.Background(), "jwt", "11111111-2222-4333-8444-555555555555", fp)
		},
	} {
		resp, err := get()
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("StatusCode = %d，want 401", resp.StatusCode)
		}
		body, err := agent.ReadBody(resp)
		if err != nil {
			t.Fatalf("ReadBody 失败: %v", err)
		}
		if len(body) != 0 {
			t.Errorf("401 的体应为空，实际 %q", body)
		}
	}
}

// 信封 `code != 0` 也是错误，且报错带上游的 `msg`。
func TestEnvelopeNonZeroCodeIsError(t *testing.T) {
	var seen record
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(gz(t, `{"code":1001,"msg":"bad token","data":null,"logid": "x"}`))
	}
	srv := fakeUpstream(t, &seen, handler)

	_, err := newClient(t, srv.URL).OAuthInit(context.Background(), "t", "zai")
	if err == nil {
		t.Fatal("code != 0 应当返回错误")
	}
	if !strings.Contains(err.Error(), "1001") || !strings.Contains(err.Error(), "bad token") {
		t.Errorf("错误应含 code 与 msg，实际: %v", err)
	}
}

// deflate 的两种封装都要能解（httpx 先按 zlib 试、失败再按裸 deflate 试）。
func TestDeflateBothWrappings(t *testing.T) {
	payload := `{"code":0,"msg":"","data":{"status":"ready"},"logid": "x"}`

	for _, tc := range []struct {
		name string
		enc  func(string) []byte
	}{
		{"zlib", func(s string) []byte {
			var b bytes.Buffer
			zw := zlib.NewWriter(&b)
			_, _ = zw.Write([]byte(s))
			_ = zw.Close()
			return b.Bytes()
		}},
		{"raw-deflate", func(s string) []byte {
			// 裸 deflate：把 zlib 封装（2 字节头 + 4 字节 Adler-32 尾）剥掉。
			var b bytes.Buffer
			zw := zlib.NewWriter(&b)
			_, _ = zw.Write([]byte(s))
			_ = zw.Close()
			raw := b.Bytes()
			return raw[2 : len(raw)-4]
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var seen record
			handler := func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Encoding", "deflate")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(tc.enc(payload))
			}
			srv := fakeUpstream(t, &seen, handler)

			status, err := newClient(t, srv.URL).OAuthPoll(context.Background(), "t", "f")
			if err != nil {
				t.Fatalf("解压 %s 失败: %v", tc.name, err)
			}
			if status != "ready" {
				t.Errorf("status = %q", status)
			}
		})
	}
}

// 未支持的编码必须明确报错，不能把压缩字节当明文交出去。
func TestUnknownEncodingIsError(t *testing.T) {
	var seen record
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("nonsense"))
	}
	srv := fakeUpstream(t, &seen, handler)

	_, err := newClient(t, srv.URL).OAuthInit(context.Background(), "t", "zai")
	if err == nil || !strings.Contains(err.Error(), "br") {
		t.Fatalf("未支持编码应明确报错并带上编码名，实际: %v", err)
	}
}

// `NewRequestID` 必须每次不同、且是 uuid4 形态（同一批共用、不同批不同）。
func TestNewRequestIDShape(t *testing.T) {
	a := agent.NewRequestID()
	b := agent.NewRequestID()
	if a == b {
		t.Error("两次生成的 X-Request-Id 不应相同")
	}
	if len(a) != 36 || a[14] != '4' || !strings.Contains("89ab", string(a[19])) {
		t.Errorf("不是 uuid4 形态: %q", a)
	}
}

// assertHeaderSet 断言请求头**恰好**是这些键（多余/缺失都算偏离契约）。
// `Host` 由 net/http 落在 `r.Host` 而不是 Header，所以不会出现在这里。
func assertHeaderSet(t *testing.T, h http.Header, want []string) {
	t.Helper()
	got := make(map[string]bool, len(h))
	for k := range h {
		got[k] = true
	}
	for _, k := range want {
		if !got[k] {
			t.Errorf("缺少请求头 %s（实际 %v）", k, keys(h))
		}
		delete(got, k)
	}
	if len(got) > 0 {
		t.Errorf("多出请求头 %v（契约要求逐字固定）", keysOf(got))
	}
}

func keys(h http.Header) []string {
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, k)
	}
	return out
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// 保险：信封解析不依赖键序（上游序列化时 `logid` 前有空格也不影响）。
func TestEnvelopeTolerantToWhitespace(t *testing.T) {
	var seen record
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"msg":"","data": {"flow_id": "ff","authorize_url":"u","expires_at":1,"poll_interval_sec":0,"poll_token":"p"},"logid": "x"}`))
	}
	srv := fakeUpstream(t, &seen, handler)

	flow, err := newClient(t, srv.URL).OAuthInit(context.Background(), "t", "zai")
	if err != nil {
		t.Fatalf("OAuthInit 失败: %v", err)
	}
	if flow.FlowID != "ff" || flow.AuthorizeURL != "u" {
		t.Errorf("解出的 flow 不对: %+v", flow)
	}

	// 顺带断言：不带 Content-Encoding 时按明文读。
	var raw map[string]any
	_ = json.Unmarshal(seen.body, &raw)
}
