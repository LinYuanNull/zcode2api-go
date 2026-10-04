package captcha

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

// solverPage 是求解页的源文件（`go:embed`，随二进制走，不依赖外部文件）。
//
// 用 `file://` 打开而不是 `data:`：无痕验证要读到正常的文档来源与环境信号，
// 不透明来源（`data:` 的 origin 是 `null`）会让 SDK 侧的风控信号自相矛盾。
//
//go:embed solver-page.html
var solverPage []byte

// writeSolverPage 把求解页落到目录里，返回它的 `file://` URL。
//
// 落在 temp 下而不是 exe 旁边：这是每次求解用完即弃的中间产物，
// 不该出现在部署目录里（A6 的「不自带外部文件」要求）。
func writeSolverPage(dir string) (string, error) {
	path := filepath.Join(dir, "solver-page.html")
	if err := os.WriteFile(path, solverPage, 0o644); err != nil {
		return "", fmt.Errorf("写入求解页失败: %w", err)
	}
	return fileURL(path), nil
}

// fileURL 把本地绝对路径转成 file:// URL。
//
// 手写而不用 `url.URL`：Windows 盘符路径（`D:\a\b.html`）用 `url.URL{Scheme:"file",Path:...}`
// 会得到 `file://D:/a/b.html`（三斜杠规则对盘符的处理很别扭），而 Chrome 认
// `file:///D:/a/b.html`。这里按 POSIX 分隔符拼，行为在各平台一致。
func fileURL(path string) string {
	p := filepath.ToSlash(path)
	if len(p) > 0 && p[0] != '/' {
		p = "/" + p // Windows 盘符：D:/x → /D:/x
	}
	return "file://" + p
}
