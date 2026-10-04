// Command mitmupstream 是**出站契约采样器 + 假上游**（开发工具，不随发布产物分发）。
//
// 为什么需要它
// ------------
// A1 采的是**入站**契约（客户端 → 网关）。A4 的转发链路要的是**出站**契约
// （网关 → `zcode.z.ai` / `api.z.ai`），而它只在有「可用账号」时才发生 ——
// 实测一个假 apiKey 账号（新增时 `status=active`）就会让网关真的发起出站调用。
//
// 出站地址是**硬编码**的（实测 10 个候选环境变量都不生效），但**出站走 `HTTPS_PROXY`**
// （实测代理收到 `CONNECT zcode.z.ai:443`）。所以这里做一个本地 MITM：
// 自签 CA → 生成 `zcode.z.ai` / `api.z.ai` 的叶子证书 → 代理时用叶子证书终结 TLS，
// 于是能看到明文请求/响应。把 CA 写出来，靶机侧设 `SSL_CERT_FILE=<CA>` 即可
// （httpx 在 `trust_env=True` 时读 `SSL_CERT_FILE`）。
//
// 两种模式
// --------
//   - `--mode capture`：把请求**真转发**给上游（直连，绕过代理），把请求/响应
//     逐条写成 JSON Lines（`--log`）。用于**采样出站契约**。
//   - `--mode fake`：不发上游，按 `--rules` 的规则返回预设响应。
//     用于**行为对照测试**：同一份规则下，靶机与 Go 实现必须表现一致。
//
// 用法:
//
//	mitmupstream --mode capture --ca-out ca.pem --log out.jsonl
//	mitmupstream --mode fake --rules rules.json --ca-out ca.pem --log out.jsonl
//
// 只用标准库。TLS 层只宣告 `http/1.1`（不宣告 h2），避免要额外实现 HTTP/2 帧解析。
package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	maxReqBody  = 1 << 22 // 4 MiB
	maxRespBody = 1 << 22
)

type record struct {
	TS           float64             `json:"ts"`
	Kind         string              `json:"kind,omitempty"` // 空=HTTP 请求；connect=CONNECT 目标（含握手失败）
	Host         string              `json:"host"`
	Method       string              `json:"method"`
	Path         string              `json:"path"`
	ReqHeaders   map[string][]string `json:"req_headers"`
	ReqBodyB64   string              `json:"req_body_b64,omitempty"`
	ReqBodyText  string              `json:"req_body_text,omitempty"`
	RespStatus   int                 `json:"resp_status"`
	RespHeaders  map[string][]string `json:"resp_headers"`
	RespBodyB64  string              `json:"resp_body_b64,omitempty"`
	RespBodyText string              `json:"resp_body_text,omitempty"`
	RespChunks   int                 `json:"resp_chunks,omitempty"`
	Error        string              `json:"error,omitempty"`
}

// rule 是 fake 模式的一条匹配规则。
type rule struct {
	Host       string            `json:"host"`        // 空 = 任意；否则要求 host 等于该值或以 .该值 结尾
	Method     string            `json:"method"`      // 空 = 任意
	PathSuffix string            `json:"path_suffix"` // 空 = 任意；否则要求 path 以此结尾
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	// BodyFile 非空时从该文件读 body（相对规则文件所在目录）。用于内联太长的响应，
	// 例如 `client/configs` 的真实夹具（23 KB）。
	BodyFile string `json:"body_file"`
	// Times > 0 时该规则最多生效 Times 次，之后自动跳过。用于表达「先失败 N 次再成功」
	// 这类序列（前置规则写 Times，后置规则兜底）。
	Times int `json:"times"`
	// DelayMs > 0 时在响应前等待，用于模拟慢上游 / 观察冷却与超时行为。
	DelayMs int `json:"delay_ms"`
	// Abort 为真时**不回任何响应**，直接关掉连接 —— 模拟「连接失败」分支
	// （靶机的日志里这是与「鉴权失败 401」并列的一类失败）。
	Abort bool `json:"abort"`
	// Chunks 非空时按 SSE 分块发送（每个元素一个 `data:` 载荷，自动补 `data: ` 与空行）。
	Chunks []string `json:"chunks"`
	// ChunksRaw 非空时按 SSE 分块发送，但**每个元素原样写出**（自带 `event:` 行也照写），
	// 只在末尾补一个空行。真实 Anthropic SSE 每帧都有 `event:` 行，只发 `data:` 测不透。
	ChunksRaw []string `json:"chunks_raw"`
}

