package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProxyRoundTrip(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || r.URL.EscapedPath() != "/base/a%2Fb" || r.URL.Query().Get("q") != "hello" || r.URL.Query().Get("key") != "value" || string(body) != "payload" {
			t.Errorf("unexpected forwarded request: %s %s body=%q", r.Method, r.URL, body)
		}
		if r.Header.Get("X-Forwarded-For") != "127.0.0.1" || r.Header.Get("X-Forwarded-Proto") != "http" || r.Header.Get("X-Forwarded-Host") == "forged" {
			t.Errorf("incorrect forwarding headers: %v", r.Header)
		}
		if r.Host != r.Header.Get("X-Expected-Host") {
			t.Errorf("unexpected Host: %s", r.Host)
		}
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("upstream response"))
	}))
	defer upstream.Close()
	target, err := parseTarget(upstream.URL + "/base?key=value")
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(newTestProxy(t, target))
	defer proxy.Close()
	req, _ := http.NewRequest("POST", proxy.URL+"/a%2Fb?q=hello", strings.NewReader("payload"))
	req.Header.Set("X-Expected-Host", target.Host)
	req.Header.Set("X-Forwarded-For", "forged")
	req.Header.Set("X-Forwarded-Host", "forged")
	req.Header.Set("X-Forwarded-Proto", "forged")
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 201 || resp.Header.Get("X-Upstream") != "yes" || string(body) != "upstream response" {
		t.Fatalf("incorrect response: %d %v %q", resp.StatusCode, resp.Header, body)
	}
}

func TestUnavailableUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	upstream.Close()
	target, _ := parseTarget(upstream.URL)
	response := httptest.NewRecorder()
	newTestProxy(t, target).ServeHTTP(response, httptest.NewRequest("GET", "http://proxy/test", nil))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("listen: '0.0.0.0:7789'\ntarget: 'https://www.google.com'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"0.0.0.0:7789", "https://www.google.com"},
		{"-listen", "0.0.0.0:7789", "-target", "https://www.google.com"},
		{"-config", path},
	} {
		cfg, err := loadConfig(args, io.Discard)
		if err != nil || cfg.Proxies[0].Listen != "0.0.0.0:7789" || cfg.Proxies[0].Target.String() != "https://www.google.com" {
			t.Fatalf("args=%v cfg=%+v err=%v", args, cfg, err)
		}
	}
	cfg, err := loadConfig([]string{"-config", path, "-listen", "127.0.0.1:8080"}, io.Discard)
	if err != nil || cfg.Proxies[0].Listen != "127.0.0.1:8080" {
		t.Fatalf("override failed: %+v %v", cfg, err)
	}
	for _, args := range [][]string{nil, {"7789", "https://example.com"}, {"localhost:7789", "ftp://example.com"}, {"localhost:7789"}, {"-listen", "localhost:8080", "localhost:7789", "https://example.com"}} {
		if _, err := loadConfig(args, io.Discard); err == nil {
			t.Errorf("accepted invalid arguments: %v", args)
		}
	}
	if err := os.WriteFile(path, []byte("listen: ':7789'\ntarget: 'https://example.com'\ntypo: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig([]string{"-config", path}, io.Discard); err == nil {
		t.Error("accepted unknown YAML field")
	}
}

func TestMultiConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	valid := "proxies:\n  - listen: '127.0.0.1:7789'\n    target: 'https://example.com'\n  - listen: '127.0.0.1:7790'\n    target: 'https://example.org'\n"
	write := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(valid)
	cfg, err := loadConfig([]string{"-config", path}, io.Discard)
	if err != nil || len(cfg.Proxies) != 2 || cfg.Proxies[1].Target.String() != "https://example.org" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	for _, args := range [][]string{
		{"-config", path, "-listen", ":8080"},
		{"-config", path, ":8080", "https://example.net"},
	} {
		if _, err := loadConfig(args, io.Discard); err == nil {
			t.Errorf("accepted mixed args: %v", args)
		}
	}
	for _, data := range []string{
		"proxies: []\n",
		valid + "listen: ':8080'\n",
		strings.Replace(valid, "7790", "7789", 1),
		strings.Replace(valid, "    target: 'https://example.org'\n", "", 1),
		strings.Replace(valid, "https://example.org", "ftp://example.org", 1),
		strings.Replace(valid, "    target: 'https://example.org'", "    typo: 'https://example.org'", 1),
	} {
		write(data)
		if _, err := loadConfig([]string{"-config", path}, io.Discard); err == nil {
			t.Errorf("accepted invalid config: %s", data)
		}
	}
}

