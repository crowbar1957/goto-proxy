package main

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// --------------- 日志 ---------------

type ctxKey string

const targetURLKey ctxKey = "targetURL"

const (
	levelDebug = 0
	levelInfo  = 1
	levelError = 2
)

var currentLevel = levelInfo

func logDebug(format string, args ...interface{}) {
	if currentLevel <= levelDebug {
		log.Printf("[DEBUG] "+format, args...)
	}
}

func logInfo(format string, args ...interface{}) {
	if currentLevel <= levelInfo {
		log.Printf("[INFO]  "+format, args...)
	}
}

func logError(format string, args ...interface{}) {
	if currentLevel <= levelError {
		log.Printf("[ERROR] "+format, args...)
	}
}

// --------------- 正则 ---------------

var (
	// href/src/action 属性
	reHrefSrcDQ = regexp.MustCompile("(?i)((?:href|src|action)\\s*=\\s*\")([^\"]*)(\")")
	reHrefSrcSQ = regexp.MustCompile("(?i)((?:href|src|action)\\s*=\\s*')([^']*)(')")
	// CSS url()
	reCSSURLDQ = regexp.MustCompile("(?i)url\\(\\s*\"([^\"]*)\"")
	reCSSURLSQ = regexp.MustCompile("(?i)url\\(\\s*'([^']*)'")
	reCSSURLNQ = regexp.MustCompile("(?i)url\\(([^)]+)\\)")
)

// --------------- 入口 ---------------

func main() {
	// 日志级别
	switch strings.ToUpper(os.Getenv("LOG_LEVEL")) {
	case "DEBUG":
		currentLevel = levelDebug
	case "ERROR":
		currentLevel = levelError
	default:
		currentLevel = levelInfo
	}

	// 必填环境变量
	proxyStr := os.Getenv("PROXY_URL")
	if proxyStr == "" {
		log.Fatal("未配置代理地址，请通过 PROXY_URL 环境变量配置，例如：http://127.0.0.1:7890")
	}
	htmlBaseURL := os.Getenv("HTML_BASE_URL")
	if htmlBaseURL == "" {
		log.Fatal("未配置 HTML 改写地址，请通过 HTML_BASE_URL 环境变量配置，例如：http://192.168.1.2:1234")
	}
	htmlBaseURL = strings.TrimRight(htmlBaseURL, "/")

	// 可选环境变量
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	proxyURL, err := url.Parse(proxyStr)
	if err != nil {
		log.Fatalf("解析代理地址失败: %v", err)
	}

	// Director：改写请求目标
	director := func(req *http.Request) {
		req.Header.Set("Accept-Encoding", "gzip, deflate")

		path := strings.TrimPrefix(req.URL.Path, "/")
		if strings.HasPrefix(path, "http:/") && !strings.HasPrefix(path, "http://") {
			path = strings.Replace(path, "http:/", "http://", 1)
		} else if strings.HasPrefix(path, "https:/") && !strings.HasPrefix(path, "https://") {
			path = strings.Replace(path, "https:/", "https://", 1)
		}

		target, err := url.Parse(path)
		if err != nil {
			logError("解析目标 URL 失败 [%s]: %v", path, err)
			return
		}

		*req = *req.WithContext(context.WithValue(req.Context(), targetURLKey, path))
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = target.Path
		req.Host = target.Host
	}

	// Transport：禁用自动压缩，由 modifyResponse 手动处理
	transport := &http.Transport{
		DisableCompression:  true,
		MaxIdleConns:        1000,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}

	switch proxyURL.Scheme {
	case "http", "https":
		transport.Proxy = http.ProxyURL(proxyURL)
	case "socks5":
		socksDialer, err := proxy.FromURL(proxyURL, proxy.Direct)
		if err != nil {
			log.Fatalf("创建 SOCKS5 代理拨号器失败: %v", err)
		}
		if ctx, ok := socksDialer.(proxy.ContextDialer); ok {
			transport.DialContext = ctx.DialContext
		} else {
			transport.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
				return socksDialer.Dial(network, addr)
			}
		}
	default:
		log.Fatalf("不支持的代理协议: %s（仅支持 http、https、socks5）", proxyURL.Scheme)
	}

	// ModifyResponse：解压 + 改写 HTML/CSS
	modifyResponse := func(resp *http.Response) error {
		if resp.StatusCode == http.StatusNotModified {
			return nil
		}

		ct := strings.ToLower(resp.Header.Get("Content-Type"))
		isHTML := strings.Contains(ct, "text/html")
		isCSS := strings.Contains(ct, "text/css")

		if !isHTML && !isCSS {
			if strings.HasSuffix(strings.ToLower(resp.Request.URL.Path), ".css") {
				isCSS = true
			}
		}
		if !isHTML && !isCSS {
			return nil
		}

		targetURL, _ := resp.Request.Context().Value(targetURLKey).(string)
		if targetURL == "" {
			return nil
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			logError("读取响应体失败 [%s]: %v", targetURL, err)
			return err
		}
		resp.Body.Close()

		body = decompress(body, resp.Header.Get("Content-Encoding"))

		var rewritten []byte
		if isHTML {
			rewritten = rewriteHTML(body, targetURL, htmlBaseURL)
		} else {
			rewritten = rewriteCSS(body, targetURL, htmlBaseURL)
		}

		logDebug("改写 %s: %d -> %d bytes", targetURL, len(body), len(rewritten))

		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Content-Length")
		resp.ContentLength = int64(len(rewritten))
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(rewritten)))
		resp.Body = io.NopCloser(bytes.NewReader(rewritten))
		return nil
	}

	p := &httputil.ReverseProxy{
		Director:       director,
		Transport:      transport,
		ModifyResponse: modifyResponse,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logError("代理错误 [%s %s]: %v", r.Method, r.URL.Path, err)
			http.Error(w, "代理错误: "+err.Error(), http.StatusBadGateway)
		},
	}

	// 外层 Handler：校验路径 + panic 恢复
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer func() {
			if r := recover(); r != nil {
				logError("请求处理 panic [%s]: %v", req.URL.Path, r)
				http.Error(w, fmt.Sprintf("内部错误: %v", r), http.StatusInternalServerError)
			}
		}()

		logInfo("请求: %s %s", req.Method, req.URL.Path)

		path := strings.TrimPrefix(req.URL.Path, "/")
		if strings.HasPrefix(path, "http:/") && !strings.HasPrefix(path, "http://") {
			path = "http://" + path[6:]
		} else if strings.HasPrefix(path, "https:/") && !strings.HasPrefix(path, "https://") {
			path = "https://" + path[7:]
		}
		if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
			http.Error(w, "请在路径中指定目标 URL，例如 /http://example.com", http.StatusBadRequest)
			return
		}

		p.ServeHTTP(w, req)
	})

	addr := "0.0.0.0:" + port
	logInfo("启动地址: %s", addr)
	logInfo("上游代理: %s", proxyStr)
	logInfo("HTML 改写地址: %s", htmlBaseURL)
	log.Fatal(http.ListenAndServe(addr, handler))
}