// rulesMu 保护 rule.Times 的递减（一次运行内多条连接会并发匹配）。
var rulesMu sync.Mutex

var (
	mode      = flag.String("mode", "capture", "capture | fake")
	listen    = flag.String("listen", "127.0.0.1:0", "监听地址")
	caOut     = flag.String("ca-out", "", "把 CA 证书 PEM 写到这个路径（给 SSL_CERT_FILE）")
	logPath   = flag.String("log", "", "把捕获记录追加为 JSON Lines")
	hostsFlag = flag.String("hosts", "zcode.z.ai,api.z.ai", "启动时预热叶子证书的主机（逗号分隔）；其余主机在 CONNECT 时按需动态签发")
	rulesPath = flag.String("rules", "", "fake 模式的规则文件（JSON 数组）")
	upstream  = flag.String("upstream", "", "capture 模式下强制转发到该 host:port（默认按 CONNECT 目标）")
	logConns  = flag.Bool("log-connects", false, "把每个 CONNECT 目标也写成一条记录（kind=connect）")
)

func main() {
	flag.Parse()
	ca, err := newCA()
	if err != nil {
		log.Fatalf("生成 CA 失败: %v", err)
	}
	if *caOut != "" {
		if err := os.WriteFile(*caOut, ca.caPEM, 0o644); err != nil {
			log.Fatalf("写 CA 失败: %v", err)
		}
	}

	var rules []rule
	if *mode == "fake" {
		if *rulesPath == "" {
			log.Fatal("fake 模式必须给 --rules")
		}
		b, err := os.ReadFile(*rulesPath)
		if err != nil {
			log.Fatalf("读规则失败: %v", err)
		}
		if err := json.Unmarshal(b, &rules); err != nil {
			log.Fatalf("解析规则失败: %v", err)
		}
		// body_file 在加载期就解析成绝对路径读进来，运行期规则即自包含。
		base := filepath.Dir(*rulesPath)
		for i := range rules {
			if rules[i].BodyFile == "" {
				continue
			}
			fp := rules[i].BodyFile
			if !filepath.IsAbs(fp) {
				fp = filepath.Join(base, fp)
			}
			fb, err := os.ReadFile(fp)
			if err != nil {
				log.Fatalf("读规则 %d 的 body_file 失败: %v", i, err)
			}
			rules[i].Body = string(fb)
		}
	}

	var mu sync.Mutex
	var logf *os.File
	if *logPath != "" {
		logf, err = os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Fatalf("打开日志失败: %v", err)
		}
		defer logf.Close()
	}
	emit := func(rec record) {
		b, _ := json.Marshal(rec)
		mu.Lock()
		defer mu.Unlock()
		if logf != nil {
			logf.Write(append(b, '\n'))
			logf.Sync()
		}
		fmt.Fprintln(os.Stderr, "CAPTURE", string(b[:min(len(b), 400)]))
	}

	// 预热：--hosts 里的主机先签好叶子证书（其余主机在 CONNECT 时按需签发）。
	for _, h := range splitCSV(*hostsFlag) {
		if _, err := ca.leafFor(h); err != nil {
			log.Fatalf("为 %s 签发叶子证书失败: %v", h, err)
		}
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("监听失败: %v", err)
	}
	fmt.Printf("LISTEN %s\n", ln.Addr().String())
	// ⚠️ **不要在这里 `os.Stdout.Sync()`**。Go 的 `os.Stdout` 本来就无缓冲，
	// 每行 Write 都直接落到操作系统，Sync 对正确性毫无帮助；而它在 Windows 上是
	// `FlushFileBuffers`，**对管道会阻塞到对端把缓冲读空**。于是当调用方（脚本）
	// 自己挑好端口、用 `--listen` 传进来、因而**不需要读 stdout** 时，这一行就永久阻塞：
	// `net.Listen` 已经成功（TCP 依然能连上、netstat 里是 ESTABLISHED），
	// 但下面那行 `srv.Serve` 永远不执行 ⇒ 所有请求挂在 accept 队列里，
	// 表现为「出站请求永无响应」。实测复现见 tools/e2e_quota.py 的排查记录。

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "this is a CONNECT proxy", http.StatusMethodNotAllowed)
			return
		}
		handleConnect(w, r, ca, rules, emit)
	})}
	log.Fatal(srv.Serve(ln))
}

