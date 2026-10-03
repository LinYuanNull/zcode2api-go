// Package buildinfo 承载编译期注入的版本信息。
//
// 版本号有**两个**消费点，两者在靶机上是**不同的值**（实测：`GET /meta` →
// `2.6.8`，而页面里的 `{{APP_VERSION}}` 被替换成 `2.6.3`）：
//
//   - `GET /meta` 的 `version` —— 面板头部（`js/header.js`）拉它显示 `v<version>`；
//   - 页面里的 `{{APP_VERSION}}` —— **只用作静态资源的 cache-buster**
//     （`app.css?v={{APP_VERSION}}`），不参与任何判断。
//
// 本实现是独立实现，两个位置统一用**自己的**版本号：面板头部显示的是
// 「这个进程的版本」，语义比显示上游前端版本更正确；cache-buster 只要求
// 每次发版变化，自己的版本号天然满足。
//
// 注入方式（发布脚本用）：
//
//	go build -ldflags "-X github.com/LinYuanNull/zcode2api-go/internal/buildinfo.Version=v0.1.0"
package buildinfo

// Version 是版本号。默认值带 `-dev` 后缀，避免把开发构建误认成发布版。
var Version = "0.1.0-dev"
