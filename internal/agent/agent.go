// Package agent 是出站上游客户端：端点选择、鉴权头装配、传输层（代理 / 证书 / HTTP 版本）。
//
// 端点与头全部是**实测常量**（docs/contract/outbound/observations.md 一、二、三之四）：
//
//	消息转发  POST https://api.z.ai/api/anthropic/v1/messages
//	客户端配置 GET  https://zcode.z.ai/api/v1/client/configs?app_version=3.14.4
//	遥测上报  POST https://zcode.z.ai/api/v1/event/report
//
// **地址硬编码**：10 个候选环境变量（`ZCODE_UPSTREAM_BASE` 等）实测全部不生效
// （observations.md #6），所以本包不提供 URL 覆盖开关。
//
// 那测试怎么把流量引到假上游？**走代理**：出站读 `HTTPS_PROXY`（#7），
// 于是 `tools/mitmupstream` 以 HTTP 代理身份终结 TLS 即可捕获明文 ——
// 不需要、也不应该给实现加「改地址」的后门。
package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LinYuanNull/zcode2api-go/internal/identity"
)

// 上游端点。`MessagesURL` 的路径与 `client/configs` 里 `z-ai` + `schema=anthropic`
// 的 `baseUrl` + `/v1/messages` 一致（observations.md #3）。
const (
	MessagesURL = "https://api.z.ai/api/anthropic/v1/messages"
	ConfigsURL  = "https://zcode.z.ai/api/v1/client/configs"
	EventURL    = "https://zcode.z.ai/api/v1/event/report"
)

// maxBody 是出站请求体的上限（与入站一致，防呆）。
const maxBody = 1 << 22

// DialTimeout 是建连超时。
//
// ⚠️ **未覆盖**：参考实现用 httpx 的默认超时（连接/读/写各 5s）。「上游卡住多久算失败」
// 属未采样分支，这里**不猜**一个读超时（猜错会把长 SSE 流误杀）。
// 只设建连超时，读侧不设总超时。已登记在 behavior.md 第六节。
const DialTimeout = 30 * time.Second

// Client 是出站客户端。
type Client struct {
	proxied *http.Client // 走环境代理（HTTPS_PROXY / HTTP_PROXY / NO_PROXY）

	// messagesURL 允许测试指向本地假上游。生产恒为 MessagesURL 常量
	// （`New()` 不设它 ⇒ 零值回落到常量）。
	messagesURL string

	// a5Base 允许测试替换 A5 出站（OAuth + 额度）的 origin，见 `SetA5BaseForTest`。
	// 生产恒为空 ⇒ 用 a5.go 里的端点常量。
	a5Base string
}

// endpoint 返回实际使用的消息转发端点。
func (c *Client) endpoint() string {
	if c.messagesURL != "" {
		return c.messagesURL
	}
	return MessagesURL
}

// New 建一个出站客户端。
func New() *Client {
	return &Client{proxied: newHTTPClient()}
}

// SetMessagesURLForTest 让本客户端指向本地假上游，**仅供测试**。
//
// 为什么需要它：`MessagesURL` 是编译期常量（契约：端点逐字固定、不可配），
// 而调度器测试要打到 `httptest` 的本地服务。
//
// ⚠️ 生产代码不要调用它 —— 那等于把端点变成可配的，等于放弃契约。
func (c *Client) SetMessagesURLForTest(u string) { c.messagesURL = u }

// Proxied 返回走环境代理的客户端。
//
// **只有这一个出站客户端**：A4 曾按「billing 与验证码必须直连」的假设留过一个
// `Direct()` 变体，但 A5 实测**推翻了**它 —— 管理侧出站（**含 billing**）
// 同样走 `HTTPS_PROXY`（observations.md 二）。没有实测支持的分支不留，
// 免得将来有人顺手用它把流量打直连、在上游风控前暴露真实出口 IP。
func (c *Client) Proxied() *http.Client { return c.proxied }

