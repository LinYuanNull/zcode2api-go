// Package captcha 验证码配置 + 求解器调度。
//
// 两条链路，各有一条样本依据：
//
//  1. **配置**（本包 `Config`）：`GET /admin/api/claim/captcha-config` 的响应体，
//     依据样本 `15-claim-captcha-config.GET.json`。取值来自上游公开目录
//     `client/configs` 的 `data.configs.captcha`（同一份样本集里的
//     `outbound/01-client-configs.GET.json` + `fixtures/client-configs.json`），
//     键顺序 `enabled, scene_id, region, prefix` 取自样本。
//
//  2. **求解**（`internal/captcha/cdp` + 本包的 `CDPSolver` / `Manager`）：
//     在真实浏览器里跑阿里云无痕验证 SDK，产出
//     `X-Aliyun-Captcha-Verify-Param`。这是对上游 Node 求解脚本的**独立 Go 实现**
//     —— 契约只有一条：它必须产出 SDK 成功回调里的 `captchaVerifyParam`。
//
// # 与上游的两处结构性差异（都是刻意的，见 PROVENANCE.md）
//
//   - **没有预解池**：上游后台维护 MIN=1/MAX=2 的池，代价是常驻求解进程；
//     本实现「用时现解」（计划文档明确允许）。因此 token TTL 与 invalidate()
//     在本实现里没有对应行为，已登记为未实现而不是假装实现。
//   - **求解器只用系统已装浏览器**：项目铁律禁止随包分发第三方二进制。
//     定位顺序 `$ZCODE_CHROMIUM_PATH` → Edge → Chrome（Windows）/ 常见路径（*nix）。
//
// # 与上游的一处有意偏离
//
// 配置拉取**失败也落缓存**（60s），避免断网时每次请求都卡满 15s 的上游超时。
// 响应体不变，只是把「配置恢复」的可见延迟上界从 0 放宽到 60s。
//
// # 尚未接通的接缝
//
// 自动领取（`POST /admin/api/claim` 的成功路径）**仍未接通**，原因是领取端点
// 本身没有出站样本（`outbound-admin/observations.md` 一之表只有 5 个端点，
// 不含领取）。求解器现在是可用的（CLI `captcha` 子命令可实测），但「解出来
// 的凭据往哪发」仍未采样 ⇒ `claim` 的成功路径继续显式报错（501）。
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

// Static 返回固定配置（离线装配、测试、手工配置用）。
type Static struct{ C Config }

// Config 实现 Provider。
func (s Static) Config() Config { return s.C }
