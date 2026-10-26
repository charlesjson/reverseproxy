package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRawModeConfiguration(t *testing.T) {
	list := "proxies:\n  - listen: ':7789'\n    target: 'http://example.com'\n    mode: raw\n  - listen: ':7790'\n    target: 'https://example.org'\n    mode: http\n"
	for _, tc := range []struct {
		name, yaml string
		args       []string
		wantErr    bool
	}{
		{"CLI", "", []string{"-mode", "raw", ":7789", "http://example.com"}, false},
		{"HTTPS", "", []string{"-mode", "raw", ":7789", "https://example.com/"}, false},
		{"YAML single", "mode: raw\nlisten: ':7789'\ntarget: 'http://example.com'\n", nil, false},
		{"mixed listeners", list, nil, false},
		{"upstream proxy", "", []string{"-mode", "raw", "-proxy", "socks5://localhost:1080", ":7789", "http://example.com"}, false},
		{"unknown mode", "", []string{"-mode", "other", ":7789", "http://example.com"}, true},
		{"path prefix", "", []string{"-mode", "raw", ":7789", "http://example.com/base"}, true},
		{"query", "", []string{"-mode", "raw", ":7789", "http://example.com?key=value"}, true},
		{"empty query", "", []string{"-mode", "raw", ":7789", "http://example.com?"}, true},
		{"invalid port", "", []string{"-mode", "raw", ":7789", "http://example.com:99999"}, true},
		{"mixed CLI mode", list, []string{"-mode", "raw"}, true},
		{"mixed empty mode", list + "mode: ''\n", nil, true},
		{"mixed null mode", list + "mode: null\n", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := tc.args
			if tc.yaml != "" {
				path := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(path, []byte(tc.yaml), 0600); err != nil {
					t.Fatal(err)
				}
				args = append([]string{"-config", path}, args...)
			}
			_, err := loadConfig(args, io.Discard)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestRawPreservesRequestAndResponseBytes(t *testing.T) {
	request := "POST /a%2fb?z=1&a=2 HTTP/1.1\r\nhOsT: preserved.example\r\nX-Repeat:\tone\r\nx-repeat: two\r\nTransfer-Encoding: chunked\r\nTrailer: X-End\r\n\r\n3;name=value\r\nabc\r\n0\r\nX-End: final\r\n\r\nGET /next HTTP/1.1\r\nHost: second.example\r\nConnection: close\r\n\r\n"
	response := "HTTP/1.1 201 RAW\r\nx-Same: one\r\nX-Same: two\r\nTransfer-Encoding: chunked\r\n\r\n3;tag=x\r\nxyz\r\n0\r\nX-Finish: done\r\n\r\nHTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n"
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upstream.Close() })
	done := make(chan error, 1)
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		// 等待客户端半关闭后再返回，保证透传不会提前关闭反向通道。
		got, err := io.ReadAll(conn)
		if err == nil && string(got) != request {
			err = fmt.Errorf("upstream request changed: %q", got)
		}
		if err == nil {
			_, err = io.WriteString(conn, response)
		}
		done <- err
	}()
	cfg, err := loadConfig([]string{"-mode", "raw", "127.0.0.1:0", "http://" + upstream.Addr().String()}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	servers := startTestProxyServers(t, cfg.Proxies, nil)
	conn, err := net.DialTimeout("tcp", servers[0].listener.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(conn)
	if err != nil || string(got) != response {
		t.Errorf("response=%q err=%v", got, err)
	}
	if err := <-done; err != nil {
		t.Error(err)
	}
}

func startTestProxyServers(t *testing.T, entries []proxyConfig, configure func([]proxyServer)) []proxyServer {
	t.Helper()
	logger := log.New(io.Discard, "", 0)
	servers, err := bindProxies(entries, logger)
	if err != nil {
		t.Fatal(err)
	}
	if configure != nil {
		configure(servers)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveProxies(ctx, servers, logger) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("proxy shutdown timed out")
		}
	})
	return servers
}

func TestRawHTTPSVerifiesTLSAndPreservesBytes(t *testing.T) {
	serverTLS, roots := rawTestTLSConfig(t)
	for _, trusted := range []bool{true, false} {
		t.Run(fmt.Sprintf("trusted=%v", trusted), func(t *testing.T) {
			listener, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			request := "GET /raw%2f?z=2&a=1 HTTP/1.1\r\nhost: unchanged.example\r\nx-Custom:\tvalue\r\n\r\n"
			response := "HTTP/1.1 200 RawTLS\r\nx-case: untouched\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				secured := conn.(*tls.Conn)
				if err = secured.Handshake(); !trusted {
					if err == nil {
						err = fmt.Errorf("untrusted TLS connection was accepted")
					} else {
						err = nil
					}
					done <- err
					return
				}
				if err == nil {
					state := secured.ConnectionState()
					if state.ServerName != "localhost" || state.NegotiatedProtocol != "http/1.1" {
						err = fmt.Errorf("SNI=%q ALPN=%q", state.ServerName, state.NegotiatedProtocol)
					}
				}
				if err == nil {
					var got []byte
					got, err = io.ReadAll(secured)
					if err == nil && string(got) != request {
						err = fmt.Errorf("TLS request changed: %q", got)
					}
				}
				if err == nil {
					_, err = io.WriteString(secured, response)
				}
				done <- err
			}()
			_, port, _ := net.SplitHostPort(listener.Addr().String())
			entries := []proxyConfig{{Mode: modeRaw, Listen: "127.0.0.1:0", Target: mustURL(t, "https://localhost:"+port)}}
			servers := startTestProxyServers(t, entries, func(servers []proxyServer) {
				if trusted {
					servers[0].server.(*rawServer).dialer.TLSClientConfig = &tls.Config{RootCAs: roots}
				}
			})
			conn, err := net.DialTimeout("tcp", servers[0].listener.Addr().String(), 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
			_, _ = io.WriteString(conn, request)
			_ = conn.(*net.TCPConn).CloseWrite()
			got, readErr := io.ReadAll(conn)
			if trusted && (readErr != nil || string(got) != response) {
				t.Errorf("response=%q err=%v", got, readErr)
			}
			if !trusted && len(got) != 0 {
				t.Errorf("untrusted target returned data: %q", got)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(4 * time.Second):
				t.Error("upstream did not receive a TLS connection")
			}
		})
	}
}

