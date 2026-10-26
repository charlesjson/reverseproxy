package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type fileProxyConfig struct {
	Mode                  string `yaml:"mode"`
	Listen                string `yaml:"listen"`
	Target                string `yaml:"target"`
	Proxy                 string `yaml:"proxy"`
	ResponseHeaderTimeout string `yaml:"response_header_timeout"`
}

type fileConfig struct {
	fileProxyConfig `yaml:",inline"`
	Proxies         []fileProxyConfig `yaml:"proxies"`
}

type proxyConfig struct {
	Mode                  string
	Listen                string
	Target                *url.URL
	Proxy                 *url.URL
	ResponseHeaderTimeout time.Duration
}

const defaultResponseHeaderTimeout = 30 * time.Second

const (
	modeHTTP = "http"
	modeRaw  = "raw"
)

type config struct {
	Proxies []proxyConfig
}

func loadConfig(args []string, output io.Writer) (config, error) {
	var cfg fileConfig
	var hasProxies, hasSingleFields bool
	flags := flag.NewFlagSet("reverseproxy", flag.ContinueOnError)
	flags.SetOutput(output)
	path := flags.String("config", "", "YAML 配置文件路径")
	mode := flags.String("mode", "", "代理模式：http（默认）或 raw（原始报文透传）")
	listen := flags.String("listen", "", "监听地址，例如 0.0.0.0:7789")
	proxy := flags.String("proxy", "", "上游代理 URL，支持 http/https/socks5/socks5h；为空时直连")
	headerTimeout := flags.String("response-header-timeout", "", "等待上游响应头的超时，例如 30s、500ms；默认 30s，0s 关闭")
	target := flags.String("target", "", "目标 URL，例如 https://www.google.com")
	flags.Usage = func() {
		fmt.Fprintln(output, "用法: reverseproxy [选项] [监听地址 目标URL]\n单目标配置可用命令行覆盖；proxies 列表不可与单目标参数混用。选项须位于位置参数之前。")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	if *path != "" {
		data, err := os.ReadFile(*path)
		if err != nil {
			return config{}, fmt.Errorf("打开配置文件: %w", err)
		}
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&cfg); err != nil {
			return config{}, fmt.Errorf("解析 YAML: %w", err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return config{}, errors.New("配置文件只能包含一个 YAML 文档")
		}
		// 结构体负责严格类型校验；键集合区分未填写与显式 null/空字符串。
		// 使用 YAML 解码器展开合并键，保证 << 引入的字段也参与互斥校验。
		var fields map[string]yaml.Node
		if err := yaml.Unmarshal(data, &fields); err != nil {
			return config{}, fmt.Errorf("解析 YAML 字段: %w", err)
		}
		_, hasProxies = fields["proxies"]
		for _, name := range []string{"mode", "listen", "target", "proxy", "response_header_timeout"} {
			if _, ok := fields[name]; ok {
				hasSingleFields = true
			}
		}
	}
	hasSingleFlags := false
	hasOtherSingleFlags := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "mode" || f.Name == "proxy" || f.Name == "response-header-timeout" {
			hasOtherSingleFlags = true
		}
		if f.Name == "listen" || f.Name == "target" {
			hasSingleFlags = true
		}
	})
	if hasProxies && (hasSingleFields || hasOtherSingleFlags || hasSingleFlags || flags.NArg() != 0) {
		return config{}, errors.New("proxies 列表不能与顶层单目标配置或命令行单目标参数混用")
	}
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "mode":
			cfg.Mode = *mode
		case "listen":
			cfg.Listen = *listen
		case "target":
			cfg.Target = *target
		case "proxy":
			cfg.Proxy = *proxy
		case "response-header-timeout":
			cfg.ResponseHeaderTimeout = *headerTimeout
		}
	})
	switch flags.NArg() {
	case 0:
	case 2:
		if hasSingleFlags {
			return config{}, errors.New("位置参数不能与 -listen/-target 同时使用")
		}
		cfg.Listen, cfg.Target = flags.Arg(0), flags.Arg(1)
	default:
		return config{}, errors.New("请同时提供监听地址和目标 URL")
	}
	if !hasProxies {
		cfg.Proxies = []fileProxyConfig{cfg.fileProxyConfig}
	}
	if len(cfg.Proxies) == 0 {
		return config{}, errors.New("proxies 列表不能为空")
	}
	resolved := config{Proxies: make([]proxyConfig, 0, len(cfg.Proxies))}
	seen := make(map[string]bool)
	for i, entry := range cfg.Proxies {
		if entry.Mode == "" {
			entry.Mode = modeHTTP
		}
		if entry.Mode != modeHTTP && entry.Mode != modeRaw {
			return config{}, fmt.Errorf("代理 %d: mode 必须是 http 或 raw", i+1)
		}
		if entry.Listen == "" || entry.Target == "" {
			return config{}, fmt.Errorf("代理 %d: 必须提供 listen 和 target", i+1)
		}
		if _, _, err := net.SplitHostPort(entry.Listen); err != nil {
			return config{}, fmt.Errorf("代理 %d: 无效的监听地址: %w", i+1, err)
		}
		target, err := parseTarget(entry.Target)
		if err != nil {
			return config{}, fmt.Errorf("代理 %d: %w", i+1, err)
		}
		if entry.Mode == modeRaw {
			if (target.EscapedPath() != "" && target.EscapedPath() != "/") || target.RawQuery != "" || target.ForceQuery {
				return config{}, fmt.Errorf("代理 %d: raw 模式的 target 只能指定协议、主机和端口，不能包含路径前缀或查询参数", i+1)
			}
			if target.Port() != "" {
				port, err := strconv.Atoi(target.Port())
				if err != nil || port < 1 || port > 65535 {
					return config{}, fmt.Errorf("代理 %d: raw 目标端口必须在 1 到 65535 之间", i+1)
				}
			}
		}
		proxy, err := parseProxy(entry.Proxy)
		if err != nil {
			return config{}, fmt.Errorf("代理 %d: %w", i+1, err)
		}
		timeout := defaultResponseHeaderTimeout
		if entry.ResponseHeaderTimeout != "" {
			timeout, err = time.ParseDuration(entry.ResponseHeaderTimeout)
			if err != nil || timeout < 0 {
				return config{}, fmt.Errorf("代理 %d: response_header_timeout 必须是非负时长，例如 30s、500ms 或 0s", i+1)
			}
		}
		if seen[entry.Listen] {
			return config{}, fmt.Errorf("重复的监听地址: %s", entry.Listen)
		}
		seen[entry.Listen] = true
		resolved.Proxies = append(resolved.Proxies, proxyConfig{Mode: entry.Mode, Listen: entry.Listen, Target: target, Proxy: proxy, ResponseHeaderTimeout: timeout})
	}
	return resolved, nil
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
