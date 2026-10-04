package cdp

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
)

// wrapTLS 把裸连接升级为 TLS（`wss://` 用）。
//
// 实际部署里 CDP 的 `webSocketDebuggerUrl` 恒为 `ws://127.0.0.1:<port>/...`，
// 这条路径不会被走到；保留它只是为了让 dialWS 对 scheme 的处理是完整的
// （而不是「遇到 wss 就报未实现」）。
func wrapTLS(ctx context.Context, raw net.Conn, serverName string) (net.Conn, error) {
	cfg := tlsConfigFor(serverName)
	tc := tls.Client(raw, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("TLS 握手失败: %w", err)
	}
	return tc, nil
}

// tlsConfigFor 用系统根证书池建 TLS 配置。
func tlsConfigFor(serverName string) *tls.Config {
	return &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
}
