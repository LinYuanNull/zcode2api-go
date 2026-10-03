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
	Method     string            `json:"method"`      // 空 = 任意
	PathSuffix string            `json:"path_suffix"` // 空 = 任意；否则要求 path 以此结尾
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	// Chunks 非空时按 SSE 分块发送（每个元素一个 `data:` 载荷，自动补 `data: ` 与空行）。
	Chunks []string `json:"chunks"`
}

var (
	mode      = flag.String("mode", "capture", "capture | fake")
	listen    = flag.String("listen", "127.0.0.1:0", "监听地址")
	caOut     = flag.String("ca-out", "", "把 CA 证书 PEM 写到这个路径（给 SSL_CERT_FILE）")
	logPath   = flag.String("log", "", "把捕获记录追加为 JSON Lines")
	hostsFlag = flag.String("hosts", "zcode.z.ai,api.z.ai", "叶子证书的 SAN（逗号分隔）")
	rulesPath = flag.String("rules", "", "fake 模式的规则文件（JSON 数组）")
	upstream  = flag.String("upstream", "", "capture 模式下强制转发到该 host:port（默认按 CONNECT 目标）")
)

func main() {
	flag.Parse()
	hosts := splitCSV(*hostsFlag)

	caPEM, leaf, err := makeCerts(hosts)
	if err != nil {
		log.Fatalf("生成证书失败: %v", err)
	}
	if *caOut != "" {
		if err := os.WriteFile(*caOut, caPEM, 0o644); err != nil {
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

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("监听失败: %v", err)
	}
	fmt.Printf("LISTEN %s\n", ln.Addr().String())
	os.Stdout.Sync()

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{leaf},
		NextProtos:   []string{"http/1.1"},
		MinVersion:   tls.VersionTLS12,
	}

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "this is a CONNECT proxy", http.StatusMethodNotAllowed)
			return
		}
		handleConnect(w, r, tlsCfg, rules, emit)
	})}
	log.Fatal(srv.Serve(ln))
}

func handleConnect(w http.ResponseWriter, r *http.Request, tlsCfg *tls.Config, rules []rule, emit func(record)) {
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

	tlsConn := tls.Server(conn, tlsCfg)
	if err := tlsConn.Handshake(); err != nil {
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
		rl := matchRule(rules, req)
		if rl == nil {
			rec.RespStatus = 599
			rec.RespBodyText = "no matching rule"
			writeResponse(client, 599, map[string]string{"Content-Type": "text/plain"}, []byte("no matching rule"), nil)
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
		if len(rl.Chunks) > 0 {
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

func matchRule(rules []rule, req *http.Request) *rule {
	p := req.URL.Path
	for i := range rules {
		r := &rules[i]
		if r.Method != "" && !strings.EqualFold(r.Method, req.Method) {
			continue
		}
		if r.PathSuffix != "" && !strings.HasSuffix(p, r.PathSuffix) {
			continue
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

// makeCerts 生成自签 CA 与叶子证书，返回 CA 的 PEM 与叶子 tls.Certificate。
func makeCerts(hosts []string) ([]byte, tls.Certificate, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, tls.Certificate{}, err
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
		return nil, tls.Certificate{}, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: hosts[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(30 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     hosts,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	leaf := tls.Certificate{
		Certificate: [][]byte{leafDER, caDER},
		PrivateKey:  leafKey,
	}
	return caPEM, leaf, nil
}
