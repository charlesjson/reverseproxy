# Go 反向代理

将指定监听地址收到的 HTTP 请求转发到固定的 HTTP/HTTPS 上游。默认 `http` 模式提供 HTTP 反向代理，支持请求方法、请求体、路径、查询参数、流式响应和 WebSocket 升级；`raw` 模式逐字节透传原始 HTTP 报文。HTTPS 上游使用系统 CA 验证证书。

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

每项可配置 `mode: http`（默认）或 `mode: raw`，两种模式可以在不同端口一起运行。仍兼容旧版单目标 YAML（顶层 `listen`、`target` 和可选的 `mode`、`proxy`、`response_header_timeout`），单目标配置可通过对应的 `-mode`/`-listen`/`-target`/`-proxy`/`-response-header-timeout` 参数覆盖。`proxies` 列表不能与顶层单目标配置或命令行单目标参数混用，即使顶层字段显式填写为 `null` 或空字符串也会报错。列表不能为空或 `null`，每项必须包含监听地址和目标 URL，监听地址不能重复。

选项须放在位置参数之前。位置参数与 `-listen`/`-target` 不可同时使用。未知 YAML 字段会报错。使用 `-h` 查看帮助，Ctrl+C 或 SIGTERM 可停止服务，普通请求有最多 10 秒的退出等待时间。

监听端提供 HTTP；`0.0.0.0` 表示监听所有 IPv4 网卡，只允许本机访问时使用 `127.0.0.1`。目标 URL 如包含路径前缀，该前缀会拼接到请求路径之前。固定查询参数按名称和值补齐缺少的次数：请求中已有相同参数时不会再追加，请求中有意重复的参数会保留。例如目标含 `?key=value`，重定向地址已含 `key=value` 时，下一跳不会重复添加。有效查询串保留原有顺序和编码；无法解析的请求参数仍由标准库清理。上游 Host 和 HTTPS SNI 使用目标主机，代理会重建 `X-Forwarded-*` 请求头。

对于 301/302/303/307/308 等重定向，代理会将指向目标同源地址的 `Location` 改写为客户端当前访问的代理地址，后续请求继续经过代理，状态码保持不变。支持绝对、协议相对和相对地址，保留查询参数与片段。配置了目标路径前缀时，会移除该前缀再返回给客户端。第三方源（不同协议、主机或有效端口）以及目标路径前缀以外的跳转保留原样。

一个特殊情况是跳转到不带尾斜线的前缀根路径。例如目标为 `/base` 或 `/base/`，上游返回 `Location: /base`：代理的 `/` 只能转发到 `/base/`，无法准确表示 `/base`。此时返回上游的完整绝对地址，让客户端直接访问上游，避免循环跳转；查询参数、片段和状态码保持不变。

代理不改写 HTML/JavaScript 链接、Cookie 域、Secure 属性或 Refresh 跳转，因此这些场景仍可能跳出代理或影响登录状态。客户端使用 HTTP 访问代理时，请求内容在代理程序中可读取；代理连接 HTTPS 上游时，由 Go 的 TLS 客户端加密传输和解密响应，程序默认不会记录请求体。普通 `http` 模式在上游连接失败时返回 HTTP 502。这是固定目标的反向代理，不是支持 CONNECT 的浏览器正向代理。

## Raw HTTP 透传

使用 `-mode raw` 或 YAML 的 `mode: raw`：

```sh
./bin/reverseproxy -mode raw 127.0.0.1:7791 https://www.google.com
curl --http1.1 -H 'Host: www.google.com' http://127.0.0.1:7791/
```

```yaml
proxies:
  - mode: http
    listen: "127.0.0.1:7789"
    target: "https://www.google.com"
  - mode: raw
    listen: "127.0.0.1:7791"
    target: "https://www.google.com"
    proxy: "" # 也支持 HTTP/HTTPS CONNECT 和 SOCKS5 上游代理
```

监听端接收明文 HTTP/1.x 字节。每个客户端 TCP 连接对应一个独立的上游连接，请求行、路径、查询串、头部大小写与顺序、重复头、chunked 分块、trailer 和响应字节均原样传递。长连接、HTTP 流水线和协议升级后的双向数据继续通过同一连接传输。

**Host 由客户端提供。** raw 模式不会重写 Host、添加 `X-Forwarded-*`、拼接目标路径或查询参数，也不会改写 `Location`、解压或重新分块。`target` 只允许协议、主机和端口（可带末尾 `/`），带路径前缀或查询参数会报错。目标为 `https://` 时，程序建立并验证上游 TLS，SNI 使用目标主机，只协商 HTTP/1.1。

`proxy` 支持 `http`、`https`、`socks5` 和 `socks5h`，包含 HTTP Basic 和 SOCKS5 用户名/密码认证。HTTP/HTTPS 上游代理对所有目标都使用 CONNECT，因此代理须允许 CONNECT 到相应目标端口；认证信息只用于建立隧道。SOCKS5 域名由代理端解析。未配置时直连，不读取代理环境变量，失败也不回退直连。

连接建立、代理协商和 TLS 握手合计最多等待 30 秒，传输开始后不设置 HTTP 响应头超时；`response_header_timeout` 仅作用于普通 `http` 模式。连接失败时关闭客户端连接并记录原因，不注入 HTTP 错误报文。退出时关闭 raw 监听和全部活动连接，包括尚在握手的连接。

raw 日志按连接记录 ID、客户端和目标地址、双向传输字节数与连接时长，不解析或打印请求头、请求体和响应体。

## 上游响应头超时

普通 `http` 模式默认在发送完请求（包括请求体）后，最多等待上游响应头 30 秒，超时返回 502。可以在每个代理条目中配置 `response_header_timeout: 60s`，或在单目标模式使用命令行参数：

```sh
./bin/reverseproxy -response-header-timeout 60s 0.0.0.0:7789 https://www.google.com
```

支持 `500ms`、`30s`、`2m` 等非负时长，`0s` 关闭这一超时；省略或留空使用默认值。该超时不限制收到响应头后的响应体传输时间，适用于 SSE、长下载和 WebSocket。普通响应使用写缓冲，SSE 和未知长度的流式响应仍立即刷新。

## 通过上游代理转发

每个目标可独立配置 `proxy`。不设置或设为 `""` 时直连，不读取 `HTTP_PROXY`/`HTTPS_PROXY`/`ALL_PROXY`/`NO_PROXY` 环境变量。普通 `http` 模式下代理连接失败返回 502，不自动回退直连。

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

普通 `http` 模式默认在终端（标准错误输出）打印每个转发请求，包含请求 ID、客户端地址、访问的 Host、方法、路径和查询参数，以及实际上游 URL。例如：

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