// PostMessages 发送一条消息转发请求。`token` 是账号凭据明文。
//
// 返回的响应体**未读**，由调用方决定是逐字节透传还是解析（这是两个入口语义
// 不同的根源：`/v1/messages` 透传、`/v1/chat/completions` 必须解析）。
func (c *Client) PostMessages(ctx context.Context, token string, body []byte) (*http.Response, error) {
	if len(body) > maxBody {
		return nil, errTooLarge
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = identity.Messages(token)
	return c.proxied.Do(req)
}

// GetConfigs 拉客户端配置（公开目录，**不带鉴权**，observations.md #20）。
func (c *Client) GetConfigs(ctx context.Context) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ConfigsURL+"?app_version="+identity.AppVersion, nil)
	if err != nil {
		return nil, err
	}
	req.Header = identity.Configs()
	return c.proxied.Do(req)
}

// PostEvent 上报一个遥测事件。
func (c *Client) PostEvent(ctx context.Context, body []byte) (*http.Response, error) {
	if len(body) > maxBody {
		return nil, errTooLarge
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, EventURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = identity.Event()
	return c.proxied.Do(req)
}

// DrainAndClose 读完并关闭响应体，让连接可复用。
func DrainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	_ = resp.Body.Close()
}

// ---------------------------------------------------------------- 传输层

type tooLargeError struct{}

func (tooLargeError) Error() string { return "出站请求体超过上限" }

var errTooLarge = tooLargeError{}

// newHTTPClient 建出站客户端。
//
// 三处刻意为之（每一处都对应一条实测事实或一条已知的 Go 陷阱）：
//
//  1. **HTTP/2 关闭**。参考实现用的 httpx **默认只协商 HTTP/1.1**；而且
//     `Connection: keep-alive` 是连接级头，Go 的 HTTP/2 实现会直接拒绝
//     带它的请求（`http2: invalid Connection request header`）。所以关掉 h2
//     既是保真、也是让那个头能发出去的前提。
//  2. **信任 `SSL_CERT_FILE` / `SSL_CERT_DIR`**。httpx 在 `trust_env=True` 时读它们
//     （observations.md #8），这是把流量导向本地假上游的另一半前提。
//     注意 Go 在 Windows 上**不认**这两个变量，必须自己加载。
//  3. **代理读环境变量**，与 #7 一致。**所有**出站都走这一条路径 ——
//     A5 实测管理侧出站（含 billing）同样走代理，没有直连分支（见 `Proxied`）。
func newHTTPClient() *http.Client {
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: DialTimeout}).DialContext,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{}, // 空表 = 禁用 h2
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{RootCAs: rootCAs()},
		Proxy:                 http.ProxyFromEnvironment,
	}
	return &http.Client{Transport: tr}
}

// rootCAs 组装出站信任的根证书池。
//
// 默认用系统池；`SSL_CERT_FILE` / `SSL_CERT_DIR` 有值时**追加**（不是替换）。
//
// 为什么是追加：参考实现在 Windows 上走 `ssl.SSLContext.load_default_certs()`，
// 它**先**读 `SSL_CERT_FILE`/`SSL_CERT_DIR`、**再**枚举系统证书存储，两者是并集。
// （Linux 上 Python 是替换语义 —— 该差异属未覆盖，见 behavior.md 第六节。）
func rootCAs() *x509.CertPool {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	for _, f := range extraCertFiles() {
		pem, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		pool.AppendCertsFromPEM(pem)
	}
	return pool
}

// extraCertFiles 返回 `SSL_CERT_FILE` / `SSL_CERT_DIR` 指到的 PEM 文件。
func extraCertFiles() []string {
	var out []string
	if f := strings.TrimSpace(os.Getenv("SSL_CERT_FILE")); f != "" {
		out = append(out, f)
	}
	if d := strings.TrimSpace(os.Getenv("SSL_CERT_DIR")); d != "" {
		entries, err := os.ReadDir(d)
		if err == nil {
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				out = append(out, filepath.Join(d, e.Name()))
			}
		}
	}
	return out
}
