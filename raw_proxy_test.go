package main

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRawViaHTTPProxy(t *testing.T) {
	serverTLS, roots := rawTestTLSConfig(t)
	for _, proxyScheme := range []string{"http", "https"} {
		for _, targetScheme := range []string{"http", "https"} {
			t.Run(proxyScheme+"_to_"+targetScheme, func(t *testing.T) {
				request := "GET /x%2f?b=2&a=1 HTTP/1.1\r\nhOsT: keep.example\r\nX-Dup: one\r\nx-dup: two\r\n\r\n"
				response := "HTTP/1.1 200 RawTunnel\r\nx-Case: unchanged\r\nContent-Length: 2\r\n\r\nok"
				upstream := rawTestListener(t, targetScheme == "https", serverTLS)
				upstreamDone := rawTestExchange(upstream, request, response)
				forward := rawTestListener(t, proxyScheme == "https", serverTLS)
				proxyDone := make(chan error, 1)
				go func() {
					conn, err := forward.Accept()
					if err != nil {
						proxyDone <- err
						return
					}
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
					reader := bufio.NewReader(conn)
					req, err := http.ReadRequest(reader)
					if err != nil {
						proxyDone <- err
						return
					}
					defer req.Body.Close()
					if req.Method != "CONNECT" || req.Host != upstream.Addr().String() || req.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
						proxyDone <- fmt.Errorf("unexpected CONNECT or authentication: method=%s host=%s", req.Method, req.Host)
						return
					}
					remote, err := net.DialTimeout("tcp", upstream.Addr().String(), 3*time.Second)
					if err != nil {
						proxyDone <- err
						return
					}
					defer remote.Close()
					_ = remote.SetDeadline(time.Now().Add(4 * time.Second))
					if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
						proxyDone <- err
						return
					}
					uploadDone := make(chan error, 1)
					go func() {
						_, err := io.Copy(remote, reader)
						_ = remote.(*net.TCPConn).CloseWrite()
						uploadDone <- err
					}()
					_, err = io.Copy(conn, remote)
					if closeWriter, ok := conn.(interface{ CloseWrite() error }); ok {
						_ = closeWriter.CloseWrite()
					}
					uploadErr := <-uploadDone
					if err == nil {
						err = uploadErr
					}
					proxyDone <- err
				}()
				cfg, err := loadConfig([]string{"-mode", "raw", "-proxy", proxyScheme + "://user:pass@" + forward.Addr().String(), "127.0.0.1:0", targetScheme + "://" + upstream.Addr().String()}, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				servers := startTestProxyServers(t, cfg.Proxies, func(servers []proxyServer) {
					servers[0].server.(*rawServer).dialer.TLSClientConfig = &tls.Config{RootCAs: roots}
				})
				assertRawClientExchange(t, servers[0].listener.Addr().String(), request, response)
				awaitRawResult(t, upstreamDone)
				awaitRawResult(t, proxyDone)
			})
		}
	}
}

