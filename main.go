package main

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
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

type contextKey string

const targetURLKey contextKey = "targetURL"

// -------------------- 配置 --------------------

// Config 集中管理应用配置，避免业务组件直接依赖环境变量。
type Config struct {
	ProxyURL    *url.URL
	ProxyRaw    string
	HTMLBaseURL string
	Port        string
	LogLevel    LogLevel
}

func LoadConfig() (Config, error) {
	proxyRaw := strings.TrimSpace(os.Getenv("PROXY_URL"))
	if proxyRaw == "" {
		return Config{}, errors.New(
			"未配置代理地址，请通过 PROXY_URL 环境变量配置，例如：http://127.0.0.1:7890",
		)
	}

	proxyURL, err := url.Parse(proxyRaw)
	if err != nil || proxyURL.Host == "" {
		return Config{}, fmt.Errorf("代理地址无效: %s", proxyRaw)
	}

	switch proxyURL.Scheme {
	case "http", "https", "socks5":
	default:
		return Config{}, fmt.Errorf(
			"不支持的代理协议: %s（仅支持 http、https、socks5）",
			proxyURL.Scheme,
		)
	}

	htmlBaseURL := strings.TrimRight(
		strings.TrimSpace(os.Getenv("HTML_BASE_URL")),
		"/",
	)
	if htmlBaseURL == "" {
		return Config{}, errors.New(
			"未配置 HTML 改写地址，请通过 HTML_BASE_URL 环境变量配置，例如：http://192.168.1.2:1234",
		)
	}

	baseURL, err := url.Parse(htmlBaseURL)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return Config{}, fmt.Errorf("HTML 改写地址无效: %s", htmlBaseURL)
	}

	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8080"
	}

	return Config{
		ProxyURL:    proxyURL,
		ProxyRaw:    proxyRaw,
		HTMLBaseURL: htmlBaseURL,
		Port:        port,
		LogLevel:    ParseLogLevel(os.Getenv("LOG_LEVEL")),
	}, nil
}

// -------------------- 日志 --------------------

type LogLevel int

const (
	LevelDebug LogLevel = iota
	LevelInfo
	LevelError
)

type Logger struct {
	level LogLevel
	base  *log.Logger
}

func NewLogger(level LogLevel) *Logger {
	return &Logger{
		level: level,
		base:  log.Default(),
	}
}

func ParseLogLevel(value string) LogLevel {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "DEBUG":
		return LevelDebug
	case "ERROR":
		return LevelError
	default:
		return LevelInfo
	}
}

func (l *Logger) Debug(format string, args ...any) {
	if l.level <= LevelDebug {
		l.base.Printf("[DEBUG] "+format, args...)
	}
}

func (l *Logger) Info(format string, args ...any) {
	if l.level <= LevelInfo {
		l.base.Printf("[INFO]  "+format, args...)
	}
}

func (l *Logger) Error(format string, args ...any) {
	if l.level <= LevelError {
		l.base.Printf("[ERROR] "+format, args...)
	}
}

// -------------------- 内容改写 --------------------

var (
	htmlAttributeDoublePattern = regexp.MustCompile(
		`(?i)((?:href|src|action)\s*=\s*")([^"]*)(")`,
	)
	htmlAttributeSinglePattern = regexp.MustCompile(
		`(?i)((?:href|src|action)\s*=\s*')([^']*)(')`,
	)
	cssDoublePattern = regexp.MustCompile(
		`(?i)url\(\s*"([^"]*)"`,
	)
	cssSinglePattern = regexp.MustCompile(
		`(?i)url\(\s*'([^']*)'`,
	)
	cssPlainPattern = regexp.MustCompile(
		`(?i)url\(([^)]+)\)`,
	)
)

type RewriteContext struct {
	BaseTarget string
	BasePath   string
	Host       string
	Scheme     string
}

// Rewriter 封装 HTML、CSS 和 URL 改写规则。
type Rewriter struct {
	proxyPrefix string
}

func NewRewriter(htmlBaseURL string) *Rewriter {
	return &Rewriter{
		proxyPrefix: strings.TrimRight(htmlBaseURL, "/") + "/",
	}
}

func (r *Rewriter) RewriteHTML(body []byte, targetRaw string) []byte {
	rewriteContext, ok := r.newContext(targetRaw)
	if !ok {
		return body
	}

	html := string(body)

	for _, pattern := range []*regexp.Regexp{
		htmlAttributeDoublePattern,
		htmlAttributeSinglePattern,
	} {
		html = pattern.ReplaceAllStringFunc(
			html,
			func(match string) string {
				parts := pattern.FindStringSubmatch(match)
				if len(parts) != 4 {
					return match
				}

				rewritten, changed := r.rewriteURL(
					parts[2],
					rewriteContext,
				)
				if !changed {
					return match
				}

				return parts[1] + rewritten + parts[3]
			},
		)
	}

	return []byte(r.rewriteCSSURLs(html, rewriteContext))
}

