package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigurationAndMarketID(t *testing.T) {
	t.Setenv("MODELSCOPE_API_KEY", "")
	t.Setenv("MODELSCOPE_TOKEN", "")
	t.Setenv("MODELSCOPE_HUB_BASE_URL", "")
	t.Setenv("MODELSCOPE_SKILLS_DIR", "")
	cfg, err := configFromEnv()
	if err != nil || cfg.baseURL != defaultBaseURL || !filepath.IsAbs(cfg.skillsDir) || cfg.apiKey != "" {
		t.Fatalf("默认配置不正确：%+v，%v", cfg, err)
	}
	for _, address := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com/openapi/v1", "https://example.com?key=secret"} {
		t.Setenv("MODELSCOPE_HUB_BASE_URL", address)
		if _, err := configFromEnv(); err == nil {
			t.Errorf("不应接受站点地址：%s", address)
		}
	}
	t.Setenv("MODELSCOPE_HUB_BASE_URL", "http://127.0.0.1:12345")
	t.Setenv("MODELSCOPE_TOKEN", "测试令牌")
	if cfg, err := configFromEnv(); err != nil || cfg.apiKey != "测试令牌" {
		t.Fatal("本机测试地址或令牌兼容环境变量未生效")
	}
	for _, id := range []string{"@anthropics/skill-creator", "someone/my.skill"} {
		if _, err := normalizeMarketID(id); err != nil {
			t.Errorf("拒绝了合法标识：%s", id)
		}
	}
	for _, id := range []string{"", "../name", "@author/../name", "@author/name?x=1", "https://example.com/a", "@author/name\\a", "@author/%2e%2e"} {
		if _, err := normalizeMarketID(id); err == nil {
			t.Errorf("接受了非法标识：%s", id)
		}
	}
}

func TestEnvelopeErrorsAndRedaction(t *testing.T) {
	// 测试值不是有效凭据，任何响应和错误都不应回显配置的令牌。
	const secret = "测试敏感值"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("认证请求头不正确")
		}
		switch r.URL.Path {
		case "/ok":
			fmt.Fprint(w, `{"success":true,"data":{"items":null}}`)
		case "/legacy":
			fmt.Fprint(w, `{"Code":200,"Data":{"Content":"技能正文"}}`)
		case "/business":
			fmt.Fprintf(w, `{"success":false,"message":"%s"}`, secret)
		case "/denied":
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, `{"message":"%s"}`, secret)
		case "/oversized":
			fmt.Fprint(w, strings.Repeat("x", maxJSONBytes+1))
		default:
			fmt.Fprint(w, `<html>访问受限</html>`)
		}
	}))
	defer srv.Close()
	c := newHubClient(config{baseURL: srv.URL, apiKey: secret})
	if _, err := c.api(context.Background(), http.MethodGet, "/ok", nil, nil); err != nil {
		t.Fatal(err)
	}
	if data, err := c.legacy(context.Background(), "/legacy", nil); err != nil || data["Content"] != "技能正文" {
		t.Fatalf("兼容接口解析失败：%v", err)
	}
	for _, path := range []string{"/business", "/denied", "/oversized", "/html"} {
		_, err := c.api(context.Background(), http.MethodGet, path, nil, nil)
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("错误未正确处理或泄漏令牌：%s", path)
		}
	}
	if _, err := c.request(context.Background(), http.MethodGet, "//example.com", nil, nil); err == nil {
		t.Error("不应允许外部请求地址")
	}
}

func TestRedirectDoesNotLeakCredential(t *testing.T) {
	var received string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"success":true,"data":{}}`)
	}))
	defer target.Close()
	var origin *httptest.Server
	origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer 测试值" {
			t.Error("初始请求未携带令牌")
		}
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer origin.Close()
	c := newHubClient(config{baseURL: origin.URL, apiKey: "测试值"})
	if _, err := c.api(context.Background(), http.MethodGet, "/redirect", nil, nil); err != nil {
		t.Fatal(err)
	}
	if received != "" {
		t.Error("跨源重定向泄漏了认证请求头")
	}
}

// 实网用例默认跳过，只有显式设置开关才读取官方公开接口，绝不下载技能。
func TestLivePublicAPI(t *testing.T) {
	if os.Getenv("MODELSCOPE_LIVE_TEST") != "1" {
		t.Skip("未启用公开接口实网验证")
	}
	c := newHubClient(config{baseURL: defaultBaseURL})
	session := newToolTestSession(t, c)
	for _, name := range []string{"modelscope_search_mcps", "modelscope_search_skills"} {
		search := "fetch"
		if name == "modelscope_search_skills" {
			search = "pdf"
		}
		result := callToolForTest(t, session, name, map[string]any{"search": search, "page_size": 1})
		if result.IsError {
			t.Fatalf("公共搜索失败：%s", toolResultText(t, result))
		}
		data := decodeToolResult(t, result)
		if items, _ := data["items"].([]any); len(items) != 1 {
			t.Fatalf("公共搜索未返回预期条目：%s", name)
		}
	}
	result := callToolForTest(t, session, "modelscope_get_mcp", map[string]any{"id": "@modelcontextprotocol/fetch"})
	if result.IsError {
		t.Fatal(toolResultText(t, result))
	}
	detail := decodeToolResult(t, result)
	sources, _ := detail["download_sources"].(map[string]any)
	if detail["readme"] == "" || detail["server_config"] == nil || sources["package_from_config"] == nil {
		t.Fatal("协议详情缺少说明、配置或包来源")
	}
	result = callToolForTest(t, session, "modelscope_get_skill", map[string]any{"id": "@anthropics/skill-creator", "include_content": true})
	if result.IsError {
		t.Fatal(toolResultText(t, result))
	}
	skill := decodeToolResult(t, result)
	commands, _ := skill["install_command"].([]any)
	if len(commands) == 0 || skill["download_url"] == "" || len(result.Content) != 2 || !strings.Contains(allResultText(t, result), "SKILL.md 正文") {
		t.Fatal("技能详情缺少安装说明、下载地址或正文")
	}
	t.Log("官方两类搜索、两类详情与技能正文读取成功，未使用真实令牌或下载技能")
}
