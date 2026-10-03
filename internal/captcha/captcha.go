// Package captcha 验证码池与求解调度。
//
// A3 只定义**配置接缝**（`GET /admin/api/claim/captcha-config` 的响应体）；
// 求解器（自写 CDP 客户端）属 A6，在 `captcha/cdp` 子包。
//
// 依据：`15-claim-captcha-config.GET.json` —— 返回阿里验证码 SDK 初始化参数
// `{enabled, scene_id, region, prefix}`；notes 明确「取值来自上游验证码服务
// （随环境变化，可能为空串）」。
package captcha

// Config 是 `GET /admin/api/claim/captcha-config` 的响应体。
//
// **键顺序取自样本**：`enabled, scene_id, region, prefix`（不是字典序，
// 也不是 SPEC 表格里写的顺序 —— 以样本文件为准）。
type Config struct {
	Enabled bool   `json:"enabled"`
	SceneID string `json:"scene_id"`
	Region  string `json:"region"`
	Prefix  string `json:"prefix"`
}

// Provider 提供验证码配置。
type Provider interface {
	Config() Config
}

// Unavailable 是 Provider 的占位实现：返回「未启用 + 空串」。
//
// 这是**诚实的降级**而不是伪造：notes 说取值随环境变化、可能为空串，
// 所以「拿不到上游配置」的可观测形态就是空串。真实取值由 A6 从上游拉。
type Unavailable struct{}

// Config 实现 Provider。
func (Unavailable) Config() Config { return Config{} }

// Static 返回固定配置（测试与手工配置用）。
type Static struct{ C Config }

// Config 实现 Provider。
func (s Static) Config() Config { return s.C }
