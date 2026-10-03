// Command samplefixture 从靶机抓一份 A2 落盘契约夹具（`docs/contract/store/fixtures/`）。
//
// 为什么要「夹具」而不是只写文档：A2 的验收是「能读现有 accounts.db 并产出与 Python 侧
// **逐字段一致**的账号快照」。这句话只有在**同一份库 + 同一份期望输出**被固化下来之后
// 才可复现 —— 否则每次验收都要现场起靶机、结果还随机会数漂移。夹具把这件事变成 CI 里
// 一条可重复执行的断言。
//
// 产物（`-out` 目录）：
//
//	target.db            靶机自己写出的 accounts.db（原样复制）
//	target-accounts.json 靶机 GET /admin/api/accounts 的**原始响应体**（保留键顺序）
//	target-settings.json 靶机 GET /admin/api/settings 的原始响应体
//	target-export.json   靶机 GET /admin/api/export 的原始响应体
//	target-status.json   靶机 GET /admin/api/status 的原始响应体
//
// 用法：
//
//	go run ./tools/samplefixture -zcode D:/AiWork/ZCode/zcode2api -out docs/contract/store/fixtures
//
// 靶机跑在**独立临时数据目录**里，绝不触碰真实 data/accounts.db。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const adminKey = "1234"

func main() {
	zcode := flag.String("zcode", `D:\AiWork\ZCode\zcode2api`, "靶机（上游 Python 项目）目录")
	out := flag.String("out", `docs/contract/store/fixtures`, "夹具输出目录")
	port := flag.Int("port", 13011, "靶机监听端口")
	flag.Parse()

	dataDir, err := os.MkdirTemp("", "zcode_fixture_")
	must(err)
	defer os.RemoveAll(dataDir)
	fmt.Println("临时数据目录:", dataDir)

	py := filepath.Join(*zcode, ".venv", "Scripts", "python.exe")
	if _, err := os.Stat(py); err != nil {
		fatal("找不到靶机解释器 %s: %v", py, err)
	}

	cmd := exec.Command(py, "cli.py", "serve")
	cmd.Dir = *zcode
	cmd.Env = append(os.Environ(),
		"ZCODE_DATA_DIR="+dataDir,
		"ZCODE_ADMIN_KEY="+adminKey,
		fmt.Sprintf("ZCODE_PORT=%d", *port),
	)
	logPath := filepath.Join(os.TempDir(), "zcode_fixture_server.log")
	logFile, err := os.Create(logPath)
	must(err)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	must(cmd.Start())
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		logFile.Close()
	}()

	base := fmt.Sprintf("http://127.0.0.1:%d", *port)
	if err := waitReady(base); err != nil {
		raw, _ := os.ReadFile(logPath)
		fatal("靶机未就绪: %v\n--- 靶机日志尾部 ---\n%s", err, tail(string(raw), 3000))
	}
	fmt.Println("靶机就绪:", base)

	// ── 造数据：一组覆盖 slug / 默认名 / 去重 / 跨 provider 的确定性样本
	type addReq struct {
		Provider string   `json:"provider"`
		Tokens   []string `json:"tokens"`
		Name     string   `json:"name,omitempty"`
	}
	ids := map[string]string{} // 便于后续对指定账号做写操作
	adds := []struct {
		label string
		body  addReq
	}{
		{"alpha", addReq{"zai", []string{"fixture-token-0001"}, "Fixture Alpha"}},
		{"upper", addReq{"zai", []string{"fixture-token-0002"}, "UPPER Case!"}},
		{"cjk", addReq{"zai", []string{"fixture-token-0003"}, "账号测试"}},
		{"defaultname", addReq{"zai", []string{"fixture-token-0004"}, ""}},
		{"bm", addReq{"bigmodel", []string{"fixture-token-0005"}, "bm-one"}},
		{"dupe", addReq{"zai", []string{"fixture-token-0001"}, "dup-attempt"}},
		{"crossprovider", addReq{"bigmodel", []string{"fixture-token-0001"}, "cross-provider"}},
	}
	for _, a := range adds {
		var res struct {
			Count int      `json:"count"`
			IDs   []string `json:"ids"`
		}
		status, err := post(base, "/admin/api/accounts", a.body, &res)
		must(err)
		if status != 200 {
			fatal("新增 %s 失败: status=%d", a.label, status)
		}
		if len(res.IDs) > 0 {
			ids[a.label] = res.IDs[0]
		}
		fmt.Printf("  + %-14s -> %v\n", a.label, res.IDs)
	}

	// ── 写操作：改 name、禁用、换发指纹
	_, err = request(http.MethodPut, base+"/admin/api/accounts/"+ids["alpha"],
		json.RawMessage(`{"name":"fixture-renamed"}`), nil)
	must(err)
	_, err = request(http.MethodPost, base+"/admin/api/accounts/"+ids["bm"]+"/enabled",
		json.RawMessage(`{"enabled":false}`), nil)
	must(err)
	_, err = request(http.MethodPost, base+"/admin/api/accounts/"+ids["cjk"]+"/fingerprint/rotate",
		nil, nil)
	must(err)
	_, err = request(http.MethodPut, base+"/admin/api/settings",
		json.RawMessage(`{"quota_refresh_interval":900,"account_concurrency":3,"claim_round_interval":5,"gateway_key":"fixture-gw-key-abcdef"}`), nil)
	must(err)

	// ── 抓原始响应体（保留键顺序）
	must(os.MkdirAll(*out, 0o755))
	for _, ep := range []struct{ name, path string }{
		{"target-accounts.json", "/admin/api/accounts"},
		{"target-settings.json", "/admin/api/settings"},
		{"target-export.json", "/admin/api/export"},
		{"target-status.json", "/admin/api/status"},
	} {
		body, err := rawGet(base + ep.path)
		must(err)
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, body, "", "  "); err != nil {
			pretty.Write(body)
		}
		pretty.WriteByte('\n')
		must(os.WriteFile(filepath.Join(*out, ep.name), pretty.Bytes(), 0o644))
		fmt.Printf("  -> %s (%d B)\n", ep.name, pretty.Len())
	}

	// ── 复制靶机写出的库
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	time.Sleep(200 * time.Millisecond)

	src := filepath.Join(dataDir, "accounts.db")
	dbBytes, err := os.ReadFile(src)
	must(err)
	must(os.WriteFile(filepath.Join(*out, "target.db"), dbBytes, 0o644))
	fmt.Printf("  -> target.db (%d B)\n", len(dbBytes))
	fmt.Println("完成。")
}

func waitReady(base string) error {
	deadline := time.Now().Add(60 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		_, err := rawGet(base + "/meta")
		if err == nil {
			return nil
		}
		last = err
		time.Sleep(time.Second)
	}
	return last
}

func rawGet(url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+adminKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s -> %d", url, resp.StatusCode)
	}
	return body, nil
}

func post(base, path string, body any, out any) (int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	return request(http.MethodPost, base+path, raw, out)
}

func request(method, url string, body json.RawMessage, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+adminKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, err
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s 响应不是预期 JSON: %v (原始: %s)", method, url, err, tail(string(raw), 300))
		}
	}
	return resp.StatusCode, nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func must(err error) {
	if err != nil {
		fatal("%v", err)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "samplefixture: "+format+"\n", args...)
	os.Exit(1)
}
