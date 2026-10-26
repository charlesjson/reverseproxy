package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type managedServer interface {
	Serve(net.Listener) error
	Shutdown(context.Context) error
	Close() error
}

type proxyServer struct {
	server   managedServer
	listener net.Listener
	target   *url.URL
	mode     string
}

// 先绑定所有端口；任何一项失败时释放已绑定的端口。
func bindProxies(entries []proxyConfig, logger *log.Logger) (servers []proxyServer, err error) {
	defer func() {
		if err != nil {
			for _, p := range servers {
				_ = p.listener.Close()
			}
			servers = nil
		}
	}()
	for i, entry := range entries {
		listener, listenErr := net.Listen("tcp", entry.Listen)
		if listenErr != nil {
			return servers, fmt.Errorf("代理 %d 监听 %s 失败: %w", i+1, entry.Listen, listenErr)
		}
		var server managedServer
		mode := modeHTTP
		if entry.Mode == modeRaw {
			mode = modeRaw
			server = newRawServer(entry, logger)
		} else {
			server = &http.Server{Handler: newProxy(entry, logger), ErrorLog: logger, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
		}
		servers = append(servers, proxyServer{server: server, listener: listener, target: entry.Target, mode: mode})
	}
	return servers, nil
}

func serveProxies(ctx context.Context, servers []proxyServer, logger *log.Logger) error {
	done := make(chan error, len(servers))
	for _, p := range servers {
		go func(p proxyServer) { done <- p.server.Serve(p.listener) }(p)
		logger.Printf("反向代理已启动: mode=%s http://%s -> %s", p.mode, p.listener.Addr(), p.target)
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
	logger := log.New(os.Stderr, "", log.LstdFlags)
	servers, err := bindProxies(cfg.Proxies, logger)
	if err != nil {
		return err
	}
	return serveProxies(ctx, servers, logger)
}

func main() {
	if err := run(os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}
