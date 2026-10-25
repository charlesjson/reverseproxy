package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

func BenchmarkProxyResponse(b *testing.B) {
	payload := bytes.Repeat([]byte("x"), 64*1024)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer upstream.Close()
	target, err := url.Parse(upstream.URL)
	if err != nil {
		b.Fatal(err)
	}
	handler := newProxy(proxyConfig{Target: target, ResponseHeaderTimeout: defaultResponseHeaderTimeout}, log.New(io.Discard, "", 0))
	defer handler.Transport.(*http.Transport).CloseIdleConnections()
	proxy := httptest.NewServer(handler)
	defer proxy.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		response, err := client.Get(proxy.URL + "/")
		if err != nil {
			b.Fatal(err)
		}
		n, err := io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if err != nil || n != int64(len(payload)) {
			b.Fatalf("copied=%d err=%v", n, err)
		}
	}
}