func handleConnect(w http.ResponseWriter, r *http.Request, ca *certAuthority, rules []rule, emit func(record)) {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")

	// 每个 CONNECT 目标都留痕（否则「没出站」与「出站到了没覆盖的主机」分不清）。
	if *logConns {
		emit(record{TS: float64(time.Now().UnixNano()) / 1e9, Kind: "connect", Host: host})
	}

	leaf, err := ca.leafFor(host)
	if err != nil {
		emit(record{TS: float64(time.Now().UnixNano()) / 1e9, Kind: "connect", Host: host,
			Error: "签发叶子证书失败: " + err.Error()})
		return
	}
	tlsConn := tls.Server(conn, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		NextProtos:   []string{"http/1.1"},
		MinVersion:   tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		// 握手失败也要落一条记录：这正是「请求出去了却什么也看不到」的那种情形。
		emit(record{TS: float64(time.Now().UnixNano()) / 1e9, Kind: "connect", Host: host,
			Error: "TLS 握手失败: " + err.Error()})
		log.Printf("TLS 握手失败 (%s): %v", host, err)
		return
	}
	defer tlsConn.Close()

	br := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		if !serveOne(tlsConn, req, host, rules, emit) {
			return
		}
	}
}

// serveOne 处理一个请求；返回 false 表示该连接应当关闭。
func serveOne(client net.Conn, req *http.Request, host string, rules []rule, emit func(record)) bool {
	reqBody, _ := io.ReadAll(io.LimitReader(req.Body, maxReqBody))
	req.Body.Close()

	rec := record{
		TS:         float64(time.Now().UnixNano()) / 1e9,
		Host:       host,
		Method:     req.Method,
		Path:       req.URL.RequestURI(),
		ReqHeaders: map[string][]string(req.Header),
	}
	rec.ReqBodyText, rec.ReqBodyB64 = encodeBody(reqBody)

	if *mode == "fake" {
		rl := matchRule(rules, host, req)
		if rl == nil {
			rec.RespStatus = 599
			rec.RespBodyText = "no matching rule"
			writeResponse(client, 599, map[string]string{"Content-Type": "text/plain"}, []byte("no matching rule"), nil)
			emit(rec)
			return false
		}
		if rl.DelayMs > 0 {
			time.Sleep(time.Duration(rl.DelayMs) * time.Millisecond)
		}
		if rl.Abort {
			rec.RespStatus = 0
			rec.Error = "按规则中断连接（模拟连接失败）"
			emit(rec)
			return false
		}
		hdr := map[string]string{"Content-Type": "application/json"}
		for k, v := range rl.Headers {
			hdr[k] = v
		}
		status := rl.Status
		if status == 0 {
			status = 200
		}
		rec.RespStatus = status
		if len(rl.ChunksRaw) > 0 {
			hdr["Content-Type"] = "text/event-stream"
			rec.RespChunks = len(rl.ChunksRaw)
			var acc strings.Builder
			writeResponse(client, status, hdr, nil, func(flush func([]byte)) {
				for _, c := range rl.ChunksRaw {
					frame := c + "\n\n"
					acc.WriteString(frame)
					flush([]byte(frame))
				}
			})
			rec.RespBodyText = acc.String()
		} else if len(rl.Chunks) > 0 {
			hdr["Content-Type"] = "text/event-stream"
			rec.RespChunks = len(rl.Chunks)
			var acc strings.Builder
			writeResponse(client, status, hdr, nil, func(flush func([]byte)) {
				for _, c := range rl.Chunks {
					frame := "data: " + c + "\n\n"
					acc.WriteString(frame)
					flush([]byte(frame))
				}
			})
			rec.RespBodyText = acc.String()
		} else {
			writeResponse(client, status, hdr, []byte(rl.Body), nil)
			rec.RespBodyText, rec.RespBodyB64 = encodeBody([]byte(rl.Body))
		}
		rec.RespHeaders = toMulti(hdr)
		emit(rec)
		return true
	}

	// capture：真转发（直连，绕过代理）
	target := host + ":443"
	if *upstream != "" {
		target = *upstream
	}
	up, err := net.DialTimeout("tcp", target, 15*time.Second)
	if err != nil {
		rec.Error = "拨号失败: " + err.Error()
		rec.RespStatus = 502
		writeResponse(client, 502, map[string]string{"Content-Type": "text/plain"}, []byte("dial: "+err.Error()), nil)
		emit(rec)
		return false
	}
	defer up.Close()
	sni := host
	if h, _, e := net.SplitHostPort(*upstream); e == nil && *upstream != "" {
		sni = h
	}
	upTLS := tls.Client(up, &tls.Config{ServerName: sni, MinVersion: tls.VersionTLS12})
	if err := upTLS.Handshake(); err != nil {
		rec.Error = "上游 TLS 失败: " + err.Error()
		rec.RespStatus = 502
		writeResponse(client, 502, map[string]string{"Content-Type": "text/plain"}, []byte("upstream tls: "+err.Error()), nil)
		emit(rec)
		return false
	}
	defer upTLS.Close()

	outReq := req.Clone(req.Context())
	outReq.URL.Scheme = "https"
	outReq.URL.Host = sni
	outReq.RequestURI = ""
	outReq.Host = sni
	outReq.Header.Del("Proxy-Connection")
	outReq.Body = io.NopCloser(strings.NewReader(string(reqBody)))
	outReq.ContentLength = int64(len(reqBody))
	if err := outReq.Write(upTLS); err != nil {
		rec.Error = "写上游失败: " + err.Error()
		rec.RespStatus = 502
		emit(rec)
		return false
	}

	upBR := bufio.NewReader(upTLS)
	resp, err := http.ReadResponse(upBR, outReq)
	if err != nil {
		rec.Error = "读上游响应失败: " + err.Error()
		rec.RespStatus = 502
		emit(rec)
		return false
	}
	defer resp.Body.Close()
	rec.RespStatus = resp.StatusCode
	rec.RespHeaders = map[string][]string(resp.Header)

	var acc []byte
	chunks := 0
	writeResponse(client, resp.StatusCode, singleValue(resp.Header), nil, func(flush func([]byte)) {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				chunks++
				flush(buf[:n])
				if len(acc) < maxRespBody {
					acc = append(acc, buf[:n]...)
				}
			}
			if rerr != nil {
				break
			}
		}
	})
	rec.RespChunks = chunks
	rec.RespBodyText, rec.RespBodyB64 = encodeBody(acc)
	emit(rec)
	return false
}

