package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL = "https://www.modelscope.cn"
	maxJSONBytes   = 4 << 20
)

type config struct {
	apiKey    string
	baseURL   string
	skillsDir string
}

// 令牌只读取进程环境，不写入项目文件；查询公开资源时允许不配置。
func configFromEnv() (config, error) {
	c := config{
		apiKey:    strings.TrimSpace(os.Getenv("MODELSCOPE_API_KEY")),
		baseURL:   strings.TrimRight(strings.TrimSpace(os.Getenv("MODELSCOPE_HUB_BASE_URL")), "/"),
		skillsDir: strings.TrimSpace(os.Getenv("MODELSCOPE_SKILLS_DIR")),
	}
	if c.apiKey == "" {
		c.apiKey = strings.TrimSpace(os.Getenv("MODELSCOPE_TOKEN"))
	}
	if strings.ContainsAny(c.apiKey, "\r\n") {
		return config{}, errors.New("访问令牌包含非法换行符")
	}
	if c.baseURL == "" {
		c.baseURL = defaultBaseURL
	}
	u, err := url.Parse(c.baseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || u.Opaque != "" {
		return config{}, errors.New("MODELSCOPE_HUB_BASE_URL 必须是无路径、凭据和查询参数的站点地址")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return config{}, errors.New("站点地址必须使用安全连接，本地测试地址除外")
	}
	if c.skillsDir == "" {
		c.skillsDir = "skills"
	}
	c.skillsDir, err = filepath.Abs(c.skillsDir)
	if err != nil {
		return config{}, errors.New("无法确定技能下载目录")
	}
	return c, nil
}

type hubClient struct {
	baseURL    string
	apiKey     string
	skillsDir  string
	http       *http.Client
	downloadMu sync.Mutex
}

func newHubClient(c config) *hubClient {
	return &hubClient{
		baseURL: c.baseURL, apiKey: c.apiKey, skillsDir: c.skillsDir,
		http: &http.Client{
			Timeout: 120 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("重定向次数过多")
				}
				if req.URL.User != nil || (req.URL.Scheme != "https" && !(req.URL.Scheme == "http" && isLoopbackHost(req.URL.Hostname()))) {
					return errors.New("拒绝不安全的重定向地址")
				}
				// 包文件可能跳转至存储站点，令牌绝不能随跨源跳转发送。
				for _, previous := range via {
					if !sameOrigin(req.URL, previous.URL) {
						req.Header.Del("Authorization")
						req.Header.Del("Cookie")
						break
					}
				}
				return nil
			},
		},
	}
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

var (
	marketIDPattern = regexp.MustCompile(`^@?[A-Za-z0-9_-][A-Za-z0-9_.-]{0,127}/[A-Za-z0-9_-][A-Za-z0-9_.-]{0,127}$`)
	tokenPattern    = regexp.MustCompile(`ms-[A-Za-z0-9_-]{12,}`)
)

// 市场标识只允许作者和名称两段，防止参数变成任意网络地址或路径。
func normalizeMarketID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if !marketIDPattern.MatchString(id) {
		return "", errors.New("id 必须是 @作者/名称 或 作者/名称，不接受网址及路径跳转")
	}
	return id, nil
}

func (c *hubClient) archiveURL(id, revision string) string {
	return c.baseURL + "/skills/" + id + "/archive/zip/" + url.PathEscape(revision)
}

func (c *hubClient) redact(text string) string {
	if c.apiKey != "" {
		text = strings.ReplaceAll(text, c.apiKey, "【令牌已隐藏】")
	}
	return tokenPattern.ReplaceAllString(text, "【令牌已隐藏】")
}

// 所有上游请求使用固定站点；工具参数无法替换主机或注入请求头。
func (c *hubClient) request(ctx context.Context, method, path string, query url.Values, payload any) (*http.Response, error) {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "?#\\\r\n") {
		return nil, errors.New("请求路径不合法")
	}
	u, err := url.Parse(c.baseURL + path)
	if err != nil {
		return nil, errors.New("无法构造上游地址")
	}
	u.RawQuery = query.Encode()
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, errors.New("请求参数无法编码")
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, errors.New("无法创建上游请求")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", serverName+"/"+serverVersion)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, errors.New("请求已取消")
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, errors.New("上游请求超时")
		}
		return nil, fmt.Errorf("上游请求失败：%s", c.redact(err.Error()))
	}
	return resp, nil
}

func (c *hubClient) api(ctx context.Context, method, path string, query url.Values, payload any) (map[string]any, error) {
	return c.readEnvelope(ctx, method, path, query, payload, false)
}

func (c *hubClient) legacy(ctx context.Context, path string, query url.Values) (map[string]any, error) {
	return c.readEnvelope(ctx, http.MethodGet, path, query, nil, true)
}

func (c *hubClient) readEnvelope(ctx context.Context, method, path string, query url.Values, payload any, legacy bool) (map[string]any, error) {
	resp, err := c.request(ctx, method, path, query, payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes+1))
	if err != nil {
		return nil, errors.New("无法读取上游响应")
	}
	if len(body) > maxJSONBytes {
		return nil, errors.New("上游响应超过大小限制")
	}
	var envelope map[string]any
	decodeErr := json.Unmarshal(body, &envelope)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message, _ := envelope["message"].(string)
		if message == "" {
			message, _ = envelope["Message"].(string)
		}
		if len([]rune(message)) > 300 {
			message = string([]rune(message)[:300]) + "…"
		}
		return nil, fmt.Errorf("上游接口返回状态 %d：%s", resp.StatusCode, c.redact(message))
	}
	if decodeErr != nil {
		return nil, errors.New("上游未返回有效的数据对象，可能需要登录或触发了访问限制")
	}
	dataKey := "data"
	if legacy {
		dataKey = "Data"
		if code, _ := envelope["Code"].(float64); code != 200 {
			message, _ := envelope["Message"].(string)
			return nil, fmt.Errorf("技能兼容接口失败：%s", c.redact(message))
		}
	} else if success, _ := envelope["success"].(bool); !success {
		message, _ := envelope["message"].(string)
		return nil, fmt.Errorf("上游业务处理失败：%s", c.redact(message))
	}
	data, ok := envelope[dataKey].(map[string]any)
	if !ok {
		return nil, errors.New("上游响应缺少数据对象")
	}
	return data, nil
}
