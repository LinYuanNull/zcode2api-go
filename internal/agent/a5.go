// A5 管理侧出站：端点、头装配与响应信封解析。
//
// 与转发链路（`/v1/messages`）并列的第二类出站。依据：
// docs/contract/outbound-admin/observations.md（5 个样本 + 2 个夹具，全部实测）。
//
// 三条**不能想当然**的实测事实：
//
//  1. **管理侧出站也走环境代理**（同一份 `proxied` 客户端）—— A4 曾按「billing 必须
//     直连」的假设留过直连分支，A5 实测推翻了它（observations.md 二）。
//  2. **响应是 gzip**，而我们必须**显式**发 `Accept-Encoding`（契约要求），
//     于是 Go 的透明解压不生效 ⇒ 得自己解压（见 `readData`）。
//  3. **额度查询一次三条并发、共用同一个 `X-Request-Id`**（observations.md 4.1）。
package agent

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/LinYuanNull/zcode2api-go/internal/identity"
	"github.com/LinYuanNull/zcode2api-go/internal/models"
	"github.com/LinYuanNull/zcode2api-go/internal/pyjson"
)

// A5 端点。**硬编码** —— 与 `MessagesURL` 同样的契约：端点逐字固定、不可配。
//
// 依据：outbound-admin 一（`01-oauth-init` … `05-plan-usage`）。
const (
	OAuthInitURL      = "https://zcode.z.ai/api/v1/oauth/cli/init"
	OAuthPollBase     = "https://zcode.z.ai/api/v1/oauth/cli/poll/"
	PlanUsageURL      = "https://zcode.z.ai/api/v1/zcode-plan/usage"
	BillingCurrentURL = "https://zcode.z.ai/api/v1/zcode-plan/billing/current"
	BillingBalanceURL = "https://zcode.z.ai/api/v1/zcode-plan/billing/balance"
)

// a5Host 是 A5 全部端点的 origin（测试替换基址时按它裁剪）。
const a5Host = "https://zcode.z.ai"

// maxA5Body 是 A5 响应体的上限。这几个端点的响应都很小（几 KB），1 MiB 足够防呆。
const maxA5Body = 1 << 20

// OAuthFlow 是 `POST /api/v1/oauth/cli/init` 的解析结果。
//
// 注意 `PollToken` 与请求头里的 Bearer **同值**（实测，observations.md 3.2）——
// 它由调用方生成后传入，这里只是把上游回显的值一并交回去以便核对。
type OAuthFlow struct {
	FlowID          string
	PollToken       string
	AuthorizeURL    string
	ExpiresAt       int64
	PollIntervalSec int
}

// UpstreamError 是上游返回的**非 2xx**。
//
// 保留**状态码与原始错误体**，因为调用方要靠状态码做判定：
// 额度查询要把 401/404 判成「凭据失效」（observations.md 4.3），
// 登录链路要把失败包成 502 的文案。只给一个字符串是不够的。
type UpstreamError struct {
	Status int
	Body   []byte
}

// Error 实现 error。
func (e *UpstreamError) Error() string {
	b := strings.TrimSpace(string(e.Body))
	if len(b) > 200 {
		b = b[:200] + "…"
	}
	return fmt.Sprintf("上游返回 %d: %s", e.Status, b)
}

// NewRequestID 生成一次额度查询批次的 `X-Request-Id`（uuid4 形态）。
//
// **同一批的三条请求必须共用同一个值**（不同批次之间不同）——
// 这是实测的契约，不是实现细节（outbound-admin 4.1）。
func NewRequestID() string { return models.NewUUID4() }

// SetA5BaseForTest 把 A5 出站（OAuth + 额度）指向本地假上游，**仅供测试**。
//
// 为什么只给一个开关而不是五个：A5 的五个端点**同 host**，替换 origin 即可覆盖全部，
// 比逐个暴露 URL 更不容易被误用成一个「可配端点」的后门。
//
// ⚠️ 生产代码不要调用它 —— 那等于把端点变成可配的，等于放弃契约。
func (c *Client) SetA5BaseForTest(base string) { c.a5Base = strings.TrimSuffix(base, "/") }

// a5url 解析 A5 端点（测试时替换 origin）。
func (c *Client) a5url(u string) string {
	if c.a5Base == "" {
		return u
	}
	return c.a5Base + strings.TrimPrefix(u, a5Host)
}

