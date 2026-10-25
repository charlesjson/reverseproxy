package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestLogging(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		name := "success"
		if unavailable {
			name = "unavailable"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if string(body) != "request body" {
					t.Errorf("request body changed: %q", body)
				}
				w.WriteHeader(http.StatusCreated)
			}))
			defer upstream.Close()
			if unavailable {
				upstream.Close()
			}
			var output bytes.Buffer
			target := mustURL(t, upstream.URL)
			proxy := newProxy(proxyConfig{Target: target, ResponseHeaderTimeout: defaultResponseHeaderTimeout}, log.New(&output, "", 0))
			defer proxy.Transport.(*http.Transport).CloseIdleConnections()
			request := httptest.NewRequest("POST", "http://proxy.local/path?q=hello", strings.NewReader("request body"))
			request.Header.Set("Authorization", "test-authorization-value")
			request.Header.Set("Cookie", "test-cookie-value")
			response := httptest.NewRecorder()
			proxy.ServeHTTP(response, request)
			expected := "status=201"
			if unavailable {
				expected = "status=502"
			}
			for _, value := range []string{"请求 id=", "method=POST", `host="proxy.local"`, `uri="/path?q=hello"`, "upstream=", "响应 id=", expected, "headers_duration="} {
				if !strings.Contains(output.String(), value) {
					t.Errorf("missing %q in %s", value, output.String())
				}
			}
			if strings.Count(output.String(), "请求 id=") != 1 || strings.Count(output.String(), "响应 id=") != 1 {
				t.Errorf("request/response logs were duplicated or mixed: %s", output.String())
			}
			for _, value := range []string{"request body", "test-authorization-value", "test-cookie-value"} {
				if strings.Contains(output.String(), value) {
					t.Errorf("unexpected body or header in logs: %s", output.String())
				}
			}
		})
	}
}