func (r *Rewriter) RewriteCSS(body []byte, targetRaw string) []byte {
	rewriteContext, ok := r.newContext(targetRaw)
	if !ok {
		return body
	}

	return []byte(
		r.rewriteCSSURLs(string(body), rewriteContext),
	)
}

func (r *Rewriter) newContext(targetRaw string) (RewriteContext, bool) {
	target, err := url.Parse(targetRaw)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return RewriteContext{}, false
	}

	basePath := target.Path
	if !strings.HasSuffix(basePath, "/") {
		if index := strings.LastIndex(basePath, "/"); index >= 0 {
			basePath = basePath[:index+1]
		}
	}

	return RewriteContext{
		BaseTarget: target.Scheme + "://" + target.Host,
		BasePath:   basePath,
		Host:       target.Host,
		Scheme:     target.Scheme,
	}, true
}

func (r *Rewriter) rewriteURL(
	value string,
	rewriteContext RewriteContext,
) (string, bool) {
	lowerValue := strings.ToLower(value)

	if value == "" ||
		value == "#" ||
		strings.HasPrefix(lowerValue, "javascript:") ||
		lowerValue == "about:blank" ||
		strings.HasPrefix(value, r.proxyPrefix) {
		return value, false
	}

	if absoluteURL, err := url.Parse(value); err == nil && absoluteURL.IsAbs() {
		if absoluteURL.Host == rewriteContext.Host {
			return r.proxyPrefix + value, true
		}

		return value, false
	}

	if strings.HasPrefix(value, "//") {
		return r.proxyPrefix +
			rewriteContext.Scheme +
			":" +
			value, true
	}

	if strings.HasPrefix(value, "/") {
		return r.proxyPrefix +
			rewriteContext.BaseTarget +
			value, true
	}

	return r.proxyPrefix +
		rewriteContext.BaseTarget +
		rewriteContext.BasePath +
		value, true
}

func (r *Rewriter) rewriteCSSURLs(
	css string,
	rewriteContext RewriteContext,
) string {
	css = r.rewriteQuotedCSSURL(
		css,
		cssDoublePattern,
		`url("`,
		`"`,
		rewriteContext,
	)

	css = r.rewriteQuotedCSSURL(
		css,
		cssSinglePattern,
		"url('",
		"'",
		rewriteContext,
	)

	return cssPlainPattern.ReplaceAllStringFunc(
		css,
		func(match string) string {
			parts := cssPlainPattern.FindStringSubmatch(match)
			if len(parts) != 2 {
				return match
			}

			value := strings.TrimSpace(parts[1])
			if value == "" ||
				value[0] == '\'' ||
				value[0] == '"' {
				return match
			}

			rewritten, changed := r.rewriteURL(
				value,
				rewriteContext,
			)
			if !changed {
				return match
			}

			return "url(" + rewritten + ")"
		},
	)
}

func (r *Rewriter) rewriteQuotedCSSURL(
	css string,
	pattern *regexp.Regexp,
	prefix string,
	suffix string,
	rewriteContext RewriteContext,
) string {
	return pattern.ReplaceAllStringFunc(
		css,
		func(match string) string {
			parts := pattern.FindStringSubmatch(match)
			if len(parts) != 2 {
				return match
			}

			rewritten, changed := r.rewriteURL(
				parts[1],
				rewriteContext,
			)
			if !changed {
				return match
			}

			return prefix + rewritten + suffix
		},
	)
}

// -------------------- 响应解压 --------------------

func Decompress(body []byte, encoding string) ([]byte, error) {
	var reader io.ReadCloser
	var err error

	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return body, nil

	case "gzip":
		reader, err = gzip.NewReader(bytes.NewReader(body))

	case "deflate", "zlib":
		reader, err = zlib.NewReader(bytes.NewReader(body))

	default:
		// 未识别的编码交给调用端按原内容处理。
		return body, nil
	}

	if err != nil {
		return nil, err
	}
	defer reader.Close()

	decoded, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	return decoded, nil
}

// -------------------- 代理 Transport --------------------

func NewTransport(proxyURL *url.URL) (*http.Transport, error) {
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
		if err := configureSOCKS5Transport(transport, proxyURL); err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf(
			"不支持的代理协议: %s",
			proxyURL.Scheme,
		)
	}

	return transport, nil
}

