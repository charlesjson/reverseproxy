package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
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
	proxy := httptest.NewServer(newTestProxy(t, target))
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
		{"prefix root", "/base", "/start", "/base?q=1#part", "{upstream}/base?q=1#part"},
		{"prefix root with trailing slash", "/base/", "/start", "/base", "{upstream}/base"},
		{"relative prefix root", "/base", "/start", "../base", "{upstream}/base"},
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
				proxy := httptest.NewServer(newTestProxy(t, target))
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

func TestRedirectPrefixRootDoesNotLoop(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/base/" {
			http.Redirect(w, r, "/base", http.StatusMovedPermanently)
			return
		}
		_, _ = io.WriteString(w, "done")
	}))
	defer upstream.Close()
	for _, prefix := range []string{"/base", "/base/"} {
		t.Run(prefix, func(t *testing.T) {
			handler := newTestProxy(t, mustURL(t, upstream.URL+prefix))
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
			body, err := io.ReadAll(response.Body)
			if err != nil || string(body) != "done" || response.Request.URL.String() != upstream.URL+"/base" {
				t.Fatalf("url=%s body=%q err=%v", response.Request.URL, body, err)
			}
		})
	}
}

func TestRedirectDoesNotAccumulateTargetQuery(t *testing.T) {
	for _, tc := range []struct{ name, fixed, redirected, want string }{
		{"already present", "key=value", "key=value&extra=yes", "key=value&extra=yes"},
		{"reordered", "key=value", "extra=yes&key=value", "extra=yes&key=value"},
		{"intentional duplicates", "tag=a", "tag=a&tag=a&tag=b", "tag=a&tag=a&tag=b"},
		{"configured duplicates", "tag=a&tag=a", "tag=a&tag=a&tag=b", "tag=a&tag=a&tag=b"},
		{"missing fixed parameter", "key=value", "extra=yes", "key=value&extra=yes"},
		{"different value", "key=value", "key=other", "key=value&key=other"},
		{"equivalent escaping", "key=a%20b", "extra=%2f&key=a+b", "extra=%2f&key=a+b"},
		{"empty values", "flag=&other", "flag=&other", "flag=&other"},
		{"empty name versus empty segments", "=", "x=1&&", "=&x=1&&"},
		{"empty configured segments", "&&key=value&&", "x=1&key=value", "x=1&key=value"},
		{"invalid escape is removed", "key=value", "bad=%ZZ&ok=yes", "key=value&ok=yes"},
		{"invalid semicolon is removed", "key=value", "bad=a;b&ok=yes", "key=value&ok=yes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/start":
					w.Header().Set("Location", "/next?"+tc.redirected)
					w.WriteHeader(http.StatusFound)
				case "/next":
					w.Header().Set("Location", "/final?"+r.URL.RawQuery)
					w.WriteHeader(http.StatusTemporaryRedirect)
				default:
					_, _ = io.WriteString(w, r.URL.RawQuery)
				}
			}))
			defer upstream.Close()
			handler := newTestProxy(t, mustURL(t, upstream.URL+"?"+tc.fixed))
			defer handler.Transport.(*http.Transport).CloseIdleConnections()
			proxy := httptest.NewServer(handler)
			defer proxy.Close()
			response, err := proxy.Client().Get(proxy.URL + "/start")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || string(body) != tc.want {
				t.Fatalf("upstream query=%q want=%q err=%v", body, tc.want, err)
			}
			if response.Request.URL.Host != mustURL(t, proxy.URL).Host {
				t.Fatalf("redirect escaped proxy: %s", response.Request.URL)
			}
		})
	}
}