// OAuthInit 发起一次设备码登录。`token` 是本地生成的 64 位小写 hex（不落盘）。
func (c *Client) OAuthInit(ctx context.Context, token, provider string) (OAuthFlow, error) {
	obj := pyjson.NewObject()
	obj.Set("provider", pyjson.NewString(provider))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.a5url(OAuthInitURL),
		bytes.NewReader(obj.MarshalCompact()))
	if err != nil {
		return OAuthFlow{}, err
	}
	req.Header = identity.OAuthInit(token)
	resp, err := c.proxied.Do(req)
	if err != nil {
		return OAuthFlow{}, err
	}
	data, err := readData(resp)
	if err != nil {
		return OAuthFlow{}, err
	}
	var d struct {
		FlowID          string `json:"flow_id"`
		PollToken       string `json:"poll_token"`
		AuthorizeURL    string `json:"authorize_url"`
		ExpiresAt       int64  `json:"expires_at"`
		PollIntervalSec int    `json:"poll_interval_sec"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return OAuthFlow{}, fmt.Errorf("oauth/cli/init 的 data 不是期望结构: %w", err)
	}
	return OAuthFlow{
		FlowID:          d.FlowID,
		PollToken:       d.PollToken,
		AuthorizeURL:    d.AuthorizeURL,
		ExpiresAt:       d.ExpiresAt,
		PollIntervalSec: d.PollIntervalSec,
	}, nil
}

// OAuthPoll 轮询一个设备码会话，返回上游 `data.status` 原值。
//
// **原值返回**（`pending` / `ready` / `failed` / `expired` …）：映射由 oauth 包负责，
// 这里不认识这些语义，免得两处各有一份枚举、将来对不上。
func (c *Client) OAuthPoll(ctx context.Context, token, flowID string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.a5url(OAuthPollBase+flowID), nil)
	if err != nil {
		return "", err
	}
	req.Header = identity.OAuthPoll(token)
	resp, err := c.proxied.Do(req)
	if err != nil {
		return "", err
	}
	data, err := readData(resp)
	if err != nil {
		return "", err
	}
	var d struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return "", fmt.Errorf("oauth/cli/poll 的 data 不是期望结构: %w", err)
	}
	return d.Status, nil
}

// GetPlanUsage 查一份额度（用量）。
//
// `requestID` 必须与同批的 billing 两条**共用**（见 `NewRequestID`）。
// 返回的响应体**未读**，由调用方决定怎么用（当前只关心状态码）。
func (c *Client) GetPlanUsage(ctx context.Context, jwt, requestID string, fp identity.QuotaFingerprint) (*http.Response, error) {
	return c.quotaGet(ctx, PlanUsageURL, jwt, requestID, fp)
}

// GetBillingCurrent 查当前账期（与 `GetPlanUsage` 同批并发）。
func (c *Client) GetBillingCurrent(ctx context.Context, jwt, requestID string, fp identity.QuotaFingerprint) (*http.Response, error) {
	return c.quotaGet(ctx, BillingCurrentURL, jwt, requestID, fp)
}

// GetBillingBalance 查余额（与 `GetPlanUsage` 同批并发）。
func (c *Client) GetBillingBalance(ctx context.Context, jwt, requestID string, fp identity.QuotaFingerprint) (*http.Response, error) {
	return c.quotaGet(ctx, BillingBalanceURL, jwt, requestID, fp)
}

func (c *Client) quotaGet(ctx context.Context, url, jwt, requestID string, fp identity.QuotaFingerprint) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.a5url(url), nil)
	if err != nil {
		return nil, err
	}
	req.Header = identity.Quota(jwt, requestID, fp)
	return c.proxied.Do(req)
}

// ── 响应信封 ───────────────────────────────────────────────

// envelope 是 zcode 出站响应的外层信封。
//
// 实测形状：`{"code":0,"msg":"","data":{…},"logid":"…"}`
// （注意上游序列化时 `logid` 前**带空格** —— 那是上游的事，解析不受影响）。
type envelope struct {
	Code  int             `json:"code"`
	Msg   string          `json:"msg"`
	Data  json.RawMessage `json:"data"`
	LogID string          `json:"logid"`
}

// readData 读出信封，校验 HTTP 状态与 `code`，返回 `data` 原文。
func readData(resp *http.Response) (json.RawMessage, error) {
	body, err := ReadBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &UpstreamError{Status: resp.StatusCode, Body: body}
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("上游响应不是合法 JSON（%d 字节）: %w", len(body), err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("上游返回 code=%d: %s", env.Code, env.Msg)
	}
	if len(env.Data) == 0 {
		return nil, errors.New("上游响应缺少 data 字段")
	}
	return env.Data, nil
}

// ReadBody 读出响应体，按 `Content-Encoding` **自行解压**，返回明文并关闭响应体。
//
// ⚠️ 为什么不能指望 Go 的透明解压：`Transport` 只在「**它自己**加上的
// `Accept-Encoding`」上做透明解压；而我们的头是**显式**设的（契约要求逐字发送
// `gzip, deflate`），此时它会把压缩字节**原样**交出来。实测上游这几个端点回 gzip
// （outbound-admin 的 5 个样本里 4 个是 `Content-Encoding: gzip`，连 404 的
// `404 page not found` 都是 gzip 的）。
//
// **为什么导出**：额度查询那三条路径（`GetPlanUsage` / `GetBilling*`）按设计
// **不读响应体**返回 —— 调用方要自己按状态码分派（401/404 ⇒ 凭据失效，见
// observations.md 4.3）。既然解压这步调用方也必须做，就不能只留一个包内函数，
// 否则 A5-3 会照抄一份 gzip 逻辑，两处将来必然漂移。
//
// deflate 的处理与参考实现（httpx）一致：**先按 zlib 试，失败再按裸 deflate 试**。
// 这不是「猜协议」—— 主流客户端都这么做，因为「deflate」在实践中两种封装都出现过。
func ReadBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxA5Body))
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "", "identity":
		return raw, nil
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("解压 gzip 响应失败: %w", err)
		}
		defer zr.Close()
		return io.ReadAll(io.LimitReader(zr, maxA5Body))
	case "deflate":
		if out, err := inflate(raw, true); err == nil {
			return out, nil
		}
		return inflate(raw, false)
	default:
		return nil, fmt.Errorf("未支持的上游响应编码 %q", resp.Header.Get("Content-Encoding"))
	}
}

func inflate(raw []byte, zlibWrapped bool) ([]byte, error) {
	var r io.ReadCloser
	if zlibWrapped {
		zr, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		r = zr
	} else {
		r = flate.NewReader(bytes.NewReader(raw))
	}
	defer r.Close()
	return io.ReadAll(io.LimitReader(r, maxA5Body))
}