func TestRawViaSOCKSProxy(t *testing.T) {
	serverTLS, roots := rawTestTLSConfig(t)
	for _, tc := range []struct {
		name, scheme, target, host string
		auth                       bool
		addressType, replyType     byte
		port                       uint16
	}{
		{"domain", "socks5", "http://destination.invalid", "destination.invalid", false, 3, 1, 80},
		{"authenticated", "socks5", "http://destination.invalid", "destination.invalid", true, 3, 1, 80},
		{"socks5h", "socks5h", "http://destination.invalid", "destination.invalid", true, 3, 3, 80},
		{"IPv4", "socks5", "http://192.0.2.1:8080", "192.0.2.1", false, 1, 1, 8080},
		{"IPv6", "socks5h", "http://[2001:db8::1]:8080", "2001:db8::1", false, 4, 4, 8080},
		{"HTTPS", "socks5h", "https://localhost", "localhost", true, 3, 1, 443},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := "GET /raw HTTP/1.1\r\nhOsT: unchanged.example\r\nX-Dup: one\r\nx-dup: two\r\n\r\n"
			response := "HTTP/1.1 200 SOCKS\r\nContent-Length: 2\r\n\r\nok"
			listener := rawTestListener(t, false, nil)
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
				done <- func() error {
					read := func(n int) ([]byte, error) { b := make([]byte, n); _, err := io.ReadFull(conn, b); return b, err }
					greeting, err := read(2)
					if err != nil || greeting[0] != 5 {
						return fmt.Errorf("invalid greeting %v: %v", greeting, err)
					}
					if _, err := read(int(greeting[1])); err != nil {
						return err
					}
					method := byte(0)
					if tc.auth {
						method = 2
					}
					if _, err := conn.Write([]byte{5, method}); err != nil {
						return err
					}
					if tc.auth {
						auth, err := read(2)
						if err != nil || auth[0] != 1 {
							return fmt.Errorf("invalid authentication version: %v", err)
						}
						user, err := read(int(auth[1]))
						if err != nil || string(user) != "user" {
							return fmt.Errorf("invalid authentication username")
						}
						length, err := read(1)
						if err != nil {
							return err
						}
						secret, err := read(int(length[0]))
						if err != nil || string(secret) != "pass" {
							return fmt.Errorf("invalid authentication secret")
						}
						if _, err := conn.Write([]byte{1, 0}); err != nil {
							return err
						}
					}
					header, err := read(4)
					if err != nil || header[0] != 5 || header[1] != 1 || header[2] != 0 || header[3] != tc.addressType {
						return fmt.Errorf("invalid SOCKS CONNECT %v: %v", header, err)
					}
					length := 4
					if tc.addressType == 4 {
						length = 16
					} else if tc.addressType == 3 {
						size, err := read(1)
						if err != nil {
							return err
						}
						length = int(size[0])
					}
					address, err := read(length)
					if err != nil {
						return err
					}
					host := string(address)
					if tc.addressType != 3 {
						host = net.IP(address).String()
					}
					port, err := read(2)
					if err != nil || host != tc.host || binary.BigEndian.Uint16(port) != tc.port {
						return fmt.Errorf("unexpected target %s: %v", host, err)
					}
					reply := []byte{5, 0, 0, tc.replyType}
					switch tc.replyType {
					case 1:
						reply = append(reply, 127, 0, 0, 1)
					case 3:
						reply = append(reply, 1, 'x')
					case 4:
						reply = append(reply, make([]byte, 16)...)
					}
					if _, err := conn.Write(append(reply, 0, 80)); err != nil {
						return err
					}
					var tunnel net.Conn = conn
					if strings.HasPrefix(tc.target, "https:") {
						secured := tls.Server(conn, serverTLS)
						defer secured.Close()
						tunnel = secured
					}
					got, err := io.ReadAll(tunnel)
					if err != nil || string(got) != request {
						return fmt.Errorf("raw SOCKS request changed: %q err=%v", got, err)
					}
					_, err = io.WriteString(tunnel, response)
					return err
				}()
			}()
			credentials := ""
			if tc.auth {
				credentials = "user:pass@"
			}
			cfg, err := loadConfig([]string{"-mode", "raw", "-proxy", tc.scheme + "://" + credentials + listener.Addr().String(), "127.0.0.1:0", tc.target}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			servers := startTestProxyServers(t, cfg.Proxies, func(servers []proxyServer) {
				servers[0].server.(*rawServer).dialer.TLSClientConfig = &tls.Config{RootCAs: roots}
			})
			assertRawClientExchange(t, servers[0].listener.Addr().String(), request, response)
			awaitRawResult(t, done)
		})
	}
}

