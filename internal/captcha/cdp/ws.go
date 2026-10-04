package cdp

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// WebSocket 客户端（**手写帧解析**，只用标准库）。
//
// 为什么手写：Go 标准库没有 WebSocket 客户端，而本项目对第三方依赖极克制
// （go.mod 只有 1 个直接依赖）。CDP 只需要 RFC 6455 的一个**极小子集** ——
// 客户端侧握手 + 文本帧 + 分片 + 掩码 + 三个控制帧，不需要扩展协商、不需要
// permessage-deflate、不需要服务端侧。手写反而比引入一个通用库更少意外。
//
// 明确不支持（也不需要）：任何扩展（RSV 位非 0 即报错）、二进制帧（CDP 只用
// 文本帧）、服务端掩码（服务端→客户端按 RFC 必须不掩码，掩码即协议错误）。

const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// maxFramePayload 是单帧载荷上限。CDP 消息都是 JSON 小包（几 KB），
// 1 MiB 足够；设上限是为了不因为一条畸形长度头就分配巨量内存。
const maxFramePayload = 1 << 20

// maxMessagePayload 是**重组后**消息上限（分片累加）。
const maxMessagePayload = 4 << 20

// errWSClosed 表示对端已发 Close 帧或连接已断。
var errWSClosed = errors.New("websocket 已关闭")

// wsConn 是一条已握手的 WebSocket 客户端连接。
//
// 读由调用方单 goroutine 驱动（`ReadMessage`），写内部串行化（`WriteText`）。
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader

	wmu sync.Mutex // 写串行化（CDP 会话可能有并发调用）
}

// dialWS 建立 WebSocket 连接。
//
// rawURL 只接受 `ws://` 与 `wss://`（CDP 的 `webSocketDebuggerUrl` 就是这两种）。
// **刻意不发 `Origin` 头** —— Chromium 的 DevTools 端点会拒绝带不白名单 Origin
// 的握手，而不带 Origin 时一律放行（浏览器内的页面才必须带）。
func dialWS(ctx context.Context, rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("解析 WebSocket 地址失败: %w", err)
	}
	var secure bool
	switch u.Scheme {
	case "ws":
		secure = false
	case "wss":
		secure = true
	default:
		return nil, fmt.Errorf("不支持的 WebSocket 协议 %q", u.Scheme)
	}

	host := u.Host
	if u.Port() == "" {
		if secure {
			host = net.JoinHostPort(u.Hostname(), "443")
		} else {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}

	d := &net.Dialer{Timeout: 10 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("连接 %s 失败: %w", host, err)
	}

	// 握手要在 ctx 截止时能中断，所以把 deadline 挂到裸连接上。
	if dl, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(dl)
	}

	c := raw
	if secure {
		tc, err := wrapTLS(ctx, raw, u.Hostname())
		if err != nil {
			_ = raw.Close()
			return nil, err
		}
		c = tc
	}

	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}

	key, err := wsKey()
	if err != nil {
		_ = c.Close()
		return nil, err
	}

	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("WebSocket 握手写入失败: %w", err)
	}

	br := bufio.NewReaderSize(c, 16<<10)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("WebSocket 握手响应读取失败: %w", err)
	}
	// 101 的 Body 是裸连接，不能当 HTTP 体去读 —— 这里只取状态与头。
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = c.Close()
		return nil, fmt.Errorf("WebSocket 握手失败: HTTP %d", resp.StatusCode)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		_ = c.Close()
		return nil, fmt.Errorf("WebSocket 握手失败: Upgrade=%q", resp.Header.Get("Upgrade"))
	}
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), wsAccept(key); got != want {
		_ = c.Close()
		return nil, fmt.Errorf("WebSocket 握手失败: Sec-WebSocket-Accept=%q", got)
	}

	// 握手完成，清掉握手期的 deadline（后续由各自的调用决定超时）。
	_ = c.SetDeadline(time.Time{})
	return &wsConn{conn: c, br: br}, nil
}

