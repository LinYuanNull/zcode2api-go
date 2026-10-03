// Package appdir 解析运行期目录（数据目录、面板目录）。
//
// 为什么要单独一个包：`go:embed` 之外的**所有**运行期路径都取决于「进程从哪里
// 启动」，而这两件事最容易出岔子 ——
//
//   - 数据目录搞错 ⇒ 用户打开的是一个空账号池，看起来像「账号全丢了」（风险登记 #6）；
//   - 面板目录搞错 ⇒ 面板 404，但管理 API 一切正常，最难排查的一类现象。
//
// 所以两者的解析规则都写在这里，并且**只在这里**，附上判据。
package appdir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// DataDir 解析数据目录（其下是 `accounts.db`）。
//
// 取值顺序：
//
//  1. `flagValue`（`--data-dir`）
//  2. 环境变量 `ZCODE_DATA_DIR`（与上游同名，便于复用同一份 `.env`）
//  3. `<exe 同级>/data`
//  4. `<当前工作目录>/data`
//
// 相对路径按 **exe 所在目录**解析（上游是「相对项目根」；对单文件 exe 而言，
// exe 所在目录就是它的「项目根」）。第 3、4 步都要**实际建目录成功**才采用 ——
// 装在 `C:\Program Files` 下的 exe 会写不进去，必须能落到别处。
func DataDir(flagValue string) (string, error) {
	if v := strings.TrimSpace(flagValue); v != "" {
		return resolveAgainstExe(v), nil
	}
	if v := strings.TrimSpace(os.Getenv("ZCODE_DATA_DIR")); v != "" {
		return resolveAgainstExe(v), nil
	}
	candidates := []string{
		filepath.Join(exeDir(), "data"),
		filepath.Join(cwd(), "data"),
	}
	for _, c := range candidates {
		if err := os.MkdirAll(c, 0o755); err == nil {
			return c, nil
		}
	}
	return "", errors.New("无法确定数据目录：exe 同级与当前目录都不可写，请用 --data-dir 或 ZCODE_DATA_DIR 指定")
}

// PanelDir 解析管理面板静态资源目录。
//
// 取值顺序：
//
//  1. `flagValue`（`--panel-dir`）
//  2. 环境变量 `ZCODE_PANEL_DIR`
//  3. 环境变量 `ZCODE_FRONTEND_DIR` —— **上游的同名变量**，这样已经配好上游
//     `.env` 的用户不用改任何东西
//  4. `<exe 同级>/frontend`（存在才采用）
//  5. 空串 —— 表示未配置，面板页面走占位说明页（管理 API 不受影响）
//
// **上游前端不进本仓库**（许可纪律），所以这里只做「指路」，不内置任何资源。
func PanelDir(flagValue string) string {
	for _, v := range []string{flagValue, os.Getenv("ZCODE_PANEL_DIR"), os.Getenv("ZCODE_FRONTEND_DIR")} {
		if v = strings.TrimSpace(v); v != "" {
			return resolveAgainstExe(v)
		}
	}
	local := filepath.Join(exeDir(), "frontend")
	if st, err := os.Stat(local); err == nil && st.IsDir() {
		return local
	}
	return ""
}

// ── 内部 ────────────────────────────────────────────────────

// resolveAgainstExe 把相对路径解析到 exe 所在目录之下；绝对路径原样返回。
func resolveAgainstExe(p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(exeDir(), p)
}

func exeDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return cwd()
}

func cwd() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}
