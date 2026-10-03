package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LinYuanNull/zcode2api-go/internal/agent"
	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/models"
	"github.com/LinYuanNull/zcode2api-go/internal/store"
)

// 本文件走**完整 HTTP 链路**（httptest 起 Gateway + 假上游），
// 与 scheduler_test.go 的单元级判据互补。判据来源：
//   - docs/contract/gateway/*.json（错误分支的逐字样本）
//   - docs/contract/outbound/behavior.md §一/§3.0（>>> 行与响应头规则）

// recorder 收记号行。
type recorder struct{ lines []string }

func (r *recorder) MarkLine(line string) { r.lines = append(r.lines, line) }
func (r *recorder) joined() string       { return strings.Join(r.lines, "\n") }

// fixture 一次搭好：真 store + 2 账号 + 假上游 + 网关（挂真实 HTTP 服务）。
type fixture struct {
	srv     *httptest.Server // 网关入口
	up      *httptest.Server // 假上游
	upCount int32            // 上游收到的请求数（由 handler 维护）
	rec     *recorder
	st      *store.Store
}

func newFixture(t *testing.T, handler http.HandlerFunc) *fixture {
	t.Helper()
	f := &fixture{rec: &recorder{}}

	f.up = httptest.NewServer(handler)
	t.Cleanup(f.up.Close)

	var err error
	f.st, err = store.Open(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.st.Close() })
	for i := 0; i < 2; i++ {
		tok := fmt.Sprintf("tok-%02d", i)
		_ = f.st.Put(models.Account{
			ID: fmt.Sprintf("h-%02d", i), Name: fmt.Sprintf("h-%02d", i),
			Provider: "zai", Mode: constants.ModeAPIKey, APIKey: &tok,
			Enabled: true, Status: constants.StatusActive, CreatedAt: float64(i),
		})
	}

	ag := agent.New()
	ag.SetMessagesURLForTest(f.up.URL)
	gw := New(Options{Store: f.st, GatewayKey: func() string { return "" }, Marks: f.rec, Agent: ag})
	f.srv = httptest.NewServer(gw)
	t.Cleanup(f.srv.Close)
	return f
}

const reqBody = `{"model":"GLM-5.3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

// TestPassthroughSuccess 覆盖成功路径：体逐字节、Content-Type 照搬 + charset、
// cache-control 总加、其它上游头不透传。
func TestPassthroughSuccess(t *testing.T) {
	upBody := `{"id": "msg_h", "type": "message", "role": "assistant", "model": "GLM-5.3", "content": [{"type": "text", "text": "hello"}], "stop_reason": "end_turn", "stop_sequence": null, "usage": {"input_tokens": 8, "output_tokens": 3}}`
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		// 出站体必须经过 bodytransform（字符串 content → 数组）且保持键序。
		got := make([]byte, r.ContentLength)
		_, _ = readFull(r, got)
		want := `{"model": "GLM-5.3", "max_tokens": 16, "messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}]}`
		if string(got) != want {
			t.Errorf("出站体不符：\n got  %s\n want %s", got, want)
		}
		// 出站鉴权头是 X-Api-Key（不是 Authorization）。
		if got := r.Header.Get("X-Api-Key"); got != "tok-00" {
			t.Errorf("X-Api-Key = %q, 想要 tok-00", got)
		}
		if r.Header.Get("User-Agent") != "ZCode/3.14.4" {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Custom-Probe", "abc") // 探针 h-*：入站**没有**这个头
		w.WriteHeader(200)
		fmt.Fprint(w, upBody)
	})
	resp, err := http.Post(f.srv.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if v := resp.Header.Get("Content-Type"); v != "application/json" {
		t.Errorf("Content-Type = %q", v)
	}
	if v := resp.Header.Get("Cache-Control"); v != "no-cache" {
		t.Errorf("Cache-Control = %q, 想要 no-cache（总是加）", v)
	}
	if v := resp.Header.Get("X-Custom-Probe"); v != "" {
		t.Errorf("X-Custom-Probe = %q, 想要空（上游头不透传）", v)
	}
	buf := make([]byte, len(upBody))
	if _, err := ioReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != upBody {
		t.Errorf("响应体不是逐字节透传：\n got  %s\n want %s", buf, upBody)
	}
	// 成功路径只有一条 >>> 记号，无 [~]/<!>
	if len(f.rec.lines) != 1 || !strings.HasPrefix(f.rec.lines[0], ">>>") {
		t.Errorf("记号应只有一条 >>>：\n%s", f.rec.joined())
	}
}

