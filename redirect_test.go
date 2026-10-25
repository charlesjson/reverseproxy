package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRedirectStaysOnProxy(t *testing.T) {
	var upstreamURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, upstreamURL+"/next?q=ok", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "done")
	}))
	defer upstream.Close()
	upstreamURL = upstream.URL
	target, _ := url.Parse(upstreamURL)
	proxy := httptest.NewServer(newProxy(target))
	defer proxy.Close()
	resp, err := proxy.Client().Get(proxy.URL + "/start")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Request.URL.Host != mustURL(t, proxy.URL).Host {
		t.Fatalf("redirect escaped proxy: %s", resp.Request.URL)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "done" || resp.Request.URL.RawQuery != "q=ok" {
		t.Fatalf("unexpected result: %s %s", body, resp.Request.URL)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestRedirectLocations(t *testing.T) {
	for _, tc := range []struct{ name, targetPath, requestPath, location, want string }{
		{"absolute", "", "/start", "{upstream}/next?q=a%2Fb#part", "{proxy}/next?q=a%2Fb#part"},
		{"scheme relative", "", "/start", "//{host}/next", "{proxy}/next"},
		{"root relative", "/base", "/start", "/base/next", "{proxy}/next"},
		{"relative", "/base", "/dir/start", "next", "{proxy}/dir/next"},
		{"escaped path", "/base", "/start", "{upstream}/base/a%2Fb?q=1", "{proxy}/a%2Fb?q=1"},
		{"outside prefix", "/base", "/start", "{upstream}/elsewhere", "{upstream}/elsewhere"},
		{"external", "", "/start", "https://other.example/next", "https://other.example/next"},
		{"lookalike host", "", "/start", "http://{hostname}.evil.example/", "http://{hostname}.evil.example/"},
	} {
		for _, status := range []int{301, 302, 303, 307, 308} {
			t.Run(fmt.Sprintf("%s/%d", tc.name, status), func(t *testing.T) {
				var location string
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Location", location)
					w.WriteHeader(status)
				}))
				defer upstream.Close()
				target := mustURL(t, upstream.URL+tc.targetPath)
				proxy := httptest.NewServer(newProxy(target))
				defer proxy.Close()
				replace := strings.NewReplacer("{upstream}", upstream.URL, "{proxy}", proxy.URL, "{host}", target.Host, "{hostname}", target.Hostname())
				location = replace.Replace(tc.location)
				client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
				defer client.CloseIdleConnections()
				response, err := client.Get(proxy.URL + tc.requestPath)
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if response.StatusCode != status || response.Header.Get("Location") != replace.Replace(tc.want) {
					t.Fatalf("status=%d Location=%q want=%q", response.StatusCode, response.Header.Get("Location"), replace.Replace(tc.want))
				}
			})
		}
	}
}
