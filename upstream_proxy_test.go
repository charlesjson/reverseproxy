package main

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHTTPUpstreamProxy(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "HTTP", true: "HTTPS_CONNECT"}[secure], func(t *testing.T) {
			destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Proxy-Authorization") != "" {
					t.Error("proxy credentials leaked to destination")
				}
				_, _ = io.WriteString(w, "via tunnel")
			}))
			defer destination.Close()
			seen := make(chan string, 1)
			forward := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
					t.Error("missing proxy authentication")
				}
				seen <- r.Method
				if secure {
					if r.Method != "CONNECT" || r.Host != mustURL(t, destination.URL).Host {
						t.Errorf("unexpected CONNECT: %s %s", r.Method, r.Host)
						w.WriteHeader(400)
						return
					}
					remote, err := net.Dial("tcp", r.Host)
					if err != nil {
						t.Error(err)
						w.WriteHeader(502)
						return
					}
					defer remote.Close()
					client, buffer, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer client.Close()
					_, _ = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
					_ = buffer.Flush()
					go func() { _, _ = io.Copy(remote, buffer); _ = remote.Close() }()
					_, _ = io.Copy(client, remote)
					return
				}
				if !r.URL.IsAbs() || r.URL.Host != "destination.invalid" || r.URL.Path != "/test" {
					t.Errorf("unexpected proxy request: %s", r.URL)
				}
				_, _ = io.WriteString(w, "via proxy")
			}))
			defer forward.Close()
			proxyURL := mustURL(t, strings.Replace(forward.URL, "://", "://user:pass@", 1))
			target := mustURL(t, "http://destination.invalid")
			if secure {
				target = mustURL(t, destination.URL)
			}
			handler := newProxy(proxyConfig{Target: target, Proxy: proxyURL, ResponseHeaderTimeout: defaultResponseHeaderTimeout}, log.New(io.Discard, "", 0))
			transport := handler.Transport.(*http.Transport)
			if secure {
				transport.TLSClientConfig = &tls.Config{RootCAs: destination.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
			}
			defer transport.CloseIdleConnections()
			reverse := httptest.NewServer(handler)
			defer reverse.Close()
			client := &http.Client{Timeout: 3 * time.Second}
			defer client.CloseIdleConnections()
			response, err := client.Get(reverse.URL + "/test")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			expected := "via proxy"
			if secure {
				expected = "via tunnel"
			}
			if response.StatusCode != 200 || string(body) != expected {
				t.Fatalf("status=%d body=%q", response.StatusCode, body)
			}
			select {
			case <-seen:
			default:
				t.Fatal("upstream proxy was bypassed")
			}
		})
	}
}

func TestSOCKSUpstreamProxy(t *testing.T) {
	for _, scheme := range []string{"socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := listener.Accept()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				read := func(n int) []byte {
					b := make([]byte, n)
					if _, err := io.ReadFull(conn, b); err != nil {
						t.Error(err)
					}
					return b
				}
				greeting := read(2)
				if greeting[0] != 5 {
					t.Error("incorrect SOCKS version")
					return
				}
				read(int(greeting[1]))
				_, _ = conn.Write([]byte{5, 2})
				auth := read(2)
				username := read(int(auth[1]))
				password := read(int(read(1)[0]))
				if auth[0] != 1 || string(username) != "user" || string(password) != "pass" {
					t.Error("incorrect SOCKS authentication")
					return
				}
				_, _ = conn.Write([]byte{1, 0})
				request := read(4)
				if request[0] != 5 || request[1] != 1 || request[3] != 3 {
					t.Errorf("expected domain CONNECT: %v", request)
					return
				}
				domain := read(int(read(1)[0]))
				port := binary.BigEndian.Uint16(read(2))
				if string(domain) != "destination.invalid" || port != 80 {
					t.Errorf("unexpected destination: %s:%d", domain, port)
					return
				}
				_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
				httpRequest, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					t.Error(err)
					return
				}
				defer httpRequest.Body.Close()
				if httpRequest.URL.Path != "/test" || httpRequest.Header.Get("Proxy-Authorization") != "" {
					t.Error("incorrect tunneled request")
				}
				_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 9\r\nConnection: close\r\n\r\nvia socks")
			}()
			proxyURL, err := parseProxy(scheme + "://user:pass@" + listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			handler := newProxy(proxyConfig{Target: mustURL(t, "http://destination.invalid"), Proxy: proxyURL, ResponseHeaderTimeout: defaultResponseHeaderTimeout}, log.New(io.Discard, "", 0))
			defer handler.Transport.(*http.Transport).CloseIdleConnections()
			reverse := httptest.NewServer(handler)
			defer reverse.Close()
			client := &http.Client{Timeout: 3 * time.Second}
			defer client.CloseIdleConnections()
			response, err := client.Get(reverse.URL + "/test")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != 200 || string(body) != "via socks" {
				t.Fatalf("status=%d body=%q", response.StatusCode, body)
			}
			<-done
		})
	}
}

func TestProxyConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("proxies:\n  - listen: ':7789'\n    target: 'https://example.com'\n    proxy: 'socks5://localhost:1080'\n  - listen: ':7790'\n    target: 'https://example.org'\n    proxy: 'http://localhost:3128'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig([]string{"-config", path}, io.Discard)
	if err != nil || cfg.Proxies[0].Proxy.String() != "socks5://localhost:1080" || cfg.Proxies[1].Proxy.String() != "http://localhost:3128" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	if _, err := loadConfig([]string{"-config", path, "-proxy", "http://localhost:3128"}, io.Discard); err == nil {
		t.Error("accepted mixed multi-target config and proxy flag")
	}
	cfg, err = loadConfig([]string{"-proxy", "http://localhost:3128", ":7789", "https://example.com"}, io.Discard)
	if err != nil || cfg.Proxies[0].Proxy.String() != "http://localhost:3128" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	for _, raw := range []string{"socks4://localhost:1080", "http://", "http://user:secret@localhost:99999", "http://localhost/path", "http://localhost?x=y"} {
		_, err := parseProxy(raw)
		if err == nil {
			t.Errorf("accepted %q", raw)
		} else if strings.Contains(err.Error(), "secret") {
			t.Error("credentials exposed in error")
		}
	}
	t.Setenv("HTTP_PROXY", "http://environment.invalid:3128")
	transport := newTestProxy(t, mustURL(t, "http://example.com")).Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Error("direct connection unexpectedly uses environment proxy")
	}
}
