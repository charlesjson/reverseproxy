package main

import (
	"bufio"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResponseHeaderTimeoutConfig(t *testing.T) {
	single := "listen: ':7789'\ntarget: 'http://example.com'\n"
	list := "proxies:\n  - listen: ':7789'\n    target: 'http://example.com'\n    response_header_timeout: 250ms\n  - listen: ':7790'\n    target: 'http://example.org'\n    response_header_timeout: 0s\n"
	for _, tc := range []struct {
		name, yaml string
		args       []string
		want       []time.Duration
	}{
		{"default", single, nil, []time.Duration{30 * time.Second}},
		{"YAML", single + "response_header_timeout: 2s\n", nil, []time.Duration{2 * time.Second}},
		{"CLI override", single + "response_header_timeout: 2s\n", []string{"-response-header-timeout", "250ms"}, []time.Duration{250 * time.Millisecond}},
		{"disabled", single, []string{"-response-header-timeout", "0s"}, []time.Duration{0}},
		{"per target", list, nil, []time.Duration{250 * time.Millisecond, 0}},
		{"negative", single, []string{"-response-header-timeout", "-1s"}, nil},
		{"invalid unit", single + "response_header_timeout: forever\n", nil, nil},
		{"mixed flag", list, []string{"-response-header-timeout", "1s"}, nil},
		{"mixed empty field", list + "response_header_timeout: null\n", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfig(append([]string{"-config", path}, tc.args...), io.Discard)
			if tc.want == nil {
				if err == nil {
					t.Fatal("accepted invalid timeout configuration")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.Proxies) != len(tc.want) {
				t.Fatalf("got %d proxies, want %d", len(cfg.Proxies), len(tc.want))
			}
			for i, want := range tc.want {
				if cfg.Proxies[i].ResponseHeaderTimeout != want {
					t.Errorf("proxy %d timeout=%s want=%s", i, cfg.Proxies[i].ResponseHeaderTimeout, want)
				}
			}
		})
	}
}

func TestHeaderTimeoutDoesNotLimitStreamingBody(t *testing.T) {
	for _, contentType := range []string{"text/event-stream", "application/octet-stream"} {
		t.Run(contentType, func(t *testing.T) {
			release := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", contentType)
				if contentType == "text/event-stream" {
					w.Header().Set("Content-Length", "12")
				}
				_, _ = io.WriteString(w, "first\n")
				w.(http.Flusher).Flush()
				select {
				case <-release:
					_, _ = io.WriteString(w, "final\n")
				case <-r.Context().Done():
				}
			}))
			defer upstream.Close()
			handler := newProxy(proxyConfig{Target: mustURL(t, upstream.URL), ResponseHeaderTimeout: 40 * time.Millisecond}, log.New(io.Discard, "", 0))
			defer handler.Transport.(*http.Transport).CloseIdleConnections()
			proxy := httptest.NewServer(handler)
			defer proxy.Close()
			client := &http.Client{Timeout: 3 * time.Second}
			defer client.CloseIdleConnections()
			response, err := client.Get(proxy.URL + "/")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			reader := bufio.NewReader(response.Body)
			first, err := reader.ReadString('\n')
			if err != nil || first != "first\n" {
				t.Fatalf("first chunk=%q err=%v", first, err)
			}
			// 在响应头超时之后才允许上游结束，验证首块立即可读且流不会被截断。
			<-time.After(100 * time.Millisecond)
			close(release)
			last, err := io.ReadAll(reader)
			if err != nil || string(last) != "final\n" {
				t.Fatalf("last chunk=%q err=%v", last, err)
			}
		})
	}
}
