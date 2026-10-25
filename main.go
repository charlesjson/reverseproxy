package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

type proxyConfig struct {
	Listen string `yaml:"listen"`
	Target string `yaml:"target"`
	Proxy  string `yaml:"proxy"`
}

type config struct {
	Listen  string        `yaml:"listen"`
	Target  string        `yaml:"target"`
	Proxy   string        `yaml:"proxy"`
	Proxies []proxyConfig `yaml:"proxies"`
}

func loadConfig(args []string, output io.Writer) (config, error) {
	var cfg config
	flags := flag.NewFlagSet("reverseproxy", flag.ContinueOnError)
	flags.SetOutput(output)
	path := flags.String("config", "", "YAML 配置文件路径")
	listen := flags.String("listen", "", "监听地址，例如 0.0.0.0:7789")
	proxy := flags.String("proxy", "", "上游代理 URL，支持 http/https/socks5/socks5h；为空时直连")
	target := flags.String("target", "", "目标 URL，例如 https://www.google.com")
	flags.Usage = func() {
		fmt.Fprintln(output, "用法: reverseproxy [选项] [监听地址 目标URL]\n单目标配置可用命令行覆盖；proxies 列表不可与单目标参数混用。选项须位于位置参数之前。")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return cfg, err
	}
	if *path != "" {
		file, err := os.Open(*path)
		if err != nil {
			return cfg, fmt.Errorf("打开配置文件: %w", err)
		}
		defer file.Close()
		decoder := yaml.NewDecoder(file)
		decoder.KnownFields(true)
		if err := decoder.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("解析 YAML: %w", err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return cfg, errors.New("配置文件只能包含一个 YAML 文档")
		}
	}
	hasSingleFlags := false
	hasProxyFlag := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "proxy" {
			hasProxyFlag = true
		}
		if f.Name == "listen" || f.Name == "target" {
			hasSingleFlags = true
		}
	})
	if cfg.Proxies != nil && (cfg.Listen != "" || cfg.Target != "" || cfg.Proxy != "" || hasProxyFlag || hasSingleFlags || flags.NArg() != 0) {
		return cfg, errors.New("proxies 列表不能与顶层 listen/target/proxy 或命令行单目标参数混用")
	}
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "listen":
			cfg.Listen = *listen
		case "target":
			cfg.Target = *target
		case "proxy":
			cfg.Proxy = *proxy
		}
	})
	switch flags.NArg() {
	case 0:
	case 2:
		if hasSingleFlags {
			return cfg, errors.New("位置参数不能与 -listen/-target 同时使用")
		}
		cfg.Listen, cfg.Target = flags.Arg(0), flags.Arg(1)
	default:
		return cfg, errors.New("请同时提供监听地址和目标 URL")
	}
	if cfg.Proxies == nil {
		cfg.Proxies = []proxyConfig{{Listen: cfg.Listen, Target: cfg.Target, Proxy: cfg.Proxy}}
	}
	if len(cfg.Proxies) == 0 {
		return cfg, errors.New("proxies 列表不能为空")
	}
	seen := make(map[string]bool)
	for i, entry := range cfg.Proxies {
		if entry.Listen == "" || entry.Target == "" {
			return cfg, fmt.Errorf("代理 %d: 必须提供 listen 和 target", i+1)
		}
		if _, _, err := net.SplitHostPort(entry.Listen); err != nil {
			return cfg, fmt.Errorf("代理 %d: 无效的监听地址: %w", i+1, err)
		}
		if _, err := parseTarget(entry.Target); err != nil {
			return cfg, fmt.Errorf("代理 %d: %w", i+1, err)
		}
		if _, err := parseProxy(entry.Proxy); err != nil {
			return cfg, fmt.Errorf("代理 %d: %w", i+1, err)
		}
		if seen[entry.Listen] {
			return cfg, fmt.Errorf("重复的监听地址: %s", entry.Listen)
		}
		seen[entry.Listen] = true
	}
	return cfg, nil
}

func parseTarget(raw string) (*url.URL, error) {
	target, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("无效的目标 URL: %w", err)
	}
	if (target.Scheme != "http" && target.Scheme != "https") || target.Hostname() == "" {
		return nil, errors.New("目标 URL 必须是包含主机名的 http:// 或 https:// 地址")
	}
	if target.User != nil || target.Fragment != "" || strings.HasSuffix(target.Host, ":") {
		return nil, errors.New("目标 URL 不能包含用户信息、片段或空端口")
	}
	return target, nil
}