// --------------- 解压 ---------------

func decompress(body []byte, encoding string) []byte {
	switch strings.ToLower(encoding) {
	case "gzip":
		if r, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
			if decoded, err := io.ReadAll(r); err == nil {
				body = decoded
			}
			r.Close()
		}
	case "deflate", "zlib":
		if r, err := zlib.NewReader(bytes.NewReader(body)); err == nil {
			if decoded, err := io.ReadAll(r); err == nil {
				body = decoded
			}
			r.Close()
		}
	}
	return body
}

// --------------- URL 改写 ---------------

// rewriteURL 将单个 URL 值改写为代理地址，返回新值和是否发生了改写。
//
// 改写规则（以 target=http://b.com/dir/page.html, proxy=http://p/ 为例）：
//
//	/style.css          → http://p/http://b.com/style.css
//	file.css            → http://p/http://b.com/dir/file.css
//	//cdn.com/lib.js    → http://p/https://cdn.com/lib.js
//	http://b.com/page   → http://p/http://b.com/page  （同域名绝对地址）
//	https://other.com   → 不改写（异域名绝对地址）
//	javascript:...      → 不改写
func rewriteURL(val, proxyPrefix, baseTarget, targetPath, targetHost, targetScheme string) (string, bool) {
	lower := strings.ToLower(val)
	if val == "" || val == "#" || strings.HasPrefix(lower, "javascript:") || lower == "about:blank" {
		return val, false
	}
	if strings.HasPrefix(val, proxyPrefix) {
		return val, false
	}
	// 绝对 URL：同域名改写，异域名跳过
	if strings.HasPrefix(val, "http://") || strings.HasPrefix(val, "https://") {
		host := val
		if i := strings.Index(val, "://"); i >= 0 {
			host = val[i+3:]
		}
		if i := strings.Index(host, "/"); i >= 0 {
			host = host[:i]
		}
		if host == targetHost {
			return proxyPrefix + val, true
		}
		return val, false
	}
	// 协议相对
	if strings.HasPrefix(val, "//") {
		return proxyPrefix + targetScheme + ":" + val, true
	}
	// 根相对
	if strings.HasPrefix(val, "/") {
		return proxyPrefix + baseTarget + val, true
	}
	// 路径相对
	return proxyPrefix + baseTarget + targetPath + val, true
}

