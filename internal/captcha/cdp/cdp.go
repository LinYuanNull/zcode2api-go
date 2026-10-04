package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ProtocolError 是 CDP 返回的 `error` 对象。
type ProtocolError struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

// Error 实现 error。
func (e *ProtocolError) Error() string {
	return fmt.Sprintf("CDP 错误 %d: %s", e.Code, e.Message)
}

type rpcMessage struct {
	ID        *int64          `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *ProtocolError  `json:"error,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

type rpcRequest struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

// Client 是一条 CDP 连接（通常连到**浏览器级**端点，页面级命令靠 sessionId 路由）。
//
// 只做三件事：请求/响应按 `id` 配对、事件回调解绑、`sessionId` 原样透传。
// 命令语义（参数形状）由本文件的类型化封装表达，见文件末尾。
type Client struct {
	ws *wsConn

	mu      sync.Mutex
	next    int64
	pending map[int64]chan rpcMessage

	eventsMu sync.Mutex
	onEvent  func(method string, params json.RawMessage, sessionID string)

	done      chan struct{}
	closeOnce sync.Once
	readErr   error
	readErrMu sync.Mutex
}

// Dial 连到 `webSocketDebuggerUrl` 并启动读循环。
func Dial(ctx context.Context, wsURL string) (*Client, error) {
	ws, err := dialWS(ctx, wsURL)
	if err != nil {
		return nil, err
	}
	c := &Client{
		ws:      ws,
		pending: make(map[int64]chan rpcMessage),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// OnEvent 注册事件回调（无 `id` 的消息）。回调在**读循环**里同步执行，
// 不要在里面做会阻塞的调用；求解流程只用轮询，不依赖事件。
func (c *Client) OnEvent(fn func(method string, params json.RawMessage, sessionID string)) {
	c.eventsMu.Lock()
	c.onEvent = fn
	c.eventsMu.Unlock()
}

// Close 关闭连接（幂等）。
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.ws.Close()
	})
}

// Err 返回读循环的终结原因（`Close` 之后不保证有意义）。
func (c *Client) Err() error {
	c.readErrMu.Lock()
	defer c.readErrMu.Unlock()
	return c.readErr
}

func (c *Client) readLoop() {
	for {
		raw, err := c.ws.ReadMessage()
		if err != nil {
			c.readErrMu.Lock()
			if c.readErr == nil && !errors.Is(err, errWSClosed) {
				c.readErr = err
			}
			c.readErrMu.Unlock()
			c.failAll(err)
			c.Close()
			return
		}
		var msg rpcMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			// 无法解析的消息不该让整条连接挂掉；忽略之（CDP 偶发非 JSON 帧）。
			continue
		}
		if msg.ID != nil {
			c.mu.Lock()
			ch := c.pending[*msg.ID]
			delete(c.pending, *msg.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- msg
			}
			continue
		}
		c.eventsMu.Lock()
		fn := c.onEvent
		c.eventsMu.Unlock()
		if fn != nil {
			fn(msg.Method, msg.Params, msg.SessionID)
		}
	}
}

// failAll 在读循环终结时唤醒所有等待者，避免调用方一直等到 ctx 超时。
func (c *Client) failAll(err error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[int64]chan rpcMessage)
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- rpcMessage{Error: &ProtocolError{Code: -1, Message: "连接已关闭: " + err.Error()}}
	}
}

