package appdir_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/LinYuanNull/zcode2api-go/internal/appdir"
)

// exeDirOfTest 复刻 appdir 的「exe 同级」基准，用来断言相对路径的落点。
func exeDirOfTest(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(exe)
}

// 优先级：--data-dir > ZCODE_DATA_DIR > <exe>/data > <cwd>/data。
func TestDataDirPrecedence(t *testing.T) {
	flagDir := t.TempDir()
	envDir := t.TempDir()

	t.Setenv("ZCODE_DATA_DIR", envDir)
	got, err := appdir.DataDir(flagDir)
	if err != nil || got != filepath.Clean(flagDir) {
		t.Fatalf("flag 应压过 env：got=%q err=%v", got, err)
	}

	got, err = appdir.DataDir("")
	if err != nil || got != filepath.Clean(envDir) {
		t.Fatalf("flag 为空时应取 env：got=%q err=%v", got, err)
	}
}

// 相对路径按 **exe 所在目录**解析（单文件 exe 的「项目根」就是它自己的目录），
// 不是按当前工作目录 —— 双击启动时 cwd 可能是任意地方。
func TestDataDirRelativeResolvesAgainstExe(t *testing.T) {
	t.Setenv("ZCODE_DATA_DIR", "")
	got, err := appdir.DataDir("rel-data")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(exeDirOfTest(t), "rel-data")
	if got != want {
		t.Fatalf("相对路径落点不符：got=%q want=%q", got, want)
	}
}

// 都不配置时要能自己找一处可写目录（并在那里**真的建出**目录）。
func TestDataDirFallsBackToWritableCandidate(t *testing.T) {
	t.Setenv("ZCODE_DATA_DIR", "")
	got, err := appdir.DataDir("")
	if err != nil {
		t.Skipf("当前环境两个候选都不可写，跳过：%v", err)
	}
	if st, err := os.Stat(got); err != nil || !st.IsDir() {
		t.Fatalf("回落目录应被实际创建：%q err=%v", got, err)
	}
	// 清掉本次建出来的候选（<exe>/data 或 <cwd>/data），避免污染后续用例。
	_ = os.Remove(got)
}

// PanelDir 优先级：--panel-dir > ZCODE_PANEL_DIR > ZCODE_FRONTEND_DIR > <exe>/frontend > 空串。
func TestPanelDirPrecedence(t *testing.T) {
	flagDir := t.TempDir()
	envDir := t.TempDir()
	frontDir := t.TempDir()

	t.Setenv("ZCODE_PANEL_DIR", envDir)
	t.Setenv("ZCODE_FRONTEND_DIR", frontDir)

	if got := appdir.PanelDir(flagDir); got != filepath.Clean(flagDir) {
		t.Fatalf("flag 应最优先：%q", got)
	}
	if got := appdir.PanelDir(""); got != filepath.Clean(envDir) {
		t.Fatalf("其次 ZCODE_PANEL_DIR：%q", got)
	}
	t.Setenv("ZCODE_PANEL_DIR", "")
	if got := appdir.PanelDir(""); got != filepath.Clean(frontDir) {
		t.Fatalf("再次 ZCODE_FRONTEND_DIR（上游同名变量）：%q", got)
	}
}

// `<exe>/frontend` 只在**确实存在**时才被采用：否则会把「没配面板」误判成
// 「配了一个不存在的目录」，页面 404 但管理 API 一切正常，最难排查。
func TestPanelDirExeLocalOnlyWhenPresent(t *testing.T) {
	t.Setenv("ZCODE_PANEL_DIR", "")
	t.Setenv("ZCODE_FRONTEND_DIR", "")

	local := filepath.Join(exeDirOfTest(t), "frontend")
	if _, err := os.Stat(local); err == nil {
		t.Skipf("测试环境里已存在 %q，跳过（避免误删真实资源）", local)
	}
	if got := appdir.PanelDir(""); got != "" {
		t.Fatalf("不存在时 should be 空串：%q", got)
	}

	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Skipf("无法在 exe 同级建目录，跳过：%v", err)
	}
	defer func() { _ = os.Remove(local) }()
	if got := appdir.PanelDir(""); got != local {
		t.Fatalf("存在时应采用：got=%q want=%q", got, local)
	}
}