func rawTestTLSConfig(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, NextProtos: []string{"h2", "http/1.1"}}, roots
}

func TestRawShutdownClosesConnectionsAndHandshakes(t *testing.T) {
	for _, mode := range []string{"active", "TLS handshake", "HTTP CONNECT", "SOCKS handshake"} {
		t.Run(mode, func(t *testing.T) {
			listener := rawTestListener(t, false, nil)
			accepted := make(chan net.Conn, 1)
			go func() {
				conn, err := listener.Accept()
				if err == nil {
					accepted <- conn
				}
			}()
			entry := proxyConfig{Mode: modeRaw, Listen: "127.0.0.1:0", Target: mustURL(t, "http://"+listener.Addr().String())}
			switch mode {
			case "TLS handshake":
				entry.Target.Scheme = "https"
			case "HTTP CONNECT":
				entry.Proxy = mustURL(t, "http://"+listener.Addr().String())
				entry.Target = mustURL(t, "http://destination.invalid")
			case "SOCKS handshake":
				entry.Proxy = mustURL(t, "socks5://"+listener.Addr().String())
				entry.Target = mustURL(t, "http://destination.invalid")
			}
			servers := startTestProxyServers(t, []proxyConfig{entry}, nil)
			client, err := net.DialTimeout("tcp", servers[0].listener.Addr().String(), 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			var remote net.Conn
			select {
			case remote = <-accepted:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream connection was not established")
			}
			defer remote.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := servers[0].server.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			for _, conn := range []net.Conn{client, remote} {
				_ = conn.SetReadDeadline(time.Now().Add(time.Second))
				_, err := io.ReadAll(conn)
				if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
					t.Errorf("connection remained open after shutdown: %v", err)
				}
			}
		})
	}
}

func TestHTTPAndRawListenersRunTogether(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "normal "+r.URL.Path)
	}))
	defer upstream.Close()
	rawUpstream := rawTestListener(t, false, nil)
	rawRequest := "GET /raw HTTP/1.1\r\nhOsT: preserved.example\r\n\r\n"
	rawResponse := "HTTP/1.1 200 RAW\r\nContent-Length: 3\r\n\r\nraw"
	rawDone := rawTestExchange(rawUpstream, rawRequest, rawResponse)
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := fmt.Sprintf("proxies:\n  - mode: http\n    listen: '127.0.0.1:0'\n    target: '%s/base'\n  - mode: raw\n    listen: 'localhost:0'\n    target: 'http://%s'\n", upstream.URL, rawUpstream.Addr())
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig([]string{"-config", path}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	servers := startTestProxyServers(t, cfg.Proxies, nil)
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Get("http://" + servers[0].listener.Addr().String() + "/hello")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "normal /base/hello" {
		t.Fatalf("normal proxy body=%q err=%v", body, err)
	}
	assertRawClientExchange(t, servers[1].listener.Addr().String(), rawRequest, rawResponse)
	awaitRawResult(t, rawDone)
}

func TestRawUpgradeStreamsBeforeClientEOF(t *testing.T) {
	request := "GET /chat HTTP/1.1\r\nHost: preserved.example\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
	response := "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n"
	clientFrame := string([]byte{0x81, 0x82, 1, 2, 3, 4, 'h' ^ 1, 'i' ^ 2})
	serverFrame := string([]byte{0x81, 0x02, 'o', 'k'})
	upstream := rawTestListener(t, false, nil)
	done := make(chan error, 1)
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		for _, pair := range [][2]string{{request, response}, {clientFrame, serverFrame}} {
			buffer := make([]byte, len(pair[0]))
			if _, err := io.ReadFull(conn, buffer); err != nil || string(buffer) != pair[0] {
				done <- fmt.Errorf("upgraded request changed or stalled: %v", err)
				return
			}
			if _, err := io.WriteString(conn, pair[1]); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	servers := startTestProxyServers(t, []proxyConfig{{Mode: modeRaw, Listen: "127.0.0.1:0", Target: mustURL(t, "http://"+upstream.Addr().String())}}, nil)
	conn, err := net.DialTimeout("tcp", servers[0].listener.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
	for _, pair := range [][2]string{{request, response}, {clientFrame, serverFrame}} {
		if _, err := io.WriteString(conn, pair[0]); err != nil {
			t.Fatal(err)
		}
		buffer := make([]byte, len(pair[1]))
		if _, err := io.ReadFull(conn, buffer); err != nil || string(buffer) != pair[1] {
			t.Fatalf("upgraded response changed or stalled: %q err=%v", buffer, err)
		}
	}
	awaitRawResult(t, done)
}