// WriteText 发一条文本消息（单帧；分片只对**入站**做兼容）。
func (c *wsConn) WriteText(payload []byte) error {
	if len(payload) > maxFramePayload {
		return fmt.Errorf("WebSocket 出站消息过大: %d 字节", len(payload))
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()

	frame, err := encodeFrame(opText, payload)
	if err != nil {
		return err
	}
	_, err = c.conn.Write(frame)
	return err
}

// ReadMessage 读一条完整**数据**消息（自动跳过 Ping/Pong、处理分片）。
//
// 返回对端 Close 帧或连接断开时返回 errWSClosed。
func (c *wsConn) ReadMessage() ([]byte, error) {
	var (
		buf     []byte
		started bool
	)
	for {
		final, opcode, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch opcode {
		case opPing:
			if err := c.pong(payload); err != nil {
				return nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			_ = c.writeControl(opClose, nil)
			return nil, errWSClosed
		case opBinary:
			return nil, errors.New("收到不支持的 WebSocket 二进制帧（CDP 只用文本帧）")
		case opText:
			if started {
				return nil, errors.New("WebSocket 协议错误：上一条消息未收完就来了新的数据帧")
			}
			buf, started = append(buf, payload...), true
		case opContinuation:
			if !started {
				return nil, errors.New("WebSocket 协议错误：续帧没有前置数据帧")
			}
			buf = append(buf, payload...)
		default:
			return nil, fmt.Errorf("WebSocket 协议错误：未知操作码 0x%X", opcode)
		}
		if len(buf) > maxMessagePayload {
			return nil, fmt.Errorf("WebSocket 入站消息过大: 超过 %d 字节", maxMessagePayload)
		}
		if final {
			return buf, nil
		}
	}
}

// Close 发 Close 帧并关闭底层连接。
func (c *wsConn) Close() error {
	_ = c.writeControl(opClose, nil)
	return c.conn.Close()
}

// CloseAbort 直接断开底层连接（不握手）。用于超时/取消路径。
func (c *wsConn) CloseAbort() { _ = c.conn.Close() }

// SetWriteDeadline 设置写超时（读侧由 ReadMessage 的调用方用 goroutine 兜）。
func (c *wsConn) SetWriteDeadline(t time.Time) { _ = c.conn.SetWriteDeadline(t) }

func (c *wsConn) pong(payload []byte) error { return c.writeControl(opPong, payload) }

func (c *wsConn) writeControl(opcode byte, payload []byte) error {
	frame, err := encodeFrame(opcode, payload)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.conn.Write(frame)
	return err
}

// readFrame 读一帧。返回 (final, opcode, 载荷)。
func (c *wsConn) readFrame() (bool, byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		// 对端直接断开（没发 Close）也是常态：CDP 目标被关掉就是这样。
		return false, 0, nil, errWSClosed
	}
	final := head[0]&0x80 != 0
	if head[0]&0x70 != 0 {
		return false, 0, nil, errors.New("WebSocket 协议错误：RSV 位非 0（不支持扩展）")
	}
	opcode := head[0] & 0x0F
	masked := head[1]&0x80 != 0

	length := uint64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, errWSClosed
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, errWSClosed
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > maxFramePayload {
		return false, 0, nil, fmt.Errorf("WebSocket 帧过大: %d 字节", length)
	}
	if opcode >= opClose && (!final || length > 125) {
		return false, 0, nil, errors.New("WebSocket 协议错误：控制帧必须 FIN 且载荷 ≤125 字节")
	}

	if masked {
		// 服务端→客户端按 RFC 6455 必须不掩码；掩码了说明对端在说别的协议。
		return false, 0, nil, errors.New("WebSocket 协议错误：服务端帧不应带掩码")
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, errWSClosed
	}
	return final, opcode, payload, nil
}

// encodeFrame 组装一个客户端帧（**必带掩码**，RFC 6455 §5.3）。
func encodeFrame(opcode byte, payload []byte) ([]byte, error) {
	n := len(payload)
	header := make([]byte, 0, 14)
	header = append(header, 0x80|opcode)

	switch {
	case n < 126:
		header = append(header, 0x80|byte(n))
	case n <= 0xFFFF:
		header = append(header, 0x80|126)
		header = binary.BigEndian.AppendUint16(header, uint16(n))
	default:
		header = append(header, 0x80|127)
		header = binary.BigEndian.AppendUint64(header, uint64(n))
	}

	var key [4]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, fmt.Errorf("生成 WebSocket 掩码失败: %w", err)
	}
	header = append(header, key[:]...)

	out := make([]byte, len(header)+n)
	copy(out, header)
	for i := 0; i < n; i++ {
		out[len(header)+i] = payload[i] ^ key[i%4]
	}
	return out, nil
}

// wsKey 生成 16 字节随机 key（base64）。
func wsKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成 WebSocket key 失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b[:]), nil
}

// wsAccept 按 RFC 6455 §4.2.2 算 Sec-WebSocket-Accept。
func wsAccept(key string) string {
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h[:])
}
