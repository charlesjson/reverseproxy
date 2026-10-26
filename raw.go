package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// rawServer 只管理连接生命周期，不解析或重建 HTTP 报文。
type rawServer struct {
	dialer rawDialer
	logger *log.Logger
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	listener net.Listener
	clients  map[net.Conn]struct{}
	closed   bool
	workers  sync.WaitGroup
}

func newRawServer(cfg proxyConfig, logger *log.Logger) *rawServer {
	ctx, cancel := context.WithCancel(context.Background())
	return &rawServer{dialer: rawDialer{cfg: cfg}, logger: logger, ctx: ctx, cancel: cancel, clients: make(map[net.Conn]struct{})}
}

func (s *rawServer) Serve(listener net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = listener.Close()
		return http.ErrServerClosed
	}
	s.listener = listener
	s.mu.Unlock()
	for {
		client, err := listener.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return http.ErrServerClosed
			}
			return err
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = client.Close()
			return http.ErrServerClosed
		}
		s.clients[client] = struct{}{}
		s.workers.Add(1)
		s.mu.Unlock()
		go s.handle(client)
	}
}

func (s *rawServer) handle(client net.Conn) {
	defer s.workers.Done()
	defer func() {
		_ = client.Close()
		s.mu.Lock()
		delete(s.clients, client)
		s.mu.Unlock()
	}()
	id, started := requestSequence.Add(1), time.Now()
	upstream, err := s.dialer.DialContext(s.ctx)
	if err != nil {
		if s.ctx.Err() == nil {
			s.logger.Printf("raw 连接失败 id=%d client=%q: %v", id, client.RemoteAddr(), err)
		}
		return
	}
	defer upstream.Close()
	stop := context.AfterFunc(s.ctx, func() { abortRawConnection(upstream) })
	defer stop()
	s.logger.Printf("raw 连接 id=%d client=%q upstream=%q", id, client.RemoteAddr(), s.dialer.cfg.Target.String())
	type copyResult struct {
		upload bool
		bytes  int64
		err    error
	}
	results := make(chan copyResult, 2)
	copyStream := func(dst, src net.Conn, upload bool) {
		buffer := responseBuffers.Get()
		n, err := io.CopyBuffer(dst, src, buffer)
		responseBuffers.Put(buffer)
		if err == nil {
			err = closeWrite(dst)
		}
		results <- copyResult{upload: upload, bytes: n, err: err}
	}
	go copyStream(upstream, client, true)
	go copyStream(client, upstream, false)
	var uploaded, downloaded int64
	for i := 0; i < 2; i++ {
		result := <-results
		if result.upload {
			uploaded = result.bytes
		} else {
			downloaded = result.bytes
		}
		if result.err != nil {
			abortRawConnection(client)
			abortRawConnection(upstream)
		}
	}
	s.logger.Printf("raw 断开 id=%d uploaded=%d downloaded=%d duration=%s", id, uploaded, downloaded, time.Since(started).Round(time.Microsecond))
}

// 停机或读写失败时直接关闭底层连接，避免等待 TLS close_notify 写入。
func abortRawConnection(conn net.Conn) {
	for {
		wrapped, ok := conn.(interface{ NetConn() net.Conn })
		if !ok {
			_ = conn.Close()
			return
		}
		conn = wrapped.NetConn()
	}
}

func closeWrite(conn net.Conn) error {
	if writer, ok := conn.(interface{ CloseWrite() error }); ok {
		return writer.CloseWrite()
	}
	return nil
}

func (s *rawServer) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.cancel()
	listener := s.listener
	clients := make([]net.Conn, 0, len(s.clients))
	for client := range s.clients {
		clients = append(clients, client)
	}
	s.mu.Unlock()
	var err error
	if listener != nil {
		err = listener.Close()
		if errors.Is(err, net.ErrClosed) {
			err = nil
		}
	}
	for _, client := range clients {
		_ = client.Close()
	}
	return err
}

func (s *rawServer) Shutdown(ctx context.Context) error {
	err := s.Close()
	done := make(chan struct{})
	go func() { s.workers.Wait(); close(done) }()
	select {
	case <-done:
		return err
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	}
}