func TestRawCONNECTKeepsBufferedAndLargeResponses(t *testing.T) {
	request := "GET /raw HTTP/1.1\r\nHost: keep.example\r\n\r\n"
	body := strings.Repeat("raw-body\x00", 10000)
	response := fmt.Sprintf("HTTP/1.1 200 Large\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	forward := rawTestListener(t, false, nil)
	done := make(chan error, 1)
	go func() {
		conn, err := forward.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
		reader := bufio.NewReader(conn)
		connect, err := http.ReadRequest(reader)
		if err != nil {
			done <- err
			return
		}
		defer connect.Body.Close()
		// 同一次写入包含 CONNECT 响应头和隧道数据，覆盖 bufio 预读。
		_, err = io.WriteString(conn, "HTTP/1.1 200 Connected\r\nContent-Length: 123\r\n\r\n"+response)
		if err == nil {
			var got []byte
			got, err = io.ReadAll(reader)
			if err == nil && string(got) != request {
				err = fmt.Errorf("request changed: %q", got)
			}
		}
		done <- err
	}()
	entries := []proxyConfig{{Mode: modeRaw, Listen: "127.0.0.1:0", Target: mustURL(t, "http://destination.invalid"), Proxy: mustURL(t, "http://"+forward.Addr().String())}}
	servers := startTestProxyServers(t, entries, nil)
	assertRawClientExchange(t, servers[0].listener.Addr().String(), request, response)
	awaitRawResult(t, done)
}

func TestRawCONNECTAcceptsInformationalResponses(t *testing.T) {
	request := "GET /raw HTTP/1.1\r\nHost: keep.example\r\n\r\n"
	response := "HTTP/1.1 200 RawFinal\r\nContent-Length: 2\r\n\r\nok"
	forward := rawTestListener(t, false, nil)
	done := make(chan error, 1)
	go func() {
		conn, err := forward.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(conn)
		connect, err := http.ReadRequest(reader)
		if err != nil {
			done <- err
			return
		}
		connect.Body.Close()
		// 临时响应、最终响应和隧道数据共用同一个输入缓冲区。
		_, err = io.WriteString(conn, "HTTP/1.1 103 Early Hints\r\nLink: </hint>\r\n\r\nHTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 200 Connected\r\n\r\n"+response)
		if err == nil {
			var got []byte
			got, err = io.ReadAll(reader)
			if err == nil && string(got) != request {
				err = fmt.Errorf("request changed: %q", got)
			}
		}
		done <- err
	}()
	entries := []proxyConfig{{Mode: modeRaw, Listen: "127.0.0.1:0", Target: mustURL(t, "http://destination.invalid"), Proxy: mustURL(t, "http://"+forward.Addr().String())}}
	servers := startTestProxyServers(t, entries, nil)
	assertRawClientExchange(t, servers[0].listener.Addr().String(), request, response)
	awaitRawResult(t, done)
}

func TestRawProxyFailureDoesNotFallBack(t *testing.T) {
	for _, scheme := range []string{"http", "socks5"} {
		t.Run(scheme, func(t *testing.T) {
			var directConnections atomic.Int32
			upstream := httptest.NewUnstartedServer(http.NotFoundHandler())
			upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					directConnections.Add(1)
				}
			}
			upstream.Start()
			defer upstream.Close()
			forward := rawTestListener(t, false, nil)
			done := make(chan error, 1)
			go func() {
				conn, err := forward.Accept()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				if scheme == "http" {
					request, err := http.ReadRequest(bufio.NewReader(conn))
					if err != nil {
						done <- err
						return
					}
					request.Body.Close()
					_, err = io.WriteString(conn, "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n")
					done <- err
					return
				}
				var greeting [2]byte
				if _, err := io.ReadFull(conn, greeting[:]); err != nil {
					done <- err
					return
				}
				if _, err := io.CopyN(io.Discard, conn, int64(greeting[1])); err != nil {
					done <- err
					return
				}
				_, err = conn.Write([]byte{5, 255})
				done <- err
			}()
			entries := []proxyConfig{{Mode: modeRaw, Listen: "127.0.0.1:0", Target: mustURL(t, upstream.URL), Proxy: mustURL(t, scheme+"://"+forward.Addr().String())}}
			servers := startTestProxyServers(t, entries, nil)
			conn, err := net.DialTimeout("tcp", servers[0].listener.Addr().String(), 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			_, _ = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
			got, err := io.ReadAll(conn)
			if len(got) != 0 {
				t.Errorf("raw mode injected a response or bypassed the proxy: %q", got)
			}
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Error("connection stayed open after proxy failure")
			}
			awaitRawResult(t, done)
			if directConnections.Load() != 0 {
				t.Error("connection fell back to the destination")
			}
		})
	}
}

func rawTestListener(t *testing.T, secure bool, cfg *tls.Config) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if secure {
		listener = tls.NewListener(listener, cfg)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func rawTestExchange(listener net.Listener, request, response string) <-chan error {
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
		got, err := io.ReadAll(conn)
		if err == nil && string(got) != request {
			err = fmt.Errorf("raw request changed: %q", got)
		}
		if err == nil {
			_, err = io.WriteString(conn, response)
		}
		done <- err
	}()
	return done
}

func assertRawClientExchange(t *testing.T, address, request, response string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(conn)
	if err != nil || string(got) != response {
		t.Fatalf("raw response=%q err=%v", got, err)
	}
}

func awaitRawResult(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(5 * time.Second):
		t.Error("upstream did not complete")
	}
}
