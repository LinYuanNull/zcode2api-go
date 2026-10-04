package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// dialFake 连到假 CDP 服务。
func dialFake(t *testing.T, f *fakeServer) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, f.wsURL())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func callCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func TestHandshakeAndRoundTrip(t *testing.T) {
	f := newFakeServer(t)
	f.setHandler(func(method string, _ json.RawMessage, _ string) (fakeReply, bool) {
		if method == "Ping.me" {
			return fakeReply{Result: map[string]any{"pong": "hi"}}, true
		}
		return fakeReply{}, false
	})
	c := dialFake(t, f)

	ctx, cancel := callCtx(t)
	defer cancel()
	var res struct {
		Pong string `json:"pong"`
	}
	if err := c.Call(ctx, "", "Ping.me", nil, &res); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Pong != "hi" {
		t.Errorf("pong = %q，期望 hi", res.Pong)
	}
	if got := f.methods(); len(got) != 1 || got[0] != "Ping.me" {
		t.Errorf("服务端收到 %v", got)
	}
}

func TestCallCarriesSessionIDAndParams(t *testing.T) {
	f := newFakeServer(t)
	f.setHandler(func(method string, params json.RawMessage, _ string) (fakeReply, bool) {
		return fakeReply{Result: map[string]any{"ok": method}}, true
	})
	c := dialFake(t, f)

	ctx, cancel := callCtx(t)
	defer cancel()
	if err := c.Call(ctx, "S-1", "Some.method", map[string]any{"a": 1}, nil); err != nil {
		t.Fatalf("Call: %v", err)
	}

	calls := f.Calls()
	if len(calls) != 1 {
		t.Fatalf("收到 %d 条命令", len(calls))
	}
	if calls[0].SessionID != "S-1" {
		t.Errorf("sessionId = %q，期望 S-1", calls[0].SessionID)
	}
	var p map[string]int
	if err := json.Unmarshal(calls[0].Params, &p); err != nil {
		t.Fatalf("params 解析失败: %v", err)
	}
	if p["a"] != 1 {
		t.Errorf("params = %v", p)
	}
}

func TestProtocolErrorIsReturnedVerbatim(t *testing.T) {
	f := newFakeServer(t)
	f.setHandler(func(string, json.RawMessage, string) (fakeReply, bool) {
		return fakeReply{Err: &ProtocolError{Code: -32601, Message: "'X' wasn't found"}}, true
	})
	c := dialFake(t, f)

	ctx, cancel := callCtx(t)
	defer cancel()
	err := c.Call(ctx, "", "X", nil, nil)
	if err == nil {
		t.Fatal("期望报错，实际成功")
	}
	var pe *ProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("错误类型 %T，期望 *ProtocolError", err)
	}
	if pe.Code != -32601 || pe.Message != "'X' wasn't found" {
		t.Errorf("错误 = %+v", pe)
	}
}

func TestFragmentedReplyIsReassembled(t *testing.T) {
	f := newFakeServer(t)
	blob := strings.Repeat("分片载荷", 200) // 足够跨多个分片
	f.setHandler(func(string, json.RawMessage, string) (fakeReply, bool) {
		return fakeReply{Result: map[string]any{"blob": blob}}, true
	})
	f.fragSize = 64
	c := dialFake(t, f)

	ctx, cancel := callCtx(t)
	defer cancel()
	var res struct {
		Blob string `json:"blob"`
	}
	if err := c.Call(ctx, "", "Big", nil, &res); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Blob != blob {
		t.Errorf("分片重组结果不符：got %d 字节，want %d", len(res.Blob), len(blob))
	}
}

func TestRepliesMatchByIDNotArrivalOrder(t *testing.T) {
	f := newFakeServer(t)
	f.setHandler(func(method string, params json.RawMessage, _ string) (fakeReply, bool) {
		var p struct {
			N int `json:"n"`
		}
		_ = json.Unmarshal(params, &p)
		return fakeReply{Result: map[string]any{"n": p.N}}, true
	})
	f.reorder2 = true
	c := dialFake(t, f)

	// 两条**并发**命令；服务端会把响应倒序发出。按到达顺序配对的实现会串号。
	var wg sync.WaitGroup
	results := make([]int, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var res struct {
				N int `json:"n"`
			}
			if err := c.Call(ctx, "", "Echo", map[string]any{"n": i + 1}, &res); err != nil {
				errs[i] = err
				return
			}
			results[i] = res.N
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 条调用失败: %v", i, err)
		}
	}
	if results[0] != 1 || results[1] != 2 {
		t.Errorf("并发调用串号：results = %v，期望 [1 2]", results)
	}
}

