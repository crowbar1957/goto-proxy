# goto-proxy

`goto-proxy` 是一个基于 Go 的 HTTP 反向代理服务。浏览器把目标 URL 放在请求路径中，服务再通过指定的上游代理访问目标站点，并改写部分 HTML、XML 和 CSS 响应中的 URL，使后续资源请求继续经过 `goto-proxy`。

```text
浏览器 -> goto-proxy -> 上游 HTTP/HTTPS/SOCKS5 代理 -> 目标站点
```

## 请求方式

请求路径格式如下：

```text
http://<goto-proxy 地址>/<目标 URL>
```

例如：

```text
http://127.0.0.1:8080/https://www.example.com/page?q=go
```

目标 URL 只支持 `http://` 和 `https://`。请求中的查询参数会作为目标 URL 的查询参数转发。

转发时：

- 请求方法、请求体、查询参数及普通端到端请求头由 Go 的 `httputil.ReverseProxy` 转发。
- 请求的 `Host` 会设置为目标站点的 Host。
- `Accept-Encoding` 会被设置为 `gzip, deflate`，以便服务在内容改写前解压响应。
- Hop-by-hop 请求头等细节遵循 `httputil.ReverseProxy` 的标准处理方式，因此该服务不是字节级透明代理。

### 默认首页

服务启动时会尝试从当前工作目录读取一次 `index.html`：

- 如果文件存在，当请求路径中没有合法目标 URL 时返回该页面。
- 如果文件不存在，当请求路径中没有合法目标 URL 时返回 `400 Bad Request`。
- `index.html` 只在启动时加载，运行期间修改文件不会自动刷新。

## 环境变量

| 变量名          | 是否必填 | 默认值 | 说明 |
| --------------- | -------- | ------ | ---- |
| `PROXY_URL`     | 必填     | —      | 上游代理地址，仅支持 `http`、`https` 和 `socks5`，例如 `http://127.0.0.1:7890` 或 `socks5://127.0.0.1:1080` |
| `HTML_BASE_URL` | 必填     | —      | 写入改写结果的 goto-proxy 对外访问地址，必须包含 scheme 和 Host；末尾的 `/` 会被移除 |
| `PORT`          | 可选     | `8080` | 服务监听端口；监听地址固定为 `0.0.0.0:<PORT>` |
| `LOG_LEVEL`     | 可选     | `INFO` | 支持 `DEBUG`、`INFO`、`ERROR`，大小写不敏感；其他值按 `INFO` 处理 |

`HTML_BASE_URL` 与实际监听端口相互独立，可用于端口映射或前置反向代理场景。例如服务监听 `8080`，但通过 `https://proxy.example.com` 对外访问时，可以将其设置为该公网地址。

缺少 `PROXY_URL` 或 `HTML_BASE_URL`，或者地址格式不合法时，服务会启动失败。

## 构建与启动

项目要求 Go 1.22，并依赖 `golang.org/x/net`：

```bash
go build -o goto-proxy .
```

基本启动示例：

```bash
PROXY_URL=http://127.0.0.1:7890 \
HTML_BASE_URL=http://192.168.1.2:8080 \
./goto-proxy
```

此时：

- 服务监听 `0.0.0.0:8080`。
- 请求通过 `http://127.0.0.1:7890` 转发。
- 响应中的可改写 URL 以 `http://192.168.1.2:8080/` 为前缀。

自定义端口并开启调试日志：

```bash
PROXY_URL=socks5://127.0.0.1:1080 \
HTML_BASE_URL=https://proxy.example.com \
PORT=3000 \
LOG_LEVEL=DEBUG \
./goto-proxy
```

## 响应处理

以下响应会进入内容改写流程：

| 判断条件 | 处理方式 |
| -------- | -------- |
| `Content-Type` 包含 `html` 或 `xml` | 按标记文本处理属性，然后改写文本中的 CSS `url()` |
| `Content-Type` 包含 `text/css` | 改写 CSS `url()` |
| 请求目标路径以 `.css` 结尾 | 即使 `Content-Type` 缺失或不准确，也按 CSS 处理 |

其他响应体直接透传。`304 Not Modified` 响应始终直接透传，不读取或改写响应体。

