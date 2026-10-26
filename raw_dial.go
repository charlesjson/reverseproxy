package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

const rawConnectTimeout = 30 * time.Second

type rawDialer struct {
	cfg             proxyConfig
	TLSClientConfig *tls.Config
}

func (d *rawDialer) DialContext(ctx context.Context) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, rawConnectTimeout)
	defer cancel()
	endpoint := d.cfg.Target
	if d.cfg.Proxy != nil {
		endpoint = d.cfg.Proxy
	}
	dialer := net.Dialer{Timeout: rawConnectTimeout, KeepAlive: 30 * time.Second}
	base, err := dialer.DialContext(ctx, "tcp", endpointAddress(endpoint))
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = base.Close() })
	ready := false
	defer func() {
		stop()
		if !ready {
			_ = base.Close()
		}
	}()
	deadline, _ := ctx.Deadline()
	if err := base.SetDeadline(deadline); err != nil {
		return nil, err
	}
	var conn net.Conn = base
	if d.cfg.Proxy != nil {
		if endpoint.Scheme == "https" {
			conn, err = d.secure(ctx, conn, endpoint.Hostname())
			if err != nil {
				return nil, err
			}
		}
		switch endpoint.Scheme {
		case "http", "https":
			conn, err = connectHTTPTunnel(conn, endpoint, endpointAddress(d.cfg.Target))
		case "socks5", "socks5h":
			err = connectSOCKSTunnel(conn, endpoint, d.cfg.Target)
		default:
			err = fmt.Errorf("不支持的 raw 上游代理协议")
		}
		if err != nil {
			return nil, err
		}
	}
	if d.cfg.Target.Scheme == "https" {
		conn, err = d.secure(ctx, conn, d.cfg.Target.Hostname())
		if err != nil {
			return nil, err
		}
	}
	if !stop() || ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	ready = true
	return conn, nil
}

func connectHTTPTunnel(conn net.Conn, proxy *url.URL, address string) (net.Conn, error) {
	request := &http.Request{Method: http.MethodConnect, URL: &url.URL{Host: address}, Host: address, Header: make(http.Header)}
	if proxy.User != nil {
		password, _ := proxy.User.Password()
		credentials := proxy.User.Username() + ":" + password
		request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(credentials)))
	}
	if err := request.Write(conn); err != nil {
		return nil, fmt.Errorf("发送 CONNECT: %w", err)
	}
	// 只限制握手响应头；隧道数据随后直接读取连接，不受这个字节数限制。
	reader := bufio.NewReader(io.LimitReader(conn, 64*1024))
	for {
		response, err := http.ReadResponse(reader, request)
		if err != nil {
			return nil, fmt.Errorf("读取 CONNECT 响应: %w", err)
		}
		if response.StatusCode == http.StatusSwitchingProtocols {
			return nil, fmt.Errorf("CONNECT 不支持 HTTP 101 协议升级")
		}
		if response.StatusCode >= 100 && response.StatusCode < 200 {
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, fmt.Errorf("上游代理拒绝 CONNECT: HTTP %d", response.StatusCode)
		}
		break
	}
	// 成功的 CONNECT 响应之后就是隧道，不能关闭或消费 response.Body。
	return &bufferedTunnelConn{Conn: conn, reader: reader}, nil
}

type bufferedTunnelConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedTunnelConn) Read(buffer []byte) (int, error) {
	if c.reader.Buffered() != 0 {
		return c.reader.Read(buffer)
	}
	return c.Conn.Read(buffer)
}

func (c *bufferedTunnelConn) CloseWrite() error { return closeWrite(c.Conn) }
func (c *bufferedTunnelConn) NetConn() net.Conn { return c.Conn }

func (d *rawDialer) secure(ctx context.Context, conn net.Conn, hostname string) (net.Conn, error) {
	cfg := &tls.Config{}
	if d.TLSClientConfig != nil {
		cfg = d.TLSClientConfig.Clone()
	}
	cfg.ServerName = hostname
	// 原始 HTTP/1.x 字节不能在协商为 HTTP/2 后直接发送。
	cfg.NextProtos = []string{"http/1.1"}
	secured := tls.Client(conn, cfg)
	if err := secured.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("TLS 握手失败: %w", err)
	}
	return secured, nil
}

func endpointAddress(endpoint *url.URL) string {
	port := endpoint.Port()
	if port == "" {
		switch endpoint.Scheme {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(endpoint.Hostname(), port)
}
