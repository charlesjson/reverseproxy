package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestConcurrentResponsesRemainIntact(t *testing.T) {
	const requests = 16
	payloads := make([][]byte, requests)
	for i := range payloads {
		payloads[i] = bytes.Repeat([]byte{byte(i + 1)}, 96*1024+i)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i, err := strconv.Atoi(r.URL.Query().Get("id"))
		if err != nil || i < 0 || i >= len(payloads) {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payloads[i])))
		_, _ = w.Write(payloads[i])
	}))
	defer upstream.Close()
	proxy := httptest.NewServer(newTestProxy(t, mustURL(t, upstream.URL)))
	defer proxy.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	errors := make(chan error, requests)
	for i := 0; i < requests; i++ {
		go func(i int) {
			response, err := client.Get(proxy.URL + "/?id=" + strconv.Itoa(i))
			if err != nil {
				errors <- err
				return
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err == nil && !bytes.Equal(body, payloads[i]) {
				err = fmt.Errorf("response %d was truncated or mixed with another response", i)
			}
			errors <- err
		}(i)
	}
	for i := 0; i < requests; i++ {
		if err := <-errors; err != nil {
			t.Error(err)
		}
	}
}

func TestWebSocketUpgradeAndFrames(t *testing.T) {
	// 一个带掩码的客户端文本帧，内容为 hi。
	clientFrame := []byte{0x81, 0x82, 1, 2, 3, 4, 'h' ^ 1, 'i' ^ 2}
	serverFrame := []byte{0x81, 0x02, 'o', 'k'}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		_, err = buffer.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n")
		if err != nil {
			t.Error(err)
			return
		}
		if err := buffer.Flush(); err != nil {
			t.Error(err)
			return
		}
		frame := make([]byte, len(clientFrame))
		if _, err := io.ReadFull(buffer, frame); err != nil || !bytes.Equal(frame, clientFrame) {
			t.Errorf("client frame=%v err=%v", frame, err)
			return
		}
		if _, err := conn.Write(serverFrame); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	proxy := httptest.NewServer(newTestProxy(t, mustURL(t, upstream.URL)))
	defer proxy.Close()
	conn, err := net.DialTimeout("tcp", proxy.Listener.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	_, err = fmt.Fprintf(conn, "GET /chat HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", proxy.Listener.Addr())
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusSwitchingProtocols || response.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("unexpected handshake: %d %v", response.StatusCode, response.Header)
	}
	if _, err := conn.Write(clientFrame); err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, len(serverFrame))
	if _, err := io.ReadFull(reader, frame); err != nil || !bytes.Equal(frame, serverFrame) {
		t.Fatalf("server frame=%v err=%v", frame, err)
	}
}
