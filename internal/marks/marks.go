// Package marks 路由记号日志：把请求生命周期里的关键节点打成固定前缀的行。
//
// # 为什么单独一个包
//
// 网关的调度器、安装序（install）、遥测（telemetry）都要打这种行，
// 而它们分属不同包。**记号的拼装规则是跨包共享的**（缩进、前缀、分隔空格），
// 各包各写一遍必然漂移 —— 一旦漂移，`tools/behavior_diff.py` 的记号序列比对就会
// 把「格式差异」误判成「行为差异」。所以规则只在这里写一次。
//
// # 形态（逐字取自靶机实测，见 docs/contract/outbound/behavior.md §3.0）
//
//	<TAB×4><SP×3>>>> <reqid>  <model>  sync|stream  "<preview>"
//	<TAB×4><SP×3>[~] <reqid> <正文>
//	<TAB×4><SP×3><!> <reqid>  <正文>
//
// 注意 reqid 之后：`>>>` 与 `<!>` 是**两个空格**，只有 `[~]` 是一个。
// 这个不对称是实测事实，不要"顺手统一"。
package marks

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// 前缀（不含缩进）。
const (
	PrefixRoute  = ">>>"
	PrefixNotice = "[~]"
	PrefixFail   = "<!>"
	PrefixOK     = "[+]"
)

// indent 是记号行的缩进：4 个 TAB + 3 个空格（实测 lead=7）。
const indent = "\t\t\t\t   "

// Writer 是记号输出目标。抽成接口是为了测试可替换，也为了将来接 reqlog 环形缓冲。
type Writer interface {
	MarkLine(line string)
}

// Stdout 是默认实现：写 stderr，与靶机一致（靶机记号走 logging 的 stderr 流，
// 不会污染 stdout 的管道输出）。
type Stdout struct {
	mu sync.Mutex
	w  io.Writer
}

// NewStdout 建一个写 stderr 的 Writer。
func NewStdout() *Stdout { return &Stdout{w: os.Stderr} }

// MarkLine 实现 Writer。写入时补换行并加锁 —— 多个账号的重试日志会并发打，
// 不加锁会出现半行交错。
func (s *Stdout) MarkLine(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w == nil {
		s.w = os.Stderr
	}
	io.WriteString(s.w, indent+line+"\n")
}

// ReqID 生成一个 16 位小写 hex 的请求 ID。
//
// **不用 uuid**：靶机的 reqid 是 16 hex（8 字节），不是 32 hex 的 UUID4。
// 长度是可观测事实（记号行对齐比对会看它），所以用 8 字节而不是 16。
func ReqID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败是不可恢复的（没有可退的确定性来源）。返回全 0：
		// 格式仍然合法，只是不唯一 —— 宁可记号重复，也不要 panic 打挂整个网关。
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// Route 打 `>>>` 行：请求进入路由时打，与最终结果无关。
//
// model / streamLabel / preview 三项都来自 bodytransform.Inspect，
// 取值规则（真值判定、末条 user、缺键回落）全在那里，本包不重复实现。
func Route(w Writer, reqID, model, streamLabel, preview string) {
	w.MarkLine(fmt.Sprintf("%s %s  %s  %s  %q",
		PrefixRoute, reqID, model, streamLabel, preview))
}

// Notice 打 `[~]` 行：过程中的可行动信息（切账号、重试、错误体转储）。
func Notice(w Writer, reqID, format string, args ...any) {
	w.MarkLine(fmt.Sprintf("%s %s %s", PrefixNotice, reqID, fmt.Sprintf(format, args...)))
}

// Fail 打 `<!>` 行：请求最终失败。
func Fail(w Writer, reqID, format string, args ...any) {
	w.MarkLine(fmt.Sprintf("%s %s  %s", PrefixFail, reqID, fmt.Sprintf(format, args...)))
}

// OK 打 `[+]` 行：阶段性完成（如安装序结束）。
func OK(w Writer, what, format string, args ...any) {
	body := fmt.Sprintf(format, args...)
	if strings.TrimSpace(body) == "" {
		w.MarkLine(fmt.Sprintf("%s %s", PrefixOK, what))
		return
	}
	w.MarkLine(fmt.Sprintf("%s %s %s", PrefixOK, what, body))
}