func matchRule(rules []rule, host string, req *http.Request) *rule {
	p := req.URL.Path
	rulesMu.Lock()
	defer rulesMu.Unlock()
	for i := range rules {
		r := &rules[i]
		if r.Host != "" && !(host == r.Host || strings.HasSuffix(host, "."+r.Host)) {
			continue
		}
		if r.Method != "" && !strings.EqualFold(r.Method, req.Method) {
			continue
		}
		if r.PathSuffix != "" && !strings.HasSuffix(p, r.PathSuffix) {
			continue
		}
		if r.Times < 0 {
			continue // 已用尽
		}
		if r.Times > 0 {
			r.Times--
			if r.Times == 0 {
				r.Times = -1 // 用尽，后续跳过
			}
		}
		return r
	}
	return nil
}

// writeResponse 写一个 HTTP/1.1 响应。body 非 nil 时直接写；stream 非 nil 时用分块传输。
func writeResponse(w io.Writer, status int, hdr map[string]string, body []byte, stream func(flush func([]byte))) {
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
	hasCT := false
	for k, v := range hdr {
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") {
			continue
		}
		if strings.EqualFold(k, "Content-Type") {
			hasCT = true
		}
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	if !hasCT {
		b.WriteString("Content-Type: application/json\r\n")
	}
	if stream != nil {
		b.WriteString("Transfer-Encoding: chunked\r\n")
	} else {
		fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
	}
	b.WriteString("\r\n")
	io.WriteString(w, b.String())

	if stream != nil {
		flush := func(p []byte) {
			fmt.Fprintf(w, "%x\r\n", len(p))
			w.Write(p)
			io.WriteString(w, "\r\n")
			if f, ok := w.(interface{ Flush() error }); ok {
				f.Flush()
			}
		}
		stream(flush)
		io.WriteString(w, "0\r\n\r\n")
		return
	}
	w.Write(body)
}