// TestContentTypeCharsetAppended 上游 text/* ⇒ 追加 charset（探针 h-*）。
func TestContentTypeCharsetAppended(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/x-probe")
		w.WriteHeader(200)
		fmt.Fprint(w, "hi")
	})
	resp, err := http.Post(f.srv.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if v := resp.Header.Get("Content-Type"); v != "text/x-probe; charset=utf-8" {
		t.Errorf("Content-Type = %q, 想要 text/x-probe; charset=utf-8", v)
	}
}

// TestRouteLogLine `>>>` 行的三项取值（behavior.md §3.0）。
func TestRouteLogLine(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"基本形态", reqBody, `GLM-5.3  sync  "hi"`},
		{"stream true", `{"model":"M","stream":true,"messages":[{"role":"user","content":"hi"}]}`, `M  stream  "hi"`},
		{"缺 model 印 -", `{"messages":[{"role":"user","content":"hi"}]}`, `-  sync  "hi"`},
		{"model 空串印 -", `{"model":"","messages":[{"role":"user","content":"hi"}]}`, `-  sync  "hi"`},
		{"末条 user 预览", `{"model":"M","messages":[{"role":"user","content":"A"},{"role":"assistant","content":"b"},{"role":"user","content":"again"}]}`, `M  sync  "again"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{}`)
			})
			resp, _ := http.Post(f.srv.URL+"/v1/messages", "application/json", strings.NewReader(c.body))
			resp.Body.Close()
			var line string
			for _, l := range f.rec.lines {
				if strings.HasPrefix(strings.TrimLeft(l, "\t "), ">>>") {
					line = strings.TrimLeft(l, "\t ")
					break
				}
			}
			if line == "" {
				t.Fatalf("没有 >>> 行:\n%s", f.rec.joined())
			}
			// 记号行去掉 reqid（16 hex，随机）再比。
			// 形态：`>>> <id>  <三项...>`（reqid 后是两个空格）。
			const sep = "  "
			i := strings.Index(line, sep)
			if i < 0 {
				t.Fatalf("记号行格式不对: %q", line)
			}
			id := line[len(">>>")+1 : i]
			if len(id) != 16 {
				t.Errorf("reqid = %q, 想要 16 位", id)
			}
			if got := line[i+2:]; got != c.want {
				t.Errorf("三项 = %q, 想要 %q", got, c.want)
			}
		})
	}
}

// TestErrorBranches 网关域错误分支（逐字样本）。
func TestErrorBranches(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		body      string
		wantCode  int
		wantMsg   string
		wantType  string
		wantRoute bool // 是否应有 >>> 行（错误分支都在它之前）
	}{
		{"非法 JSON", "/v1/messages", `not json`, 400, "请求体不是合法 JSON", "invalid_request", false},
		{"chat 非法 JSON type 不同", "/v1/chat/completions", `not json`, 400, "请求体不是合法 JSON", "invalid_request_error", false},
		{"非对象根", "/v1/messages", `123`, 400, "请求体必须是 JSON 对象", "invalid_request_error", false},
		{"非对象根数组", "/v1/messages", `[]`, 400, "请求体必须是 JSON 对象", "invalid_request_error", false},
		{"model 非字符串（有意偏离）", "/v1/messages", `{"model":123,"messages":[]}`, 400, "model 必须是字符串", "invalid_request_error", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("不该有出站") })
			resp, err := http.Post(f.srv.URL+c.path, "application/json", strings.NewReader(c.body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != c.wantCode {
				t.Fatalf("status = %d, 想要 %d", resp.StatusCode, c.wantCode)
			}
			var got struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got.Error.Message != c.wantMsg || got.Error.Type != c.wantType {
				t.Errorf("error = %s/%s, 想要 %s/%s", got.Error.Message, got.Error.Type, c.wantMsg, c.wantType)
			}
			hasRoute := false
			for _, l := range f.rec.lines {
				if strings.HasPrefix(l, ">>>") {
					hasRoute = true
				}
			}
			if hasRoute != c.wantRoute {
				t.Errorf(">>> 行存在 = %v, 想要 %v:\n%s", hasRoute, c.wantRoute, f.rec.joined())
			}
		})
	}
}

