package cdp

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// 本文件给出一个**进程内**的假 CDP 服务：真握手 + 真帧编解码 + 真 JSON-RPC。
//
// 它存在的理由：CDP 客户端的两类错误（帧层 masking/分片、会话层 id 配对/
// sessionId 路由）都能在没有浏览器的前提下被完整覆盖。真机验收另有其测
// （见 internal/captcha 的 e2e 与 tools/e2e_captcha.py），两者不互相替代。

type fakeCall struct {
	ID        int64
	Method    string
	Params    json.RawMessage
	SessionID string
}

type fakeReply struct {
	Result any
	Err    *ProtocolError
}

// fakeServer 是一个假 CDP 浏览器端点。
type fakeServer struct {
	srv *httptest.Server

	mu       sync.Mutex
	calls    []fakeCall
	handler  func(method string, params json.RawMessage, session string) (fakeReply, bool)
	fragSize int  // >0 时把每条响应拆成该大小的分片发出（测重组）
	pingOnce bool // 首个请求前先发一个 Ping（测控制帧处理）
	reorder2 bool // 前两条响应**倒序**发出（测按 id 配对而非按到达顺序）
	pongs    int  // 收到的 Pong 计数
	silent   bool // 收到命令不回（测超时路径）
}

// Pongs 返回收到的 Pong 帧数。
func (f *fakeServer) Pongs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pongs
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// wsURL 返回该假服务的 WebSocket 地址。
func (f *fakeServer) wsURL() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http")
}

// httpBase 返回该假服务的 HTTP 基址（用于 BrowserWSURL）。
func (f *fakeServer) httpBase() string { return f.srv.URL }

// Calls 返回收到的全部命令（副本）。
func (f *fakeServer) Calls() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

// methods 返回收到的方法名序列。
func (f *fakeServer) methods() []string {
	out := []string{}
	for _, c := range f.Calls() {
		out = append(out, c.Method)
	}
	return out
}

func (f *fakeServer) setHandler(h func(string, json.RawMessage, string) (fakeReply, bool)) {
	f.mu.Lock()
	f.handler = h
	f.mu.Unlock()
}

func (f *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/json/version" {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"webSocketDebuggerUrl":%q,"Browser":"fake"}`, f.wsURL())
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "expected websocket", http.StatusBadRequest)
		return
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()

	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + wsAccept(key) + "\r\n\r\n")
	if err := rw.Flush(); err != nil {
		return
	}

	f.serveLoop(rw)
}

func (f *fakeServer) serveLoop(rw *bufio.ReadWriter) {
	pinged := false
	var held []byte // reorder2：被扣住待后发的第一条响应
	for {
		final, opcode, payload, err := readClientFrame(rw.Reader)
		if err != nil {
			return
		}
		if !final {
			// 客户端不发出站分片；收到就报错结束。
			return
		}
		switch opcode {
		case opPing:
			_ = writeServerFrame(rw.Writer, opPong, true, payload)
			_ = rw.Flush()
			continue
		case opPong:
			f.mu.Lock()
			f.pongs++
			f.mu.Unlock()
			continue
		case opClose:
			return
		case opText:
		default:
			return
		}

		var req rpcRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, fakeCall{ID: req.ID, Method: req.Method, Params: req.Params, SessionID: req.SessionID})
		pingOnce := f.pingOnce
		f.pingOnce = false
		frag := f.fragSize
		reorder := f.reorder2 && len(f.calls) == 1
		silent := f.silent
		handler := f.handler
		f.mu.Unlock()

		if pingOnce && !pinged {
			pinged = true
			_ = writeServerFrame(rw.Writer, opPing, true, []byte("hb"))
			_ = rw.Flush()
		}

		if silent {
			continue
		}

		reply := fakeReply{Result: map[string]any{}}
		if handler != nil {
			if r, ok := handler(req.Method, req.Params, req.SessionID); ok {
				reply = r
			}
		}
		out := map[string]any{"id": req.ID}
		if reply.Err != nil {
			out["error"] = reply.Err
		} else {
			out["result"] = reply.Result
		}
		if req.SessionID != "" {
			out["sessionId"] = req.SessionID
		}
		body, err := json.Marshal(out)
		if err != nil {
			return
		}

		// reorder2：先把第一条扣住，等第二条算完再按「后、先」顺序发。
		if reorder {
			held = body
			continue
		}
		if err := writeServerMessage(rw.Writer, body, frag); err != nil {
			return
		}
		if held != nil {
			if err := writeServerMessage(rw.Writer, held, frag); err != nil {
				return
			}
			held = nil
		}
		if err := rw.Flush(); err != nil {
			return
		}
	}
}

// newFakeHTTP 起一个只回固定状态码/体的 HTTP 服务，返回基址（用于「非 101 / 非 200」分支）。
func newFakeHTTP(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// ------------------------------------------------------------ 假服务端帧编解码

// writeServerMessage 发一条文本消息（frag>0 时按 frag 切成多帧，测分片重组）。
func writeServerMessage(w io.Writer, payload []byte, frag int) error {
	if frag <= 0 || len(payload) <= frag {
		return writeServerFrame(w, opText, true, payload)
	}
	for i := 0; i < len(payload); i += frag {
		end := i + frag
		if end > len(payload) {
			end = len(payload)
		}
		last := end == len(payload)
		op := byte(opContinuation)
		if i == 0 {
			op = opText
		}
		if err := writeServerFrame(w, op, last, payload[i:end]); err != nil {
			return err
		}
	}
	return nil
}

// writeServerFrame 发一帧（服务端**不掩码**）。
func writeServerFrame(w io.Writer, opcode byte, final bool, payload []byte) error {
	var head []byte
	b0 := opcode
	if final {
		b0 |= 0x80
	}
	head = append(head, b0)
	n := len(payload)
	switch {
	case n < 126:
		head = append(head, byte(n))
	case n <= 0xFFFF:
		head = append(head, 126)
		head = binary.BigEndian.AppendUint16(head, uint16(n))
	default:
		head = append(head, 127)
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}
	if _, err := w.Write(head); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// readClientFrame 读一帧并要求带掩码（客户端→服务端按 RFC 必须掩码）。
func readClientFrame(r *bufio.Reader) (bool, byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return false, 0, nil, err
	}
	final := h[0]&0x80 != 0
	opcode := h[0] & 0x0F
	masked := h[1]&0x80 != 0
	if !masked {
		return false, 0, nil, fmt.Errorf("客户端帧未掩码（违反 RFC 6455 §5.3）")
	}
	length := uint64(h[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	var key [4]byte
	if _, err := io.ReadFull(r, key[:]); err != nil {
		return false, 0, nil, err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return false, 0, nil, err
	}
	for i := range payload {
		payload[i] ^= key[i%4]
	}
	return final, opcode, payload, nil
}