// 不回显完整代理 URL，避免错误信息泄露认证信息。
func parseProxy(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("无效的上游代理 URL")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, errors.New("上游代理协议必须是 http、https、socks5 或 socks5h")
	}
	if u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("上游代理 URL 必须包含主机，且不能包含路径、查询参数或片段")
	}
	if strings.HasSuffix(u.Host, ":") {
		return nil, errors.New("上游代理端口不能为空")
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, errors.New("上游代理端口必须在 1 到 65535 之间")
		}
	}
	if (u.Scheme == "socks5" || u.Scheme == "socks5h") && u.User != nil {
		password, _ := u.User.Password()
		if len(u.User.Username()) > 255 || len(password) > 255 {
			return nil, errors.New("SOCKS5 用户名和密码不能超过 255 字节")
		}
	}
	return u, nil
}

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
		if path != prefix && !strings.HasPrefix(path, prefix+"/") {
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

type requestLogKey struct{}
type requestLog struct {
	id      uint64
	started time.Time
}

var requestSequence atomic.Uint64

func logProxyResponse(request *http.Request, status int) {
	entry, ok := request.Context().Value(requestLogKey{}).(requestLog)
	if !ok {
		return
	}
	log.Printf("响应 id=%d status=%d headers_duration=%s", entry.id, status, time.Since(entry.started).Round(time.Microsecond))
}

func newProxy(target *url.URL) *httputil.ReverseProxy {
	return newProxyWithTransport(target, nil)
}

func newProxyWithTransport(target, upstreamProxy *url.URL) *httputil.ReverseProxy {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// 显式配置，避免环境中的 HTTP_PROXY 意外改变每个目标的路由。
	transport.Proxy = nil
	if upstreamProxy != nil {
		transport.Proxy = http.ProxyURL(upstreamProxy)
	}
	return &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(request *httputil.ProxyRequest) {
			scheme := "http"
			if request.In.TLS != nil {
				scheme = "https"
			}
			origin := url.URL{Scheme: scheme, Host: request.In.Host}
			request.Out = request.Out.WithContext(context.WithValue(request.Out.Context(), clientOriginKey{}, origin))
			entry := requestLog{id: requestSequence.Add(1), started: time.Now()}
			request.Out = request.Out.WithContext(context.WithValue(request.Out.Context(), requestLogKey{}, entry))
			request.SetURL(target)
			log.Printf("请求 id=%d client=%q host=%q method=%s uri=%q upstream=%q", entry.id, request.In.RemoteAddr, request.In.Host, request.In.Method, request.In.URL.RequestURI(), request.Out.URL.String())
			// 使用目标 Host，并重建转发信息，避免信任客户端伪造的头。
			request.Out.Header.Del("X-Forwarded-For")
			request.Out.Header.Del("X-Forwarded-Host")
			request.Out.Header.Del("X-Forwarded-Proto")
			request.SetXForwarded()
		},
		ModifyResponse: func(response *http.Response) error {
			logProxyResponse(response.Request, response.StatusCode)
			return rewriteLocation(response, target)
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logProxyResponse(r, http.StatusBadGateway)
			log.Printf("上游请求失败: %v", err)
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		},
	}
}

type proxyServer struct {
	server   *http.Server
	listener net.Listener
	target   *url.URL
}

// 先绑定所有端口；任何一项失败时释放已绑定的端口。
func bindProxies(entries []proxyConfig) (servers []proxyServer, err error) {
	defer func() {
		if err != nil {
			for _, p := range servers {
				_ = p.listener.Close()
			}
			servers = nil
		}
	}()
	for i, entry := range entries {
		target, parseErr := parseTarget(entry.Target)
		if parseErr != nil {
			return servers, fmt.Errorf("代理 %d: %w", i+1, parseErr)
		}
		upstreamProxy, proxyErr := parseProxy(entry.Proxy)
		if proxyErr != nil {
			return servers, fmt.Errorf("代理 %d: %w", i+1, proxyErr)
		}
		listener, listenErr := net.Listen("tcp", entry.Listen)
		if listenErr != nil {
			return servers, fmt.Errorf("代理 %d 监听 %s 失败: %w", i+1, entry.Listen, listenErr)
		}
		servers = append(servers, proxyServer{
			server:   &http.Server{Handler: newProxyWithTransport(target, upstreamProxy), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second},
			listener: listener, target: target,
		})
	}
	return servers, nil
}

func serveProxies(ctx context.Context, servers []proxyServer) error {
	done := make(chan error, len(servers))
	for _, p := range servers {
		go func(p proxyServer) { done <- p.server.Serve(p.listener) }(p)
		log.Printf("反向代理已启动: http://%s -> %s", p.listener.Addr(), p.target)
	}
	var serveErr error
	select {
	case serveErr = <-done:
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	shutdownErrors := make(chan error, len(servers))
	for _, p := range servers {
		wg.Add(1)
		go func(p proxyServer) {
			defer wg.Done()
			if err := p.server.Shutdown(shutdownCtx); err != nil {
				_ = p.server.Close()
				shutdownErrors <- fmt.Errorf("停止 %s: %w", p.listener.Addr(), err)
			}
			_ = p.listener.Close()
		}(p)
	}
	wg.Wait()
	close(shutdownErrors)
	for err := range shutdownErrors {
		serveErr = errors.Join(serveErr, err)
	}
	return serveErr
}

func run(args []string) error {
	cfg, err := loadConfig(args, os.Stderr)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	servers, err := bindProxies(cfg.Proxies)
	if err != nil {
		return err
	}
	return serveProxies(ctx, servers)
}

func main() {
	if err := run(os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}
