package captcha

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/LinYuanNull/zcode2api-go/internal/agent"
)

// 验证码配置的取值来源。
//
// 上游把 SDK 初始化参数放在**公开目录**里：`GET /api/v1/client/configs?app_version=…`
// 的 `data.configs.captcha` 对象。那份响应是**已采样**的
// （`docs/contract/outbound/01-client-configs.GET.json`，23,313 B 全文见
// `fixtures/client-configs.json`），其中 captcha 对象逐字节是：
//
//	{"enabled":true,"prefix":"no8xfe","region":"cn","sceneId":"11xygtvd","skip_model_request":true}
//
// 管理面回执（`15-claim-captcha-config.GET.json`）记的是它的四个字段：
//
//	{"enabled":true,"scene_id":"11xygtvd","region":"cn","prefix":"no8xfe"}
//
// 两处**同源**：管理面把 `sceneId` 改名成 `scene_id`，丢掉 `skip_model_request`。

// Default 是「取不到上游配置」时用的取值。
//
// 这不是本实现编的：它就是上游自己的静态默认值，也正是采样时观测到的取值
// —— 样本 `15-*` 与公开目录夹具的 captcha 对象在这里**完全一致**。
// 换言之：「拉不到就回落默认值」与「拉到了」在当前环境里产出同一个响应体。
var Default = Config{
	Enabled: true,
	SceneID: "11xygtvd",
	Region:  "cn",
	Prefix:  "no8xfe",
}

// ConfigSource 拉客户端公开目录的响应（`*agent.Client` 满足）。
//
// 取 `*http.Response` 而不是 `[]byte`：出站那条响应是 gzip，解压是本仓库
// `agent.ReadBody` 的职责（A5-3 已确立的唯一解压入口），这里不另造一份。
type ConfigSource interface {
	GetConfigs(ctx context.Context) (*http.Response, error)
}

// remoteCaptcha 是公开目录里 `data.configs.captcha` 的形状。
type remoteCaptcha struct {
	Enabled bool   `json:"enabled"`
	Prefix  string `json:"prefix"`
	Region  string `json:"region"`
	SceneID string `json:"sceneId"`
	// SkipModelRequest 只存在于公开目录，管理面回执里没有它。
	// 本实现**不消费**它（上游拿它控制别的东西，与求解无关），
	// 保留字段是为了让「公开目录的形状」在代码里是完整的。
	SkipModelRequest bool `json:"skip_model_request"`
}

// clientConfigsEnvelope 只声明解析所需的那条路径。
//
// 用 `json.RawMessage` 承接 `configs`：公开目录有 14 个 configs 子对象，
// 声明成结构体就得把它们的形状都写一遍（而它们大多与验证码无关）。
type clientConfigsEnvelope struct {
	Data struct {
		Configs struct {
			Captcha *remoteCaptcha `json:"captcha"`
		} `json:"configs"`
	} `json:"data"`
}

// fetchTimeout 是拉配置的上游默认超时（Options.FetchTimeout 的默认值）。
const fetchTimeout = 15 * time.Second

// fetchConfig 拉一次真实配置；任何失败都回落到 Default（**并返回错误供日志**）。
//
// 回落而不是报错，是因为配置本身有静态默认值可用，拿不到当前值不是致命错误
// —— 这与上游一致，也是 `15-*` 样本的形态来源之一。
//
// 超时由调用方（`resolveConfig`）在 ctx 上施加，这里不再套一层。
func (m *Manager) fetchConfig(ctx context.Context) (Config, error) {
	if m.src == nil {
		return Default, nil
	}

	resp, err := m.src.GetConfigs(ctx)
	if err != nil {
		return Default, fmt.Errorf("拉取客户配置失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		_, _ = agent.ReadBody(resp) // 排空以便连接复用（读失败无所谓）
		return Default, fmt.Errorf("客户配置 HTTP %d", resp.StatusCode)
	}
	body, err := agent.ReadBody(resp)
	if err != nil {
		return Default, fmt.Errorf("读取客户配置失败: %w", err)
	}
	var env clientConfigsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Default, fmt.Errorf("解析客户配置失败: %w", err)
	}
	c := env.Data.Configs.Captcha
	if c == nil {
		return Default, fmt.Errorf("客户配置里没有 captcha 对象")
	}
	return Config{
		Enabled: c.Enabled,
		SceneID: strings.TrimSpace(c.SceneID),
		Region:  strings.TrimSpace(c.Region),
		Prefix:  strings.TrimSpace(c.Prefix),
	}, nil
}

// withFallbacks 对**空字段**逐项回落默认值。
//
// 上游求解时的写法就是「`config.get("x") or DEFAULTS["x"]`」——逐字段回落，
// 而不是整对象回落。区别是实的：上游可以返回一个 `sceneId` 为空串的 captcha
// 对象（管理面会照原样透出空串），此时求解侧必须自己补上默认值。
func (c Config) withFallbacks() Config {
	if c.SceneID == "" {
		c.SceneID = Default.SceneID
	}
	if c.Region == "" {
		c.Region = Default.Region
	}
	if c.Prefix == "" {
		c.Prefix = Default.Prefix
	}
	return c
}
