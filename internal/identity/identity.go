// Package identity 构造出站身份头与追踪头。
//
// 所有取值都是**实测常量**：对 41 条真实转发请求逐头统计，11 个头 41/41 全命中
// （docs/contract/outbound/observations.md #47）。改动前请先读那份文档 —— 这些字符串
// 是上游用来识别客户端的，不是随便写的 UA。
package identity

import "net/http"

// 客户端身份常量。`AppVersion` 同时出现在三个地方：`User-Agent`、`X-Zcode-App-Version`
// 与 `client/configs` 的 `app_version` 查询参数 —— 三处必须同源。
const (
	AppVersion       = "3.14.4"
	AnthropicVersion = "2023-06-01"
	AgentName        = "glm"
	Referer          = "https://zcode.z.ai"
	UserAgent        = "ZCode/" + AppVersion
	// EventUserAgent 是遥测链路自己的 UA —— **与消息转发不同**（observations.md #22/#49）。
	EventUserAgent = "python-httpx/0.28.1"
)

// httpx 默认的两个头（`Accept-Encoding` 在 httpx 0.28.1 是 `gzip, deflate`）。
const (
	acceptAll      = "*/*"
	acceptEncoding = "gzip, deflate"
	// keepAlive 是 httpx 的默认 `Connection`。**必须真的发出去**：
	// 出站客户端要禁用 HTTP/2（httpx 默认也是 HTTP/1.1），否则 Go 会因
	// `Connection` 是连接级头而拒绝该请求（见 agent.Transport 的说明）。
	keepAlive = "keep-alive"
)

// Messages 返回消息转发的出站头，`token` 是账号凭据明文。
//
// 鉴权头是 **`X-Api-Key`**，不是 `Authorization`（observations.md #17）。
// `Content-Length` 由 HTTP 客户端按 body 长度补，这里不设。
func Messages(token string) http.Header {
	h := common()
	h.Set("Anthropic-Version", AnthropicVersion)
	h.Set("Content-Type", "application/json")
	h.Set("Http-Referer", Referer)
	h.Set("X-Api-Key", token)
	h.Set("X-Zcode-Agent", AgentName)
	h.Set("X-Zcode-App-Version", AppVersion)
	return h
}

// Configs 返回 `client/configs` 的出站头：**只有 4 个**，不带鉴权、不带 Content-Type
// （observations.md #48）。
func Configs() http.Header { return common() }

// Event 返回遥测 `event/report` 的出站头（observations.md #49）。
func Event() http.Header {
	h := common()
	h.Set("Content-Type", "application/json")
	h.Del("User-Agent")
	h.Set("User-Agent", EventUserAgent)
	return h
}

func common() http.Header {
	return http.Header{
		"Accept":          []string{acceptAll},
		"Accept-Encoding": []string{acceptEncoding},
		"Connection":      []string{keepAlive},
		"User-Agent":      []string{UserAgent},
	}
}
