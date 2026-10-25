package main

import (
	"context"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type clientOriginKey struct{}

// 将上游 URL 映射回代理入口；只处理当前目标可表示的路径。
func rewriteLocation(response *http.Response, target *url.URL) error {
	raw := response.Header.Get("Location")
	if raw == "" || response.StatusCode < 300 || response.StatusCode >= 400 {
		return nil
	}
	location, err := url.Parse(raw)
	if err != nil || location.User != nil {
		return nil
	}
	resolved := response.Request.URL.ResolveReference(location)
	if !sameOrigin(resolved, target) {
		return nil
	}
	path := resolved.EscapedPath()
	prefix := strings.TrimSuffix(target.EscapedPath(), "/")
	if prefix != "" {
		if path == prefix {
			// 代理根路径会转发到 prefix + "/"，无法表示不带尾斜线的 prefix。
			// 使用上游绝对地址，避免相对 Location 再次经过前缀拼接而循环。
			response.Header.Set("Location", resolved.String())
			return nil
		}
		if !strings.HasPrefix(path, prefix+"/") {
			return nil
		}
		path = strings.TrimPrefix(path, prefix)
	}
	if path == "" {
		path = "/"
	}
	origin, ok := response.Request.Context().Value(clientOriginKey{}).(url.URL)
	if !ok {
		return nil
	}
	origin.Path, err = url.PathUnescape(path)
	if err != nil {
		return nil
	}
	origin.RawPath = path
	origin.RawQuery, origin.ForceQuery = resolved.RawQuery, resolved.ForceQuery
	origin.Fragment, origin.RawFragment = resolved.Fragment, resolved.RawFragment
	response.Header.Set("Location", origin.String())
	return nil
}

func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

// 补齐目标固定参数的出现次数，保留请求原有的顺序、编码和有意重复的参数。
// 重定向通常已经携带固定参数，不能在每一跳再次追加。
func mergeTargetQuery(fixed, incoming string) string {
	if fixed == "" {
		return incoming
	}
	if incoming == "" {
		return fixed
	}
	type parameter struct{ key, value string }
	decode := func(raw string) parameter {
		key, value, _ := strings.Cut(raw, "=")
		if decoded, err := url.QueryUnescape(key); err == nil {
			key = decoded
		}
		if decoded, err := url.QueryUnescape(value); err == nil {
			value = decoded
		}
		return parameter{key, value}
	}
	counts := make(map[parameter]int)
	for _, raw := range strings.Split(incoming, "&") {
		if raw == "" {
			continue
		}
		counts[decode(raw)]++
	}
	var missing []string
	for _, raw := range strings.Split(fixed, "&") {
		if raw == "" {
			continue
		}
		key := decode(raw)
		if counts[key] > 0 {
			counts[key]--
		} else {
			missing = append(missing, raw)
		}
	}
	if len(missing) == 0 {
		return incoming
	}
	return strings.Join(missing, "&") + "&" + incoming
}

type requestLogKey struct{}
type requestLog struct {
	id      uint64
	started time.Time
}

var requestSequence atomic.Uint64

const responseBufferSize = 32 * 1024

type responseBufferPool struct{ pool sync.Pool }

func (p *responseBufferPool) Get() []byte {
	buffer, _ := p.pool.Get().(*[responseBufferSize]byte)
	if buffer == nil {
		buffer = new([responseBufferSize]byte)
	}
	return buffer[:]
}

func (p *responseBufferPool) Put(buffer []byte) {
	if len(buffer) == responseBufferSize {
		p.pool.Put((*[responseBufferSize]byte)(buffer))
	}
}

var responseBuffers responseBufferPool

func logProxyResponse(logger *log.Logger, request *http.Request, status int) {
	entry, ok := request.Context().Value(requestLogKey{}).(requestLog)
	if !ok {
		return
	}
	logger.Printf("响应 id=%d status=%d headers_duration=%s", entry.id, status, time.Since(entry.started).Round(time.Microsecond))
}

func newProxy(cfg proxyConfig, logger *log.Logger) *httputil.ReverseProxy {
	target := cfg.Target
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = cfg.ResponseHeaderTimeout
	// 显式配置，避免环境中的 HTTP_PROXY 意外改变每个目标的路由。
	transport.Proxy = nil
	if cfg.Proxy != nil {
		upstreamProxy := *cfg.Proxy
		// Go 1.22 尚不识别 socks5h；socks5 同样由代理端解析目标域名。
		if upstreamProxy.Scheme == "socks5h" {
			upstreamProxy.Scheme = "socks5"
		}
		transport.Proxy = http.ProxyURL(&upstreamProxy)
	}
	return &httputil.ReverseProxy{
		Transport:  transport,
		ErrorLog:   logger,
		BufferPool: &responseBuffers,
		Rewrite: func(request *httputil.ProxyRequest) {
			scheme := "http"
			if request.In.TLS != nil {
				scheme = "https"
			}
			origin := url.URL{Scheme: scheme, Host: request.In.Host}
			request.Out = request.Out.WithContext(context.WithValue(request.Out.Context(), clientOriginKey{}, origin))
			entry := requestLog{id: requestSequence.Add(1), started: time.Now()}
			request.Out = request.Out.WithContext(context.WithValue(request.Out.Context(), requestLogKey{}, entry))
			incomingQuery := request.Out.URL.RawQuery
			if raw := request.In.URL.RawQuery; raw != incomingQuery {
				// 有效查询串保持原始字节；无效编码或分号仍使用标准库清理后的值。
				if _, err := url.ParseQuery(raw); err == nil {
					incomingQuery = raw
				}
			}
			request.SetURL(target)
			request.Out.URL.RawQuery = mergeTargetQuery(target.RawQuery, incomingQuery)
			logger.Printf("请求 id=%d client=%q host=%q method=%s uri=%q upstream=%q", entry.id, request.In.RemoteAddr, request.In.Host, request.In.Method, request.In.URL.RequestURI(), request.Out.URL.String())
			// 使用目标 Host，并重建转发信息，避免信任客户端伪造的头。
			request.Out.Header.Del("X-Forwarded-For")
			request.Out.Header.Del("X-Forwarded-Host")
			request.Out.Header.Del("X-Forwarded-Proto")
			request.SetXForwarded()
		},
		ModifyResponse: func(response *http.Response) error {
			logProxyResponse(logger, response.Request, response.StatusCode)
			return rewriteLocation(response, target)
		},
		// 普通响应使用写缓冲；标准库仍会立即刷新 SSE 和未知长度的流式响应。
		FlushInterval: 0,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logProxyResponse(logger, r, http.StatusBadGateway)
			logger.Printf("上游请求失败: %v", err)
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		},
	}
}