func TestPingFrameIsAnsweredWithPong(t *testing.T) {
	f := newFakeServer(t)
	f.pingOnce = true
	f.setHandler(func(string, json.RawMessage, string) (fakeReply, bool) {
		return fakeReply{Result: map[string]any{"ok": true}}, true
	})
	c := dialFake(t, f)

	ctx, cancel := callCtx(t)
	defer cancel()
	if err := c.Call(ctx, "", "A", nil, nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	// 再发一条：若客户端没回 Pong，服务端循环仍能继续（它只是计数），
	// 所以这里用轮询等 Pong 计数上来。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && f.Pongs() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if f.Pongs() == 0 {
		t.Error("收到 Ping 但没有回 Pong")
	}
}

func TestCallTimesOutWhenServerStaysSilent(t *testing.T) {
	f := newFakeServer(t)
	f.silent = true
	c := dialFake(t, f)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := c.Call(ctx, "", "Never", nil, nil)
	if err == nil {
		t.Fatal("期望超时，实际成功")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("错误 = %v，期望包含 context.DeadlineExceeded", err)
	}
}

func TestCloseUnblocksPendingCall(t *testing.T) {
	f := newFakeServer(t)
	f.silent = true
	c := dialFake(t, f)

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- c.Call(ctx, "", "Never", nil, nil)
	}()

	time.Sleep(50 * time.Millisecond)
	c.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("连接关闭后调用应报错")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close 没有唤醒等待中的调用")
	}
}

func TestBrowserWSURL(t *testing.T) {
	f := newFakeServer(t)
	ctx, cancel := callCtx(t)
	defer cancel()

	got, err := BrowserWSURL(ctx, f.httpBase())
	if err != nil {
		t.Fatalf("BrowserWSURL: %v", err)
	}
	if got != f.wsURL() {
		t.Errorf("ws url = %q，期望 %q", got, f.wsURL())
	}
}

func TestBrowserWSURLRejectsNon200(t *testing.T) {
	srv := newHTTPStub(t, http.StatusForbidden, `{}`)
	ctx, cancel := callCtx(t)
	defer cancel()
	_, err := BrowserWSURL(ctx, srv)
	if err == nil {
		t.Fatal("HTTP 403 应报错")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("错误 = %v，期望提到 403", err)
	}
}

func TestBrowserWSURLRejectsMissingField(t *testing.T) {
	srv := newHTTPStub(t, http.StatusOK, `{"Browser":"x"}`)
	ctx, cancel := callCtx(t)
	defer cancel()
	_, err := BrowserWSURL(ctx, srv)
	if err == nil || !strings.Contains(err.Error(), "webSocketDebuggerUrl") {
		t.Errorf("错误 = %v，期望提到缺少 webSocketDebuggerUrl", err)
	}
}

// TestTypedCommandsCoverTheWholeSolveFlow 把「求解只用 7 条命令」钉在代码里：
// 依次调用全部类型化封装，断言服务端按序收到这 7 个方法名与关键参数。
func TestTypedCommandsCoverTheWholeSolveFlow(t *testing.T) {
	f := newFakeServer(t)
	f.setHandler(func(method string, _ json.RawMessage, _ string) (fakeReply, bool) {
		switch method {
		case "Target.createTarget":
			return fakeReply{Result: map[string]any{"targetId": "T-1"}}, true
		case "Target.attachToTarget":
			return fakeReply{Result: map[string]any{"sessionId": "S-1"}}, true
		case "Page.navigate":
			return fakeReply{Result: map[string]any{"frameId": "F-1"}}, true
		case "Target.closeTarget":
			return fakeReply{Result: map[string]any{"success": true}}, true
		case "Runtime.evaluate":
			return fakeReply{Result: map[string]any{
				"result": map[string]any{"type": "string", "value": "PARAM"},
			}}, true
		default:
			return fakeReply{Result: map[string]any{}}, true
		}
	})
	c := dialFake(t, f)
	ctx, cancel := callCtx(t)
	defer cancel()

	target, err := c.CreateTarget(ctx, "")
	if err != nil || target != "T-1" {
		t.Fatalf("CreateTarget = %q, %v", target, err)
	}
	session, err := c.AttachToTarget(ctx, target)
	if err != nil || session != "S-1" {
		t.Fatalf("AttachToTarget = %q, %v", session, err)
	}
	if err := c.AddScriptOnNewDocument(ctx, session, "/* patch */"); err != nil {
		t.Fatalf("AddScriptOnNewDocument: %v", err)
	}
	if err := c.SetUserAgentOverride(ctx, session, UAOverride{UserAgent: "UA/1"}); err != nil {
		t.Fatalf("SetUserAgentOverride: %v", err)
	}
	if _, err := c.Navigate(ctx, session, "file:///tmp/p.html"); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	got, err := c.EvaluateString(ctx, session, "1+1", true)
	if err != nil {
		t.Fatalf("EvaluateString: %v", err)
	}
	if got != "PARAM" {
		t.Errorf("EvaluateString = %q，期望 PARAM", got)
	}
	if err := c.CloseTarget(ctx, target); err != nil {
		t.Fatalf("CloseTarget: %v", err)
	}

	want := []string{
		"Target.createTarget",
		"Target.attachToTarget",
		"Page.addScriptToEvaluateOnNewDocument",
		"Emulation.setUserAgentOverride",
		"Page.navigate",
		"Runtime.evaluate",
		"Target.closeTarget",
	}
	if got := f.methods(); !equalStrings(got, want) {
		t.Errorf("命令序列 = %v\n期望 = %v", got, want)
	}

	// sessionId 必须落在页面级命令上（浏览器级两条不带）。
	for _, c := range f.Calls() {
		pageLevel := strings.HasPrefix(c.Method, "Page.") || strings.HasPrefix(c.Method, "Emulation.") ||
			c.Method == "Runtime.evaluate"
		if pageLevel && c.SessionID != "S-1" {
			t.Errorf("%s 缺 sessionId（收到 %q）", c.Method, c.SessionID)
		}
		if !pageLevel && c.SessionID != "" {
			t.Errorf("%s 不应带 sessionId（收到 %q）", c.Method, c.SessionID)
		}
	}

	// 关键参数：attach 必须 flatten（否则命令无法随消息带 sessionId）。
	for _, c := range f.Calls() {
		if c.Method != "Target.attachToTarget" {
			continue
		}
		var p struct {
			TargetID string `json:"targetId"`
			Flatten  bool   `json:"flatten"`
		}
		if err := json.Unmarshal(c.Params, &p); err != nil {
			t.Fatalf("attach 参数解析失败: %v", err)
		}
		if !p.Flatten || p.TargetID != "T-1" {
			t.Errorf("attach 参数 = %+v，期望 flatten=true targetId=T-1", p)
		}
	}
}

