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
	// OAuthUserAgent 是 OAuth init/poll 的 UA：**实测与遥测同值**（都是裸 httpx
	// 客户端直接打），与转发链路的 `ZCode/3.14.4` 不同（outbound-admin 3.1）。
	// 分开命名是为了将来一处变了不牵连另一处。
	OAuthUserAgent = EventUserAgent
)

// A5 额度查询的固定指纹头取值（outbound-admin 4.2）。
const (
	// ReleaseChannel 实测恒为 `stable`，不随账号变化。
	ReleaseChannel = "stable"
	// ClientTitle 逐字含 `@`，不要改写成 `Z Code electron`。
	ClientTitle = "Z Code@electron"
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

// ── A5 管理侧出站（OAuth 设备码 + 额度查询）─────────────────────
//
// 依据：docs/contract/outbound-admin/observations.md 三、四。
// 这里与转发链路有一处**真实的差异**，不要统一：转发只带「网络身份头」，
// 额度查询带**全套客户端指纹头**，而 OAuth 是**极简头**。

// OAuthInit 返回 `POST /api/v1/oauth/cli/init` 的出站头。
//
// **极简**：没有 `Content-Type` 以外的任何 `X-*` 头（observations.md 3.1）。
func OAuthInit(token string) http.Header {
	h := oauthBase(token)
	h.Set("Content-Type", "application/json")
	return h
}

// OAuthPoll 返回 `GET /api/v1/oauth/cli/poll/<flow_id>` 的出站头（比发起少一个
// `Content-Type` —— 它没有请求体）。
func OAuthPoll(token string) http.Header { return oauthBase(token) }

func oauthBase(token string) http.Header {
	return http.Header{
		"Accept":          []string{acceptAll},
		"Accept-Encoding": []string{acceptEncoding},
		"Authorization":   []string{"Bearer " + token},
		"Connection":      []string{keepAlive},
		"User-Agent":      []string{OAuthUserAgent},
	}
}

// QuotaFingerprint 是额度查询需要的客户端指纹字段。
//
// 与 `models.Fingerprint` 同域（少一个 `screen`），但这里**刻意不依赖 models**：
// 本包只处理线格式，不该认识业务类型。
type QuotaFingerprint struct {
	Platform  string // darwin | win32
	Arch      string // arm64 | x64
	OSVersion string
	Language  string
	Timezone  string
	DeviceMID string
}

// Quota 返回额度查询的出站头。`requestID` 必须是**同一批三条共享**的那个 uuid
// （observations.md 4.1）。
//
// 两个容易「好心改坏」的点：
//   - `Content-Type: application/json` 出现在 **GET** 请求上（实测如此，照发）；
//   - `X-Os-Category` 不是平台原名，要映射：darwin → `macos`、win32 → `windows`；
//     `X-Platform` 才是 `<platform>-<arch>`。
func Quota(token, requestID string, fp QuotaFingerprint) http.Header {
	h := common()
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("Http-Referer", Referer)
	h.Set("X-Client-Language", fp.Language)
	h.Set("X-Client-Timezone", fp.Timezone)
	h.Set("X-Device-Mid", fp.DeviceMID)
	h.Set("X-Os-Category", OSCategory(fp.Platform))
	h.Set("X-Os-Version", fp.OSVersion)
	h.Set("X-Platform", fp.Platform+"-"+fp.Arch)
	h.Set("X-Release-Channel", ReleaseChannel)
	h.Set("X-Request-Id", requestID)
	h.Set("X-Title", ClientTitle)
	h.Set("X-Zcode-App-Version", AppVersion)
	return h
}

// OSCategory 把账号指纹的 `platform` 映射成出站头 `X-Os-Category` 的取值。
//
// 依据：12 次抽样实测（outbound-admin 4.2）—— darwin → `macos`、win32 → `windows`。
// **未知平台原样返回**：宁可让上游拒掉一个可诊断的怪值，也不要静默猜一个。
func OSCategory(platform string) string {
	switch platform {
	case "darwin":
		return "macos"
	case "win32":
		return "windows"
	default:
		return platform
	}
}
