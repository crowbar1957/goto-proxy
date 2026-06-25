# goto-proxy

基于 Go 的 HTTP 代理转发服务器。用户通过浏览器访问代理地址，服务器将请求经上游代理转发至目标网站，并自动改写 HTML/CSS 中的资源链接，使后续请求继续经过代理。

## 工作原理

```
浏览器 → goto-proxy → 上游代理 → 目标网站
```

访问格式：`http://<代理地址>/<目标URL>`

例如：`http://127.0.0.1:8080/http://www.example.com`

- 用户请求头在转发过程中保持不变
- 上游代理支持 `http`、`https`、`socks5` 协议
- HTML 中的相对地址自动改写为代理地址
- CSS 文件及 HTML 内联样式中的 `url()` 相对路径自动改写
- 304 响应直接透传，利用浏览器缓存

## 环境变量

| 变量名 | 是否必填 | 默认值 | 说明 |
|--------|---------|--------|------|
| `PROXY_URL` | 必填 | — | 上游代理地址，如 `http://127.0.0.1:7890` 或 `socks5://127.0.0.1:1234` |
| `HTML_BASE_URL` | 必填 | — | 改写资源链接时使用的完整地址（含协议和端口），与监听端口无关 |
| `PORT` | 可选 | `8080` | 本代理监听端口 |
| `LOG_LEVEL` | 可选 | `INFO` | 日志级别：`DEBUG` / `INFO` / `ERROR` |

> 未配置 `PROXY_URL` 或 `HTML_BASE_URL` 时，服务启动将报错退出。

## 使用示例

### 基本启动

```bash
PROXY_URL=http://127.0.0.1:7890 \
HTML_BASE_URL=http://192.168.1.2:8080 \
./goto-proxy
```

- 监听 `0.0.0.0:8080`
- 上游代理为 `http://127.0.0.1:7890`
- 资源链接改写为 `http://192.168.1.2:8080/...`

### 自定义端口 + 调试日志

```bash
PROXY_URL=socks5://127.0.0.1:1234 \
PORT=3000 \
HTML_BASE_URL=http://my-proxy.example.com \
LOG_LEVEL=DEBUG \
./goto-proxy
```

### Docker

```bash
docker run -d \
  -p 8080:8080 \
  -e PROXY_URL=http://127.0.0.1:7890 \
  -e HTML_BASE_URL=http://192.168.1.2:8080 \
  goto-proxy
```

## 内容改写

### HTML 改写

当响应为 `text/html` 时，自动改写以下标签中的相对 URL：

| 标签 | 属性 |
|------|------|
| `<a>` | `href` |
| `<link>` | `href` |
| `<script>` | `src` |
| `<img>` | `src` |
| `<form>` | `action` |

支持 `<style>` 标签和 `style` 属性中的 CSS `url()` 改写。

### CSS 改写

当响应为 `text/css`（或 URL 以 `.css` 结尾但 `Content-Type` 缺失时），自动改写 `url()` 中的相对路径：

```css
/* 改写前 */                          /* 改写后 */
.bg {                                  .bg {
  background: url('/img/bg.jpg');         background: url('http://proxy/http://target/img/bg.jpg');
}                                      }
.icon {                                .icon {
  background-image: url(icon.png);       background-image: url(http://proxy/http://target/dir/icon.png);
}                                      }
```

### 改写规则

| 原始值 | 改写结果 |
|--------|---------|
| `/style.css` | `http://proxy/http://target/style.css` |
| `images/logo.png` | `http://proxy/http://target/dir/images/logo.png` |
| `//cdn.example.com/lib.js` | `http://proxy/https://cdn.example.com/lib.js` |
| `http://target.com/page` | `http://proxy/http://target.com/page`（同域名） |
| `https://other.com/res` | 不改写（异域名） |
| `javascript:void(0)` | 不改写 |

改写地址由 `HTML_BASE_URL` 决定，与代理实际监听端口无关。适用于代理前有 Nginx 反向代理、端口映射等场景。