// `Emulation.UserAgentMetadata` 里有**五项是必需的** —— 判据是浏览器自己吐的
// `/json/protocol`：`platform* platformVersion* architecture* model* mobile*`
// （带 `*` 的没有 optional 标记）。其余 `brands / fullVersionList / fullVersion /
// bitness / wow64 / formFactors` 才是可选项。
//
// 后果很坑：只要给这五项里的任何一个加上 `omitempty`，零值就会被序列化时省掉，
// 整条 `Emulation.setUserAgentOverride` 回 `-32602 Invalid parameters`，
// 而且**错误信息里不说是哪个字段缺了**。真机踩过一次 —— 罪魁是 `model`：
// 桌面 Chrome/Edge 的 model 本来就是空串，看着「没有值」，但必须显式发。
//
// 这条用例把「必需项一定要出现在 JSON 里」钉死，不依赖浏览器。
func TestUAOverrideAlwaysSendsRequiredMetadataFields(t *testing.T) {
	raw, err := json.Marshal(UAOverride{
		UserAgent: "UA/1",
		// 全零值：最容易触发 omitempty 误伤的情形。
		UserAgentMetadata: &UserAgentMetadata{},
	})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("反解失败: %v", err)
	}
	meta, ok := probe["userAgentMetadata"].(map[string]any)
	if !ok {
		t.Fatalf("userAgentMetadata 没被序列化出来: %s", raw)
	}
	for _, key := range []string{"platform", "platformVersion", "architecture", "model", "mobile"} {
		if _, ok := meta[key]; !ok {
			t.Errorf("必需字段 %q 被 omitempty 省掉了（会让整条命令 -32602）: %s", key, raw)
		}
	}

	// 字段名必须是协议里的 camelCase —— 写错了浏览器一样只回 -32602。
	multi, err := json.Marshal(UserAgentMetadata{
		Brands:          []UserAgentBrand{{Brand: "Chromium", Version: "127"}},
		FullVersionList: []UserAgentBrand{{Brand: "Chromium", Version: "127.0.6533.89"}},
		FullVersion:     "127.0.6533.89",
		PlatformVersion: "10.0.0",
	})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	for _, key := range []string{`"fullVersionList"`, `"fullVersion"`, `"platformVersion"`, `"brands"`} {
		if !strings.Contains(string(multi), key) {
			t.Errorf("缺少协议字段名 %s: %s", key, multi)
		}
	}

	// 可选字段为空时**允许**省掉（不必发 `bitness:""` 这种没意义的取值）。
	if strings.Contains(string(multi), `"bitness"`) || strings.Contains(string(multi), `"wow64"`) {
		t.Errorf("可选字段不该在空值时出现: %s", multi)
	}
}

