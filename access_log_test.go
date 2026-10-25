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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "request body" {
			t.Errorf("request body changed: %q", body)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer upstream.Close()
	for _, unavailable := range []bool{false, true} {
		var output bytes.Buffer
		previous := log.Writer()
		log.SetOutput(&output)
		func() {
			defer log.SetOutput(previous)
			if unavailable {
				upstream.Close()
			}
			target := mustURL(t, upstream.URL)
			proxy := newProxy(target)
			defer proxy.Transport.(*http.Transport).CloseIdleConnections()
			request := httptest.NewRequest("POST", "http://proxy.local/path?q=hello", strings.NewReader("request body"))
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
			if strings.Count(output.String(), "请求 id=") != 1 {
				t.Errorf("request logged multiple times: %s", output.String())
			}
		}()
	}
}