进入改写流程的响应会被完整读入内存。无压缩或 `identity` 内容会直接处理，`gzip`、`deflate` 和 `zlib` 内容会先解压；改写后的响应不再压缩，并会移除 `Content-Encoding`、重新计算 `Content-Length`。当前实现不支持解压其他 `Content-Encoding`，目标站点应遵循请求中的 `Accept-Encoding: gzip, deflate`。

上游连接失败，或者读取、解压、改写响应失败时，服务会返回 `502 Bad Gateway`。

## URL 改写规则

以下示例假定：

```text
目标 URL:      https://www.example.com/dir/page.html
HTML_BASE_URL: http://proxy.local:8080
```

### HTML 和 XML 属性

程序会扫描所有起始标签中带值的属性，而不是只处理特定标签或 `href`、`src` 等特定属性。属性值只有在符合下列形式时才会尝试改写：

- 以 `/`、`./` 或 `../` 开头的相对引用。
- `http://` 或 `https://` 绝对 URL，且与当前目标属于同一可注册主域。
- `//host/path` 形式的协议相对 URL，且 Host 属于同一可注册主域。
- `host/path` 形式的无 scheme URL，且 Host 属于同一可注册主域；改写时使用当前目标 URL 的 scheme。

属性值中的普通裸相对路径（如 `images/logo.png`）不会被改写。这与 CSS `url()` 的处理方式不同。

| 原始属性值 | 改写结果 |
| ---------- | -------- |
| `/style.css` | `http://proxy.local:8080/https://www.example.com/style.css` |
| `./image.png` | `http://proxy.local:8080/https://www.example.com/dir/image.png` |
| `../image.png` | `http://proxy.local:8080/https://www.example.com/image.png` |
| `images/logo.png` | 不改写 |
| `cdn.example.com/lib.js` | `http://proxy.local:8080/https://cdn.example.com/lib.js` |
| `//cdn.example.com/lib.js` | `http://proxy.local:8080/https://cdn.example.com/lib.js` |
| `https://api.example.com/data` | `http://proxy.local:8080/https://api.example.com/data` |
| `https://other.example.net/data` | 不改写 |

对于包含空白字符的属性值，程序只使用第一个空白字符前的部分判断和改写，并原样保留剩余内容；它没有实现完整的 `srcset` 等复合属性语法解析。

### CSS `url()`

HTML/XML 文本和 CSS 响应中的 `url()` 都会通过文本匹配处理，支持双引号、单引号和无引号形式。CSS 中的普通裸相对路径会相对于当前目标 URL 解析。

```css
/* 改写前 */
.hero {
    background-image: url("../images/hero.jpg");
}

/* 改写后 */
.hero {
    background-image: url("http://proxy.local:8080/https://www.example.com/images/hero.jpg");
}
```

对于 HTML/XML 响应，CSS `url()` 匹配会作用于整个响应文本，并不只限于 `<style>` 标签和 `style` 属性。

### 域名判断

域名比较基于公共后缀规则：根域名及同一可注册主域下的各级子域会视为同域。例如 `www.example.co.uk`、`cdn.example.co.uk` 和 `example.co.uk` 可相互改写。

- 端口不参与域名比较，但会保留在最终 URL 中。
- IP 地址和无法提取可注册主域的主机名只允许 Host 精确匹配。
- `evil-example.com` 与 `example.com` 不会因为字符串后缀相似而被视为同域。

### 不改写的值

以下内容保持不变：

- 已经以 `HTML_BASE_URL/` 开头的 URL。
- 以 `#` 或 `?` 开头的引用。
- `javascript:`、`about:blank` 以及 `data:`、`mailto:` 等非 HTTP(S) URL。
- 指向其他可注册主域的绝对或协议相对 URL。
- 无法解析为 URL 的值。

## 未处理的内容

当前实现只改写响应体中的上述 URL，不会改写 `Location`、`Set-Cookie` 等响应头。内容改写采用轻量文本扫描和正则匹配，而不是完整的 HTML、XML 或 CSS 解析器；复杂或非标准语法可能不会按浏览器语义处理。

## 部署注意事项

服务固定监听所有网络接口，并允许请求者在路径中指定任意合法的 HTTP(S) 目标。不要在没有访问控制的情况下直接暴露到不可信网络；如需公网使用，应在前置代理或防火墙中配置认证、来源限制和目标访问策略。