func encodeBody(b []byte) (text, b64 string) {
	if len(b) == 0 {
		return "", ""
	}
	if json.Valid(b) || isMostlyText(b) {
		return string(b), ""
	}
	return "", base64.StdEncoding.EncodeToString(b)
}

func isMostlyText(b []byte) bool {
	bad := 0
	for _, c := range b {
		if c == 0 {
			return false
		}
		if c < 0x09 || (c > 0x0d && c < 0x20) {
			bad++
		}
	}
	return bad*20 < len(b)
}

func singleValue(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k := range h {
		out[k] = h.Get(k)
	}
	return out
}

func toMulti(h map[string]string) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, v := range h {
		out[k] = []string{v}
	}
	return out
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// certAuthority 是自签 CA，按需为**任意** CONNECT 目标动态签发叶子证书。
//
// 为什么必须动态签发：最初只预生成 `--hosts` 里列出的那几个 SAN。于是当靶机连到
// 名单外的主机时，客户端证书校验必然失败，而失败发生在 TLS 握手阶段 ——
// **一条记录都不会留下**。排查「池被消耗但看不到出站」时就被这条坑了很久：
// 真相是请求出站到了名单外的主机（`open.bigmodel.cn`），不是没出站。
type certAuthority struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caPEM  []byte

	mu    sync.Mutex
	cache map[string]*tls.Certificate
	seq   int64
}

func newCA() (*certAuthority, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "zcode2api-go dev MITM CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(30 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}
	return &certAuthority{
		caCert: caCert,
		caKey:  caKey,
		caPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		cache:  map[string]*tls.Certificate{},
	}, nil
}

// leafFor 返回 host 对应的叶子证书（首次调用时签发并缓存）。
func (ca *certAuthority) leafFor(host string) (*tls.Certificate, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if c, ok := ca.cache[host]; ok {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	ca.seq++
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2 + ca.seq),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(30 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.caCert, &key.PublicKey, ca.caKey)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{
		Certificate: [][]byte{der, ca.caCert.Raw},
		PrivateKey:  key,
	}
	ca.cache[host] = c
	return c, nil
}