func TestEvaluateStringRejectsNonString(t *testing.T) {
	f := newFakeServer(t)
	f.setHandler(func(string, json.RawMessage, string) (fakeReply, bool) {
		return fakeReply{Result: map[string]any{
			"result": map[string]any{"type": "number", "value": 42},
		}}, true
	})
	c := dialFake(t, f)
	ctx, cancel := callCtx(t)
	defer cancel()
	got, err := c.EvaluateString(ctx, "", "6*7", false)
	if err != nil {
		t.Fatalf("EvaluateString: %v", err)
	}
	if got != "" {
		t.Errorf("非字符串结果应回空串，得到 %q", got)
	}
}

func TestEvaluateStringSurfacesPageException(t *testing.T) {
	f := newFakeServer(t)
	f.setHandler(func(string, json.RawMessage, string) (fakeReply, bool) {
		return fakeReply{Result: map[string]any{
			"result": map[string]any{"type": "object", "subtype": "error"},
			"exceptionDetails": map[string]any{
				"text":      "Uncaught",
				"exception": map[string]any{"description": "TypeError: x is not a function"},
			},
		}}, true
	})
	c := dialFake(t, f)
	ctx, cancel := callCtx(t)
	defer cancel()
	_, err := c.EvaluateString(ctx, "", "boom()", false)
	if err == nil || !strings.Contains(err.Error(), "TypeError") {
		t.Errorf("错误 = %v，期望带上页面异常描述", err)
	}
}

// ------------------------------------------------------------ 帧层

func TestEncodeFrameRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 125, 126, 127, 200, 65535, 65536} {
		payload := make([]byte, n)
		for i := range payload {
			payload[i] = byte(i)
		}
		frame, err := encodeFrame(opText, payload)
		if err != nil {
			t.Fatalf("encodeFrame(%d): %v", n, err)
		}
		if frame[0] != 0x81 {
			t.Errorf("n=%d：首字节 = 0x%02X，期望 0x81", n, frame[0])
		}
		if frame[1]&0x80 == 0 {
			t.Errorf("n=%d：客户端帧必须带掩码位", n)
		}
		final, opcode, got, err := decodeFrameForTest(frame)
		if err != nil {
			t.Fatalf("n=%d：解帧失败 %v", n, err)
		}
		if !final || opcode != opText || string(got) != string(payload) {
			t.Errorf("n=%d：往返不符", n)
		}
	}
}

func TestEncodeFrameRejectsOversize(t *testing.T) {
	c := &wsConn{conn: nil}
	if err := c.WriteText(make([]byte, maxFramePayload+1)); err == nil {
		t.Error("超过上限的出站消息应报错")
	}
}

func TestDialWSRejectsBadScheme(t *testing.T) {
	ctx, cancel := callCtx(t)
	defer cancel()
	_, err := dialWS(ctx, "http://127.0.0.1:1/")
	if err == nil || !strings.Contains(err.Error(), "不支持的 WebSocket 协议") {
		t.Errorf("错误 = %v", err)
	}
}

func TestDialWSRejectsNon101(t *testing.T) {
	srv := newHTTPStub(t, http.StatusOK, `hello`)
	ctx, cancel := callCtx(t)
	defer cancel()
	_, err := dialWS(ctx, "ws"+strings.TrimPrefix(srv, "http"))
	if err == nil || !strings.Contains(err.Error(), "握手失败") {
		t.Errorf("错误 = %v，期望握手失败", err)
	}
}

// ------------------------------------------------------------ 测试辅助

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// decodeFrameForTest 解一个（必然带掩码的）客户端帧。
func decodeFrameForTest(frame []byte) (bool, byte, []byte, error) {
	if len(frame) < 6 {
		return false, 0, nil, fmt.Errorf("帧太短: %d", len(frame))
	}
	final := frame[0]&0x80 != 0
	opcode := frame[0] & 0x0F
	masked := frame[1]&0x80 != 0
	if !masked {
		return false, 0, nil, errors.New("缺少掩码位")
	}
	length := int(frame[1] & 0x7F)
	pos := 2
	switch length {
	case 126:
		length = int(frame[2])<<8 | int(frame[3])
		pos = 4
	case 127:
		length = 0
		for i := 0; i < 8; i++ {
			length = length<<8 | int(frame[2+i])
		}
		pos = 10
	}
	if len(frame) < pos+4+length {
		return false, 0, nil, fmt.Errorf("帧长度不足: 需 %d，实有 %d", pos+4+length, len(frame))
	}
	key := frame[pos : pos+4]
	payload := make([]byte, length)
	copy(payload, frame[pos+4:pos+4+length])
	for i := range payload {
		payload[i] ^= key[i%4]
	}
	return final, opcode, payload, nil
}

// newHTTPStub 起一个只回固定状态/体的 HTTP 服务，返回基址。
func newHTTPStub(t *testing.T, status int, body string) string {
	t.Helper()
	return newFakeHTTP(t, status, body)
}
