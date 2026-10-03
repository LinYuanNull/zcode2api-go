// Command zcode2api-go 是 ZCode 网关的独立纯 Go 实现。
//
// 子命令：
//
//	serve          启动网关与管理面板（默认，等价于不带子命令）
//	login          账号登录（OAuth 设备码流程）
//	claim          立即执行一次套餐领取
//	set-admin-key  设置管理面板密码
//
// 当前处于 A0（立项与骨架）阶段：各子命令只做参数解析与提示，实际逻辑按
// A1..A6 分期落地。分期方案见 ModelMux 仓库的 docs/zcode-native-port-plan.md。
package main

import (
	"fmt"
	"os"
)

const usage = `zcode2api-go —— ZCode 网关（独立纯 Go 实现）

用法:
  zcode2api-go [子命令]

子命令:
  serve           启动网关与管理面板（默认）
  login           账号登录（OAuth 设备码流程）
  claim           立即执行一次套餐领取
  set-admin-key   设置管理面板密码
  help            显示本帮助

不带子命令时等价于 serve。
`

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "login":
		err = runLogin(args)
	case "claim":
		err = runClaim(args)
	case "set-admin-key":
		err = runSetAdminKey(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n\n%s", cmd, usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "zcode2api-go: %v\n", err)
		os.Exit(1)
	}
}

// notImplemented 统一骨架期的「尚未实现」提示，避免各子命令各写一套文案。
func notImplemented(what string) error {
	return fmt.Errorf("%s 尚未实现（当前为 A0 骨架，路线图见 README）", what)
}

func runServe(_ []string) error       { return notImplemented("serve") }
func runLogin(_ []string) error       { return notImplemented("login") }
func runClaim(_ []string) error       { return notImplemented("claim") }
func runSetAdminKey(_ []string) error { return notImplemented("set-admin-key") }
