package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
)

// 按 RFC 1928/1929 实现 SOCKS5 TCP CONNECT；域名原样交给代理解析。
func connectSOCKSTunnel(conn net.Conn, proxy, target *url.URL) error {
	greeting := []byte{5, 1, 0}
	if proxy.User != nil {
		greeting = []byte{5, 2, 0, 2}
	}
	if _, err := conn.Write(greeting); err != nil {
		return fmt.Errorf("发送 SOCKS5 协商: %w", err)
	}
	var method [2]byte
	if _, err := io.ReadFull(conn, method[:]); err != nil {
		return fmt.Errorf("读取 SOCKS5 协商: %w", err)
	}
	if method[0] != 5 {
		return errors.New("上游返回无效的 SOCKS5 版本")
	}
	switch method[1] {
	case 0:
	case 2:
		if proxy.User == nil {
			return errors.New("SOCKS5 代理要求用户名和密码")
		}
		username := proxy.User.Username()
		secret, _ := proxy.User.Password()
		if len(username) == 0 || len(username) > 255 || len(secret) == 0 || len(secret) > 255 {
			return errors.New("SOCKS5 认证用户名和密码须为 1 到 255 字节")
		}
		auth := append([]byte{1, byte(len(username))}, username...)
		auth = append(auth, byte(len(secret)))
		auth = append(auth, secret...)
		if _, err := conn.Write(auth); err != nil {
			return fmt.Errorf("发送 SOCKS5 认证: %w", err)
		}
		var status [2]byte
		if _, err := io.ReadFull(conn, status[:]); err != nil {
			return fmt.Errorf("读取 SOCKS5 认证结果: %w", err)
		}
		if status != [2]byte{1, 0} {
			return errors.New("SOCKS5 认证失败")
		}
	default:
		return fmt.Errorf("SOCKS5 代理未选择支持的认证方式: %d", method[1])
	}
	host, portText, err := net.SplitHostPort(endpointAddress(target))
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("无效的 SOCKS5 目标端口")
	}
	request := []byte{5, 1, 0}
	ip := net.ParseIP(host)
	switch {
	case ip.To4() != nil:
		request = append(request, 1)
		request = append(request, ip.To4()...)
	case ip.To16() != nil:
		request = append(request, 4)
		request = append(request, ip.To16()...)
	default:
		if len(host) == 0 || len(host) > 255 {
			return errors.New("SOCKS5 目标域名须为 1 到 255 字节")
		}
		request = append(request, 3, byte(len(host)))
		request = append(request, host...)
	}
	request = binary.BigEndian.AppendUint16(request, uint16(port))
	if _, err := conn.Write(request); err != nil {
		return fmt.Errorf("发送 SOCKS5 CONNECT: %w", err)
	}
	var reply [4]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return fmt.Errorf("读取 SOCKS5 CONNECT 响应: %w", err)
	}
	if reply[0] != 5 || reply[2] != 0 {
		return errors.New("无效的 SOCKS5 CONNECT 响应")
	}
	if reply[1] != 0 {
		return fmt.Errorf("SOCKS5 CONNECT 失败: 状态 %d", reply[1])
	}
	var addressSize int
	switch reply[3] {
	case 1:
		addressSize = net.IPv4len
	case 4:
		addressSize = net.IPv6len
	case 3:
		var size [1]byte
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return fmt.Errorf("读取 SOCKS5 绑定地址: %w", err)
		}
		addressSize = int(size[0])
	default:
		return errors.New("SOCKS5 返回了未知的地址类型")
	}
	if _, err := io.CopyN(io.Discard, conn, int64(addressSize+2)); err != nil {
		return fmt.Errorf("读取 SOCKS5 绑定地址: %w", err)
	}
	return nil
}
