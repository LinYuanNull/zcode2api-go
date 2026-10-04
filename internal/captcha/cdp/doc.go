// Package cdp 是**自写**的极简 Chrome DevTools Protocol 客户端：手写 WebSocket
// 帧 + 按 `id` 配对的 JSON-RPC 会话。它替代上游的 Node 求解脚本
// （`captcha_node/solver_pw.js`，puppeteer-core 驱动）。
//
// # 为什么手写而不是引库
//
// Go 标准库没有 WebSocket 客户端。两条路：引一个通用 WebSocket 库，或手写
// 帧解析。这里选手写，因为需求是 RFC 6455 的一个**极小子集** —— 客户端侧握手、
// 文本帧、掩码、分片重组、Ping/Pong/Close 三个控制帧；不需要任何扩展、不需要
// permessage-deflate、不需要服务端侧。手写反而少一层不受控的抽象，也让本仓库
// 的直接依赖保持在 1 个（`modernc.org/sqlite`）。
//
// # 用到的命令
//
// 求解全流程只用 7 条命令（见 `cdp.go` 末尾的类型化封装）：
//
//	Target.createTarget                    开一个 about:blank 目标
//	Target.attachToTarget                  取 sessionId（flatten 模式）
//	Page.addScriptToEvaluateOnNewDocument  反探测补丁（必须早于导航）
//	Emulation.setUserAgentOverride         改 UA（必须早于导航）
//	Page.navigate                          打开本地求解页
//	Runtime.evaluate                       轮询就绪 + 执行求解表达式（awaitPromise）
//	Target.closeTarget                     收尾
//
// 计划文档写的是「6 条」，漏了 `Target.attachToTarget` —— 没有它就无法把页面级
// 命令投递到新开的目标上（`Target.createTarget` 返回的 `targetId` 不等于会话，
// 页面级命令要么随消息带 `sessionId`、要么另开一条页面级 WebSocket）。已在
// PROVENANCE.md 登记这处更正。
//
// # 刻意不用的东西
//
// `Page.enable`：求解靠轮询 `document.readyState` 判断就绪，不依赖 load 事件，
// 少一条命令少一处失败点。
//
// `--remote-debugging-pipe`：免端口的管道模式更快，但它走 fd 3/4 的 NUL 分隔
// 协议，跨平台拉子进程时拿 fd 很别扭；本项目要跑在 Windows 上，用端口更稳。
//
// # 本包不做的事
//
// 不启动浏览器、不定位可执行文件、不写求解页 —— 那些属于 `internal/captcha`
// 的求解器。本包只回答「怎么把命令发过去、怎么把结果取回来」。
//
// 覆盖：`cdp_test.go` 用**进程内**假 CDP 服务（真握手 + 真帧编解码 + 真 JSON-RPC）
// 覆盖帧层与会话层；真机验收在 `internal/captcha` 与 `tools/e2e_captcha.py`。
package cdp