func configureSOCKS5Transport(
	transport *http.Transport,
	proxyURL *url.URL,
) error {
	dialer, err := proxy.FromURL(proxyURL, proxy.Direct)
	if err != nil {
		return fmt.Errorf(
			"创建 SOCKS5 代理拨号器失败: %w",
			err,
		)
	}

	if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
		transport.DialContext = contextDialer.DialContext
		return nil
	}

	transport.DialContext = func(
		_ context.Context,
		network string,
		address string,
	) (net.Conn, error) {
		return dialer.Dial(network, address)
	}

	return nil
}

// -------------------- 代理服务 --------------------

// ProxyServer 聚合代理所需依赖，负责完整的 HTTP 请求生命周期。
type ProxyServer struct {
	logger       *Logger
	rewriter     *Rewriter
	reverseProxy *httputil.ReverseProxy
	defaultIndex []byte
}

func NewProxyServer(
	config Config,
	logger *Logger,
) (*ProxyServer, error) {
	transport, err := NewTransport(config.ProxyURL)
	if err != nil {
		return nil, err
	}

	server := &ProxyServer{
		logger:   logger,
		rewriter: NewRewriter(config.HTMLBaseURL),
	}

	server.loadDefaultIndex("index.html")

	server.reverseProxy = &httputil.ReverseProxy{
		Director:       server.directRequest,
		Transport:      transport,
		ModifyResponse: server.modifyResponse,
		ErrorHandler:   server.handleProxyError,
	}

	return server, nil
}

func (s *ProxyServer) loadDefaultIndex(path string) {
	content, err := os.ReadFile(path)
	if err == nil {
		s.defaultIndex = content
		s.logger.Info("已加载默认首页: %s", path)
		return
	}

	if !os.IsNotExist(err) {
		s.logger.Error(
			"读取默认首页 %s 失败: %v",
			path,
			err,
		)
	}
}

func (s *ProxyServer) Handler() http.Handler {
	handler := http.HandlerFunc(s.serveHTTP)

	return s.recoverMiddleware(
		s.loggingMiddleware(handler),
	)
}

func (s *ProxyServer) serveHTTP(
	writer http.ResponseWriter,
	request *http.Request,
) {
	targetRaw, ok := NormalizeTargetPath(request.URL.Path)
	if !ok {
		s.serveDefaultIndex(writer, request)
		return
	}

	// Director 仍从请求路径中提取目标地址。
	request.URL.Path = "/" + targetRaw
	s.reverseProxy.ServeHTTP(writer, request)
}

func (s *ProxyServer) serveDefaultIndex(
	writer http.ResponseWriter,
	request *http.Request,
) {
	if s.defaultIndex == nil {
		http.Error(
			writer,
			"请在路径中指定目标 URL，例如 /http://example.com",
			http.StatusBadRequest,
		)
		return
	}

	writer.Header().Set(
		"Content-Type",
		"text/html; charset=utf-8",
	)
	writer.Header().Set(
		"Content-Length",
		fmt.Sprint(len(s.defaultIndex)),
	)
	writer.WriteHeader(http.StatusOK)

	if request.Method != http.MethodHead {
		_, _ = writer.Write(s.defaultIndex)
	}
}

func (s *ProxyServer) directRequest(request *http.Request) {
	targetRaw, ok := NormalizeTargetPath(request.URL.Path)
	if !ok {
		s.logger.Error(
			"请求目标 URL 无效: %s",
			request.URL.Path,
		)
		return
	}

	target, err := url.Parse(targetRaw)
	if err != nil {
		s.logger.Error(
			"解析目标 URL 失败 [%s]: %v",
			targetRaw,
			err,
		)
		return
	}

	request.Header.Set(
		"Accept-Encoding",
		"gzip, deflate",
	)

	requestContext := context.WithValue(
		request.Context(),
		targetURLKey,
		targetRaw,
	)
	*request = *request.WithContext(requestContext)

	request.URL.Scheme = target.Scheme
	request.URL.Host = target.Host
	request.URL.Path = target.Path
	request.URL.RawPath = target.RawPath
	request.URL.RawQuery = target.RawQuery
	request.Host = target.Host
}