// TestNoAccount503 池空 ⇒ 503 no_available_account（但 >>> 行**要打**）。
//
// 与上面 400 分支的区别：池空发生在路由之后（>>> 已打），这是 behavior.md
// §3.0 的「每次请求都有一条 >>>，与最终结果无关」。
func TestNoAccount503(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("不该有出站") })
	// 清空账号池
	for _, a := range f.st.List() {
		_, _ = f.st.Delete(a.ID)
	}
	resp, err := http.Post(f.srv.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("status = %d, 想要 503", resp.StatusCode)
	}
	var got struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.Error.Type != "no_available_account" {
		t.Errorf("type = %q", got.Error.Type)
	}
	found := false
	for _, l := range f.rec.lines {
		if strings.HasPrefix(l, ">>>") {
			found = true
		}
	}
	if !found {
		t.Errorf("池空也必须有 >>> 行:\n%s", f.rec.joined())
	}
}

// TestClientErrorPassthrough 客户端错（上游 400）原样透传 + <!> 行。
func TestClientErrorPassthrough(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"message":"bad model","type":"400"}}`)
	})
	resp, err := http.Post(f.srv.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, 想要 400（客户端错透传）", resp.StatusCode)
	}
	// harness 实测（upstream-400）：客户端错响应**没有** cache-control，
	// 靶机的 no-cache 只加在正常响应路径上。
	if v := resp.Header.Get("Cache-Control"); v != "" {
		t.Errorf("Cache-Control = %q, 想要空（客户端错分支不加）", v)
	}
	buf := make([]byte, 64)
	n, _ := ioReadFull(resp.Body, buf)
	if !strings.Contains(string(buf[:n]), "bad model") {
		t.Errorf("上游错误体应原样透传，got: %s", buf[:n])
	}
	joined := f.rec.joined()
	if !strings.Contains(joined, "上游 400 完整响应体:") {
		t.Errorf("缺 [~] 完整响应体行:\n%s", joined)
	}
	if !strings.Contains(joined, "上游错误 HTTP 400") {
		t.Errorf("缺 <!> 行:\n%s", joined)
	}
	// 顺序实测（harness upstream-400 对照）：先 <!> 后 [~] 完整响应体。
	failIdx := strings.Index(joined, "上游错误 HTTP 400")
	noticeIdx := strings.Index(joined, "上游 400 完整响应体:")
	if failIdx < 0 || noticeIdx < 0 || failIdx > noticeIdx {
		t.Errorf("记号顺序应是先 <!> 后 [~]:\n%s", joined)
	}
}

// TestGatewayAuth 鉴权三分支（样本逐字文案）。
func TestGatewayAuth(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	// 给网关加 key：重建一个带 key 的实例（Agent 也要注入，否则打到真上游）
	ag2 := agent.New()
	ag2.SetMessagesURLForTest(f.up.URL)
	gw := New(Options{Store: f.st, GatewayKey: func() string { return "sk-test" }, Marks: f.rec, Agent: ag2})
	srv := httptest.NewServer(gw)
	defer srv.Close()

	// 缺凭证 401
	resp, _ := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if resp.StatusCode != 401 {
		t.Errorf("缺凭证 status = %d, 想要 401", resp.StatusCode)
	}
	resp.Body.Close()
	// 凭证错 403
	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer wrong")
	resp2, _ := http.DefaultClient.Do(req)
	if resp2.StatusCode != 403 {
		t.Errorf("凭证错 status = %d, 想要 403", resp2.StatusCode)
	}
	resp2.Body.Close()
	// 正确凭证 200
	req3, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(reqBody))
	req3.Header.Set("Authorization", "Bearer sk-test")
	resp3, _ := http.DefaultClient.Do(req3)
	if resp3.StatusCode != 200 {
		t.Errorf("正确凭证 status = %d, 想要 200", resp3.StatusCode)
	}
	resp3.Body.Close()
}

// ---- 小工具 ----

func readFull(r *http.Request, buf []byte) (int, error) {
	return ioReadFull(r.Body, buf)
}

func ioReadFull(rd interface{ Read([]byte) (int, error) }, buf []byte) (int, error) {
	// io.ReadFull 的语义：读满或返回错误（EOF 也算，只要读满了就没错）。
	// 这里用标准库，不要手写 —— 手写版在「最后一次 Read 返回 (n, io.EOF)」
	// 时会把读满误判成错误。
	return io.ReadFull(rd, buf)
}