// Call 发一条命令并等它的响应。`sessionID` 为空表示浏览器级命令。
//
// `out` 为 nil 时丢弃 result；否则把 result 反序列化进去。
func (c *Client) Call(ctx context.Context, sessionID, method string, params any, out any) error {
	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("编码 %s 参数失败: %w", method, err)
		}
		rawParams = b
	}

	c.mu.Lock()
	c.next++
	id := c.next
	ch := make(chan rpcMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	body, err := json.Marshal(rpcRequest{ID: id, Method: method, Params: rawParams, SessionID: sessionID})
	if err != nil {
		return fmt.Errorf("编码 %s 请求失败: %w", method, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		c.ws.SetWriteDeadline(dl)
	}
	if err := c.ws.WriteText(body); err != nil {
		return fmt.Errorf("发送 %s 失败: %w", method, err)
	}

	select {
	case msg := <-ch:
		if msg.Error != nil {
			return msg.Error
		}
		if out != nil && len(msg.Result) > 0 {
			if err := json.Unmarshal(msg.Result, out); err != nil {
				return fmt.Errorf("解码 %s 响应失败: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s 超时或被取消: %w", method, ctx.Err())
	case <-c.done:
		return fmt.Errorf("%s 失败: 连接已关闭", method)
	}
}

// ------------------------------------------------------------ 目标发现

// BrowserWSURL 用 DevTools 的 HTTP 端点查出浏览器级 WebSocket 地址。
//
// 这是启动浏览器之后的第一步：`/json/version` 回 `{"webSocketDebuggerUrl": "ws://…"}`。
func BrowserWSURL(ctx context.Context, httpBase string) (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(httpBase, "/")+"/json/version", nil)
	if err != nil {
		return "", err
	}
	// 浏览器把 DevTools 端点只绑在回环地址上，绝不能走代理。
	client.Transport = &http.Transport{Proxy: nil}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("查询 DevTools 端点失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("读取 DevTools 端点失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("DevTools 端点 HTTP %d", resp.StatusCode)
	}
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", fmt.Errorf("解析 DevTools 端点失败: %w", err)
	}
	if v.WebSocketDebuggerURL == "" {
		return "", errors.New("DevTools 端点未返回 webSocketDebuggerUrl")
	}
	return v.WebSocketDebuggerURL, nil
}

// ------------------------------------------------------------ 类型化命令
//
// 求解链路**只用到下面这 7 条**命令（计划文档写「6 条」，漏了 `Target.attachToTarget`
// —— 没有它就无法把页面级命令投递到新目标上；已在 PROVENANCE 登记这处更正）：
//
//	Target.createTarget                    开一个 about:blank 目标
//	Target.attachToTarget                  取得 sessionId（flatten 模式）
//	Page.addScriptToEvaluateOnNewDocument  反探测补丁（必须早于导航）
//	Emulation.setUserAgentOverride         改 UA（必须早于导航）
//	Page.navigate                          打开本地求解页
//	Runtime.evaluate                       轮询就绪 + 执行求解表达式（awaitPromise）
//	Target.closeTarget                     收尾
//
// **刻意不用** `Page.enable`：求解靠轮询 `document.readyState` 判断就绪，
// 不需要 load 事件，少一条命令少一处失败点。

// CreateTarget 开一个新页面目标，返回 targetId。
func (c *Client) CreateTarget(ctx context.Context, url string) (string, error) {
	if url == "" {
		url = "about:blank"
	}
	var res struct {
		TargetID string `json:"targetId"`
	}
	if err := c.Call(ctx, "", "Target.createTarget", map[string]any{"url": url}, &res); err != nil {
		return "", err
	}
	if res.TargetID == "" {
		return "", errors.New("Target.createTarget 未返回 targetId")
	}
	return res.TargetID, nil
}

// AttachToTarget 附着到目标，返回 sessionId（flatten=true，命令随消息带 sessionId 发送）。
func (c *Client) AttachToTarget(ctx context.Context, targetID string) (string, error) {
	var res struct {
		SessionID string `json:"sessionId"`
	}
	err := c.Call(ctx, "", "Target.attachToTarget", map[string]any{
		"targetId": targetID,
		"flatten":  true,
	}, &res)
	if err != nil {
		return "", err
	}
	if res.SessionID == "" {
		return "", errors.New("Target.attachToTarget 未返回 sessionId")
	}
	return res.SessionID, nil
}

// AddScriptOnNewDocument 注入脚本，**对之后每一个新文档**在最早时机执行。
func (c *Client) AddScriptOnNewDocument(ctx context.Context, sessionID, source string) error {
	return c.Call(ctx, sessionID, "Page.addScriptToEvaluateOnNewDocument",
		map[string]any{"source": source}, nil)
}

// UAOverride 描述 `Emulation.setUserAgentOverride` 的入参。
//
// `UserAgentMetadata` 与 `UserAgent` 必须自洽：UA 字符串声称 Windows x64，
// 元数据就得是同一套，否则风控侧读到互相矛盾的信号。
type UAOverride struct {
	UserAgent         string             `json:"userAgent"`
	AcceptLanguage    string             `json:"acceptLanguage,omitempty"`
	Platform          string             `json:"platform,omitempty"`
	UserAgentMetadata *UserAgentMetadata `json:"userAgentMetadata,omitempty"`
}

// UserAgentMetadata 对应 CDP 的 `Emulation.UserAgentMetadata`。
//
// 字段名是协议里的 camelCase（`fullVersionList` / `fullVersion` /
// `platformVersion`），不是 Go 习惯的写法 —— 这些名字直接进 JSON。
//
// # 哪些字段**不能**加 omitempty
//
// 实测（`internal/captcha/live_test.go` 的探针，判据是浏览器自己吐的
// `/json/protocol`）：`platform` / `platformVersion` / `architecture` /
// `model` / `mobile` 这五项**没有** optional 标记 —— 它们是必需项。
// 一旦给其中任何一个加上 `omitempty`，空值就会被序列化时省掉，整条
// `Emulation.setUserAgentOverride` 直接回 `-32602 Invalid parameters`，
// 而且错误信息里**不会**说是哪个字段缺了。
//
// 最阴的一处是 `model`：桌面 Chrome/Edge 的 `model` 本来就是空串，
// 看着「没值」，但必须显式发 `"model":""`。这个坑花了一轮真机探针才定位到。
type UserAgentMetadata struct {
	// Brands 是页面上 `navigator.userAgentData.brands` 的取值（"主版本"形态）。
	Brands []UserAgentBrand `json:"brands,omitempty"`
	// FullVersionList 是 `navigator.userAgentData.getHighEntropyValues()` 里的
	// **完整版本**形态（可选；不给时浏览器会拿 Brands 顶上）。
	FullVersionList []UserAgentBrand `json:"fullVersionList,omitempty"`
	FullVersion     string           `json:"fullVersion,omitempty"`

	// 以下五项是必需项，见上面的说明：不加 omitempty。
	Platform        string `json:"platform"`
	PlatformVersion string `json:"platformVersion"`
	Architecture    string `json:"architecture"`
	Model           string `json:"model"`
	Mobile          bool   `json:"mobile"`

	Bitness string `json:"bitness,omitempty"`
	Wow64   bool   `json:"wow64,omitempty"`
}

// UserAgentBrand 是 UA 品牌项。
type UserAgentBrand struct {
	Brand   string `json:"brand"`
	Version string `json:"version"`
}

// SetUserAgentOverride 覆盖 UA（必须在导航之前调用才作用于该次导航的请求）。
func (c *Client) SetUserAgentOverride(ctx context.Context, sessionID string, ua UAOverride) error {
	return c.Call(ctx, sessionID, "Emulation.setUserAgentOverride", ua, nil)
}

// Navigate 导航到 url，返回 frameId。
func (c *Client) Navigate(ctx context.Context, sessionID, url string) (string, error) {
	var res struct {
		FrameID   string `json:"frameId"`
		ErrorText string `json:"errorText"`
	}
	if err := c.Call(ctx, sessionID, "Page.navigate", map[string]any{"url": url}, &res); err != nil {
		return "", err
	}
	if res.ErrorText != "" {
		return "", errors.New("Page.navigate 失败: " + res.ErrorText)
	}
	return res.FrameID, nil
}

// CloseTarget 关闭目标（失败不致命，由调用方决定是否忽略）。
func (c *Client) CloseTarget(ctx context.Context, targetID string) error {
	var res struct {
		Success bool `json:"success"`
	}
	return c.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": targetID}, &res)
}

// EvalResult 是 `Runtime.evaluate` 的返回。
type EvalResult struct {
	Result struct {
		Type        string          `json:"type"`
		Subtype     string          `json:"subtype"`
		Value       json.RawMessage `json:"value"`
		Description string          `json:"description"`
	} `json:"result"`
	ExceptionDetails *struct {
		Text      string `json:"text"`
		Exception *struct {
			Description string `json:"description"`
		} `json:"exception"`
	} `json:"exceptionDetails"`
}

// Evaluate 执行表达式。`awaitPromise` 为真时等待 Promise 落定并取其值。
func (c *Client) Evaluate(ctx context.Context, sessionID, expr string, awaitPromise bool) (*EvalResult, error) {
	var res EvalResult
	err := c.Call(ctx, sessionID, "Runtime.evaluate", map[string]any{
		"expression":    expr,
		"awaitPromise":  awaitPromise,
		"returnByValue": true,
	}, &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// EvaluateString 执行表达式并要求结果是字符串（`null`/`undefined` → 空串）。
func (c *Client) EvaluateString(ctx context.Context, sessionID, expr string, awaitPromise bool) (string, error) {
	res, err := c.Evaluate(ctx, sessionID, expr, awaitPromise)
	if err != nil {
		return "", err
	}
	if res.ExceptionDetails != nil {
		msg := res.ExceptionDetails.Text
		if res.ExceptionDetails.Exception != nil && res.ExceptionDetails.Exception.Description != "" {
			msg = res.ExceptionDetails.Exception.Description
		}
		return "", fmt.Errorf("页面内表达式抛错: %s", msg)
	}
	switch res.Result.Type {
	case "string":
		var s string
		if err := json.Unmarshal(res.Result.Value, &s); err != nil {
			return "", fmt.Errorf("表达式结果不是字符串: %w", err)
		}
		return s, nil
	case "null", "undefined":
		return "", nil
	default:
		return "", nil
	}
}