func (s *ProxyServer) modifyResponse(
	response *http.Response,
) error {
	if response.StatusCode == http.StatusNotModified ||
		!IsRewritableResponse(response) {
		return nil
	}

	targetRaw, _ := response.Request.Context().
		Value(targetURLKey).(string)

	if targetRaw == "" {
		return nil
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf(
			"读取响应体失败 [%s]: %w",
			targetRaw,
			err,
		)
	}

	if err := response.Body.Close(); err != nil {
		s.logger.Debug(
			"关闭响应体失败 [%s]: %v",
			targetRaw,
			err,
		)
	}

	body, err = Decompress(
		body,
		response.Header.Get("Content-Encoding"),
	)
	if err != nil {
		return fmt.Errorf(
			"解压响应体失败 [%s]: %w",
			targetRaw,
			err,
		)
	}

	contentType := strings.ToLower(
		response.Header.Get("Content-Type"),
	)

	var rewritten []byte

	switch {
	case strings.Contains(contentType, "text/html"),
		strings.Contains(contentType, "application/xml"):
		rewritten = s.rewriter.RewriteHTML(body, targetRaw)

	default:
		rewritten = s.rewriter.RewriteCSS(body, targetRaw)
	}

	s.logger.Debug(
		"改写 %s: %d -> %d bytes",
		targetRaw,
		len(body),
		len(rewritten),
	)

	SetResponseBody(response, rewritten)
	return nil
}

func IsRewritableResponse(response *http.Response) bool {
	contentType := strings.ToLower(
		response.Header.Get("Content-Type"),
	)

	if strings.Contains(contentType, "text/html") ||
		strings.Contains(contentType, "application/xml") ||
		strings.Contains(contentType, "text/css") {
		return true
	}

	return strings.HasSuffix(
		strings.ToLower(response.Request.URL.Path),
		".css",
	)
}

func SetResponseBody(
	response *http.Response,
	body []byte,
) {
	response.Header.Del("Content-Encoding")
	response.Header.Set(
		"Content-Length",
		fmt.Sprint(len(body)),
	)
	response.ContentLength = int64(len(body))
	response.Body = io.NopCloser(bytes.NewReader(body))
}

func (s *ProxyServer) handleProxyError(
	writer http.ResponseWriter,
	request *http.Request,
	err error,
) {
	s.logger.Error(
		"代理错误 [%s %s]: %v",
		request.Method,
		request.URL.Path,
		err,
	)

	http.Error(
		writer,
		"代理错误: "+err.Error(),
		http.StatusBadGateway,
	)
}

// -------------------- HTTP 中间件 --------------------

func (s *ProxyServer) loggingMiddleware(
	next http.Handler,
) http.Handler {
	return http.HandlerFunc(
		func(
			writer http.ResponseWriter,
			request *http.Request,
		) {
			s.logger.Info(
				"请求: %s %s",
				request.Method,
				request.URL.Path,
			)

			next.ServeHTTP(writer, request)
		},
	)
}

func (s *ProxyServer) recoverMiddleware(
	next http.Handler,
) http.Handler {
	return http.HandlerFunc(
		func(
			writer http.ResponseWriter,
			request *http.Request,
		) {
			defer func() {
				recovered := recover()
				if recovered == nil {
					return
				}

				s.logger.Error(
					"请求处理 panic [%s]: %v",
					request.URL.Path,
					recovered,
				)

				http.Error(
					writer,
					fmt.Sprintf(
						"内部错误: %v",
						recovered,
					),
					http.StatusInternalServerError,
				)
			}()

			next.ServeHTTP(writer, request)
		},
	)
}

// -------------------- 请求目标解析 --------------------

func NormalizeTargetPath(path string) (string, bool) {
	targetRaw := strings.TrimPrefix(path, "/")

	if strings.HasPrefix(targetRaw, "http:/") &&
		!strings.HasPrefix(targetRaw, "http://") {
		targetRaw = "http://" +
			targetRaw[len("http:/"):]
	} else if strings.HasPrefix(targetRaw, "https:/") &&
		!strings.HasPrefix(targetRaw, "https://") {
		targetRaw = "https://" +
			targetRaw[len("https:/"):]
	}

	target, err := url.Parse(targetRaw)
	if err != nil ||
		(target.Scheme != "http" && target.Scheme != "https") ||
		target.Host == "" {
		return "", false
	}

	return targetRaw, true
}

// -------------------- 应用启动 --------------------

func Run() error {
	config, err := LoadConfig()
	if err != nil {
		return err
	}

	logger := NewLogger(config.LogLevel)

	server, err := NewProxyServer(config, logger)
	if err != nil {
		return err
	}

	address := "0.0.0.0:" + config.Port

	logger.Info("启动地址: %s", address)
	logger.Info("上游代理: %s", config.ProxyRaw)
	logger.Info("HTML 改写地址: %s", config.HTMLBaseURL)

	return http.ListenAndServe(
		address,
		server.Handler(),
	)
}

func main() {
	if err := Run(); err != nil {
		log.Fatal(err)
	}
}
