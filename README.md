# Go 反向代理

将指定监听地址收到的 HTTP 请求转发到固定的 HTTP/HTTPS 上游，并返回上游的状态码、响应头和响应体。支持请求方法、请求体、路径、查询参数、流式响应和 WebSocket 升级。HTTPS 上游使用系统 CA 验证证书。

## 构建

需要 Go 1.22 或更新版本：

```sh
go build -o bin/reverseproxy .
```

## 命令行使用

```sh
./bin/reverseproxy 0.0.0.0:7789 https://www.google.com
# 或使用命名参数
./bin/reverseproxy -listen 0.0.0.0:7789 -target https://www.google.com
```

另一个终端中访问：

```sh
curl -i 'http://127.0.0.1:7789/search?q=golang'
```

请求会转发到 `https://www.google.com/search?q=golang`，其响应返回给 curl。运行程序的机器须能够访问目标站点。

## YAML 配置

使用 `proxies` 列表，在一个进程中运行多个反向代理，每个监听地址对应一个目标：

```yaml
proxies:
  - listen: "0.0.0.0:7789"
    target: "https://www.google.com"
  - listen: "0.0.0.0:7790"
    target: "https://www.bing.com"
```

```sh
./bin/reverseproxy -config config.example.yaml
```

访问 7789 端口会转发到 Google，访问 7790 端口会转发到 Bing。所有端口绑定成功后才开始服务；任何端口绑定失败会释放其他已绑定端口并退出。退出时同时停止所有代理。

仍兼容旧版单目标 YAML（顶层 `listen`、`target` 和可选的 `proxy`、`response_header_timeout`），单目标配置可通过 `-listen`/`-target`/`-proxy`/`-response-header-timeout` 覆盖。`proxies` 列表不能与顶层单目标配置或命令行单目标参数混用，即使顶层字段显式填写为 `null` 或空字符串也会报错。列表不能为空或 `null`，每项必须包含监听地址和目标 URL，监听地址不能重复。

选项须放在位置参数之前。位置参数与 `-listen`/`-target` 不可同时使用。未知 YAML 字段会报错。使用 `-h` 查看帮助，Ctrl+C 或 SIGTERM 可停止服务，普通请求有最多 10 秒的退出等待时间。

监听端提供 HTTP；`0.0.0.0` 表示监听所有 IPv4 网卡，只允许本机访问时使用 `127.0.0.1`。目标 URL 如包含路径前缀，该前缀会拼接到请求路径之前。固定查询参数按名称和值补齐缺少的次数：请求中已有相同参数时不会再追加，请求中有意重复的参数会保留。例如目标含 `?key=value`，重定向地址已含 `key=value` 时，下一跳不会重复添加。有效查询串保留原有顺序和编码；无法解析的请求参数仍由标准库清理。上游 Host 和 HTTPS SNI 使用目标主机，代理会重建 `X-Forwarded-*` 请求头。

对于 301/302/303/307/308 等重定向，代理会将指向目标同源地址的 `Location` 改写为客户端当前访问的代理地址，后续请求继续经过代理，状态码保持不变。支持绝对、协议相对和相对地址，保留查询参数与片段。配置了目标路径前缀时，会移除该前缀再返回给客户端。第三方源（不同协议、主机或有效端口）以及目标路径前缀以外的跳转保留原样。

一个特殊情况是跳转到不带尾斜线的前缀根路径。例如目标为 `/base` 或 `/base/`，上游返回 `Location: /base`：代理的 `/` 只能转发到 `/base/`，无法准确表示 `/base`。此时返回上游的完整绝对地址，让客户端直接访问上游，避免循环跳转；查询参数、片段和状态码保持不变。

代理不改写 HTML/JavaScript 链接、Cookie 域、Secure 属性或 Refresh 跳转，因此这些场景仍可能跳出代理或影响登录状态。客户端使用 HTTP 访问代理时，请求内容在代理程序中可读取；代理连接 HTTPS 上游时，由 Go 的 TLS 客户端加密传输和解密响应，程序默认不会记录请求体。上游连接失败时返回 HTTP 502。这是固定目标的反向代理，不是支持 CONNECT 的浏览器正向代理。

## 上游响应头超时

默认在发送完请求（包括请求体）后，最多等待上游响应头 30 秒，超时返回 502。可以在每个代理条目中配置 `response_header_timeout: 60s`，或在单目标模式使用命令行参数：

```sh
./bin/reverseproxy -response-header-timeout 60s 0.0.0.0:7789 https://www.google.com
```

支持 `500ms`、`30s`、`2m` 等非负时长，`0s` 关闭这一超时；省略或留空使用默认值。该超时不限制收到响应头后的响应体传输时间，适用于 SSE、长下载和 WebSocket。普通响应使用写缓冲，SSE 和未知长度的流式响应仍立即刷新。

## 通过上游代理转发

每个目标可独立配置 `proxy`。不设置或设为 `""` 时直连，不读取 `HTTP_PROXY`/`HTTPS_PROXY`/`ALL_PROXY`/`NO_PROXY` 环境变量。代理连接失败返回 502，不自动回退直连。

```yaml
proxies:
  - listen: "0.0.0.0:7789"
    target: "https://www.google.com"
    proxy: "socks5://127.0.0.1:1080"
  - listen: "0.0.0.0:7790"
    target: "https://www.bing.com"
    proxy: "http://127.0.0.1:3128"
  - listen: "0.0.0.0:7791"
    target: "https://example.com"
    proxy: ""
```

命令行（选项放在位置参数之前）：

```sh
./bin/reverseproxy -proxy socks5://127.0.0.1:1080 0.0.0.0:7789 https://www.google.com
./bin/reverseproxy -listen 0.0.0.0:7789 -target https://www.google.com -proxy http://127.0.0.1:3128
```

支持 `http://`、`https://`、`socks5://` 和 `socks5h://`。HTTP/HTTPS 代理访问 HTTPS 目标时使用 CONNECT 隧道；HTTPS 代理自身的 TLS 证书也会被验证。`socks5` 和 `socks5h` 均由代理端解析目标域名。不支持 SOCKS4。

认证可写为 `http://user:password@127.0.0.1:3128` 或 `socks5://user:password@127.0.0.1:1080`，分别使用 HTTP Basic 和 SOCKS5 用户名/密码认证。用户名、密码中的特殊字符须按 URL 规则编码。建议显式填写端口；省略时 HTTP 为 80、HTTPS 为 443、SOCKS5 为 1080。

## 请求日志

程序默认在终端（标准错误输出）打印每个转发请求，包含请求 ID、客户端地址、访问的 Host、方法、路径和查询参数，以及实际上游 URL。例如：

```text
请求 id=1 client="127.0.0.1:54321" host="127.0.0.1:7789" method=GET uri="/search?q=go" upstream="https://www.google.com/search?q=go"
响应 id=1 status=200 headers_duration=125ms
```

请求在转发前立即打印，响应日志使用同一 ID 关联。`headers_duration` 是从准备转发到收到上游响应头的耗时，不包含完整响应体传输时间；连接失败会记录 502。日志不读取或打印请求体，也不打印 Cookie、Authorization 等请求头，不影响流式传输和 WebSocket。URL 查询参数会原样记录。

## 验证

```sh
go test ./...
go test -race ./...
go vet ./...
```

运行本地 HTTP 转发基准，观察每请求内存分配和吞吐：

```sh
go test -run '^$' -bench BenchmarkProxyResponse -benchmem
```
