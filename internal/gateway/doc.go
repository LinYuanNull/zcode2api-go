// Package gateway 转发网关：调度器、并发槽、SSE 透传、错误分类与冷却。
//
// 对应计划中的 A4 阶段。这是全项目最难的一期，
// asyncio 单线程原子语义必须在 Go 里重新论证，不能凭感觉移植。
package gateway