// --------------- CSS url() 改写 ---------------

func rewriteCSSURLs(css, proxyPrefix, baseTarget, targetPath, targetHost, targetScheme string) string {
	// url("...")
	css = reCSSURLDQ.ReplaceAllStringFunc(css, func(m string) string {
		parts := reCSSURLDQ.FindStringSubmatch(m)
		if len(parts) < 2 {
			return m
		}
		if v, ok := rewriteURL(parts[1], proxyPrefix, baseTarget, targetPath, targetHost, targetScheme); ok {
			return `url("` + v + `"`
		}
		return m
	})
	// url('...')
	css = reCSSURLSQ.ReplaceAllStringFunc(css, func(m string) string {
		parts := reCSSURLSQ.FindStringSubmatch(m)
		if len(parts) < 2 {
			return m
		}
		if v, ok := rewriteURL(parts[1], proxyPrefix, baseTarget, targetPath, targetHost, targetScheme); ok {
			return "url('" + v + "'"
		}
		return m
	})
	// url(...) 无引号
	css = reCSSURLNQ.ReplaceAllStringFunc(css, func(m string) string {
		parts := reCSSURLNQ.FindStringSubmatch(m)
		if len(parts) < 2 {
			return m
		}
		val := strings.TrimSpace(parts[1])
		if len(val) == 0 || val[0] == '"' || val[0] == '\'' {
			return m
		}
		if v, ok := rewriteURL(val, proxyPrefix, baseTarget, targetPath, targetHost, targetScheme); ok {
			return "url(" + v + ")"
		}
		return m
	})
	return css
}

// --------------- HTML 改写 ---------------

func rewriteHTML(body []byte, targetURL string, htmlBaseURL string) []byte {
	tu, err := url.Parse(targetURL)
	if err != nil {
		return body
	}
	baseTarget, targetPath, proxyPrefix := urlContext(tu, htmlBaseURL)
	html := string(body)

	// href/src/action
	for _, re := range []*regexp.Regexp{reHrefSrcDQ, reHrefSrcSQ} {
		html = re.ReplaceAllStringFunc(html, func(m string) string {
			parts := re.FindStringSubmatch(m)
			if len(parts) != 4 {
				return m
			}
			if v, ok := rewriteURL(parts[2], proxyPrefix, baseTarget, targetPath, tu.Host, tu.Scheme); ok {
				return parts[1] + v + parts[3]
			}
			return m
		})
	}

	// style 中的 url()
	html = rewriteCSSURLs(html, proxyPrefix, baseTarget, targetPath, tu.Host, tu.Scheme)
	return []byte(html)
}

// --------------- CSS 文件改写 ---------------

func rewriteCSS(body []byte, targetURL string, htmlBaseURL string) []byte {
	tu, err := url.Parse(targetURL)
	if err != nil {
		return body
	}
	baseTarget, targetPath, proxyPrefix := urlContext(tu, htmlBaseURL)
	css := rewriteCSSURLs(string(body), proxyPrefix, baseTarget, targetPath, tu.Host, tu.Scheme)
	return []byte(css)
}

// --------------- 工具函数 ---------------

// urlContext 从目标 URL 中提取用于改写的上下文信息。
func urlContext(tu *url.URL, htmlBaseURL string) (baseTarget, targetPath, proxyPrefix string) {
	baseTarget = tu.Scheme + "://" + tu.Host
	targetPath = tu.Path
	if !strings.HasSuffix(targetPath, "/") {
		if i := strings.LastIndex(targetPath, "/"); i >= 0 {
			targetPath = targetPath[:i+1]
		}
	}
	proxyPrefix = htmlBaseURL + "/"
	return
}