func TestExplicitEmptyConfigFieldsAreRejected(t *testing.T) {
	list := "proxies:\n  - listen: '127.0.0.1:7789'\n    target: 'https://example.com'\n"
	single := "listen: ':7789'\ntarget: 'https://example.com'\n"
	for _, tc := range []struct {
		name, yaml string
		args       []string
	}{
		{"null list with single config", "proxies: null\n" + single, nil},
		{"empty list with single config", "proxies:\n" + single, nil},
		{"null list with CLI", "proxies: null\n", []string{":7789", "https://example.com"}},
		{"empty listen", list + "listen: ''\n", nil},
		{"null listen", list + "listen: null\n", nil},
		{"empty target", list + "target: ''\n", nil},
		{"null target", list + "target: null\n", nil},
		{"empty proxy", list + "proxy: ''\n", nil},
		{"null proxy", list + "proxy: null\n", nil},
		{"merged empty field", list + "<<: {proxy: null}\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"-config", path}, tc.args...)
			if _, err := loadConfig(args, io.Discard); err == nil {
				t.Fatal("accepted explicitly mixed configuration")
			}
		})
	}
}

func TestConfiguredResponseHeaderTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer upstream.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := "proxies:\n  - listen: '127.0.0.1:0'\n    target: '" + upstream.URL + "'\n    response_header_timeout: 40ms\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig([]string{"-config", path}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	servers, err := bindProxies(cfg.Proxies, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveProxies(ctx, servers, log.New(io.Discard, "", 0)) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client := &http.Client{Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Get("http://" + servers[0].listener.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d want=502", response.StatusCode)
	}
}

func TestMultipleProxiesAndShutdown(t *testing.T) {
	var entries []proxyConfig
	for _, body := range []string{"first", "second"} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
		defer upstream.Close()
		entries = append(entries, proxyConfig{Listen: "127.0.0.1:0", Target: mustURL(t, upstream.URL)})
	}
	servers, err := bindProxies(entries, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveProxies(ctx, servers, log.New(io.Discard, "", 0)) }()
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	for i, expected := range []string{"first", "second"} {
		resp, err := client.Get("http://" + servers[i].listener.Addr().String() + "/test")
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || string(body) != expected {
			t.Fatalf("proxy %d: body=%q err=%v", i, body, err)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown timed out")
	}
	for _, p := range servers {
		listener, err := net.Listen("tcp", p.listener.Addr().String())
		if err != nil {
			t.Fatalf("port not released: %v", err)
		}
		listener.Close()
	}
}

func TestBindFailureReleasesPorts(t *testing.T) {
	available, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := available.Addr().String()
	available.Close()
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	_, err = bindProxies([]proxyConfig{
		{Listen: address, Target: mustURL(t, "http://example.com")},
		{Listen: occupied.Addr().String(), Target: mustURL(t, "http://example.org")},
	}, log.New(io.Discard, "", 0))
	if err == nil {
		t.Fatal("expected occupied port error")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("first listener leaked: %v", err)
	}
	listener.Close()
}

func newTestProxy(t *testing.T, target *url.URL) *httputil.ReverseProxy {
	t.Helper()
	handler := newProxy(proxyConfig{Target: target, ResponseHeaderTimeout: defaultResponseHeaderTimeout}, log.New(io.Discard, "", 0))
	t.Cleanup(handler.Transport.(*http.Transport).CloseIdleConnections)
	return handler
}
