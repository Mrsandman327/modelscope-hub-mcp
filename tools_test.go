package main

// 本文件仅覆盖本实现范围负责的查询工具与下载工具的输入校验、工具注册、
// 内存会话文本结果，以及用本地 httptest 模拟的上游请求方法、参数与鉴权。
// 真实网络、真实令牌和真实技能下载不参与测试。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const testToolAPIKey = "unit-test-token"

// newToolTestClient 创建一个指向 httptest 服务器的 hubClient。
// 测试令牌是无效占位值，仅用于验证请求头和脱敏行为。
func newToolTestClient(t *testing.T, apiKey string, handler http.Handler) *hubClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &hubClient{
		baseURL:   srv.URL,
		apiKey:    apiKey,
		skillsDir: t.TempDir(),
		http:      srv.Client(),
	}
}

// newToolTestSession 使用 SDK 的内存传输建立真实的服务端与客户端会话。
func newToolTestSession(t *testing.T, c *hubClient) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	registerTools(server, c)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("服务端连接失败：%v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		_ = serverSession.Close()
		t.Fatalf("客户端连接失败：%v", err)
	}
	t.Cleanup(func() {
		_ = clientSession.Close()
		_ = serverSession.Close()
	})
	return clientSession
}

func callToolForTest(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("调用工具 %s 失败：%v", name, err)
	}
	return result
}

func toolResultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) == 0 {
		t.Fatal("工具结果为空")
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("工具内容类型为 %T，期望文本", result.Content[0])
	}
	return text.Text
}

func allResultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	var builder strings.Builder
	for _, content := range result.Content {
		text, ok := content.(*mcp.TextContent)
		if !ok {
			t.Fatalf("工具内容类型为 %T，期望文本", content)
		}
		builder.WriteString(text.Text)
		builder.WriteString("\n")
	}
	return builder.String()
}

// decodeToolResult 将首个文本内容解析为 JSON 对象，便于精确断言字段。
func decodeToolResult(t *testing.T, result *mcp.CallToolResult) map[string]any {
	t.Helper()
	text := toolResultText(t, result)
	var object map[string]any
	if err := json.Unmarshal([]byte(text), &object); err != nil {
		t.Fatalf("工具结果不是合法 JSON：%v\n%s", err, text)
	}
	return object
}

func containsHan(text string) bool {
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func schemaRequiredFields(t *testing.T, tool *mcp.Tool) []string {
	t.Helper()
	schema, ok := tool.InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("工具 %s 的输入 Schema 类型为 %T", tool.Name, tool.InputSchema)
	}
	raw, _ := schema["required"].([]any)
	fields := make([]string, 0, len(raw))
	for _, item := range raw {
		if name, ok := item.(string); ok {
			fields = append(fields, name)
		}
	}
	return fields
}

func TestToolRegistration(t *testing.T) {
	c := &hubClient{baseURL: defaultBaseURL, skillsDir: t.TempDir(), http: &http.Client{Timeout: time.Second}}
	session := newToolTestSession(t, c)

	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("列出工具失败：%v", err)
	}
	want := map[string]bool{
		"modelscope_search_mcps":    true,
		"modelscope_get_mcp":        true,
		"modelscope_search_skills":  true,
		"modelscope_get_skill":      true,
		"modelscope_download_skill": true,
	}
	if len(result.Tools) != len(want) {
		t.Fatalf("工具数量为 %d，期望 %d", len(result.Tools), len(want))
	}
	toolsByName := make(map[string]*mcp.Tool, len(result.Tools))
	for _, tool := range result.Tools {
		if !want[tool.Name] {
			t.Errorf("出现未预期的工具 %q", tool.Name)
			continue
		}
		toolsByName[tool.Name] = tool
		if tool.Description == "" || !containsHan(tool.Description) {
			t.Errorf("工具 %q 缺少中文说明", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("工具 %q 缺少输入 Schema", tool.Name)
		}
	}
	for name := range want {
		if toolsByName[name] == nil {
			t.Errorf("缺少工具 %q", name)
		}
	}

	for _, name := range []string{"modelscope_get_mcp", "modelscope_get_skill", "modelscope_download_skill"} {
		tool := toolsByName[name]
		if tool == nil {
			continue
		}
		found := false
		for _, field := range schemaRequiredFields(t, tool) {
			if field == "id" {
				found = true
			}
		}
		if !found {
			t.Errorf("工具 %s 的必填字段未包含 id", name)
		}
	}

	// 查询与详情工具必须标记为只读；下载工具会写入本地目录，绝不能标成只读。
	for _, name := range []string{"modelscope_search_mcps", "modelscope_get_mcp", "modelscope_search_skills", "modelscope_get_skill"} {
		tool := toolsByName[name]
		if tool == nil {
			continue
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("工具 %s 应标记为只读", name)
		}
		if tool.Annotations == nil || tool.Annotations.OpenWorldHint == nil || !*tool.Annotations.OpenWorldHint {
			t.Errorf("工具 %s 应标记为访问外部实体", name)
		}
	}
	download := toolsByName["modelscope_download_skill"]
	if download != nil {
		if download.Annotations == nil {
			t.Fatal("下载工具缺少注解")
		}
		if download.Annotations.ReadOnlyHint {
			t.Error("下载工具不应标记为只读")
		}
		if download.Annotations.DestructiveHint == nil || *download.Annotations.DestructiveHint {
			t.Error("下载工具不覆盖已有文件，应显式标记为非破坏性")
		}
	}
}

func TestSearchMCPsRequestAndResult(t *testing.T) {
	var method, path, rawQuery, authorization string
	var requestBody map[string]any
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		rawQuery = r.URL.RawQuery
		authorization = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Errorf("解析协议搜索请求体失败：%v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"success": true,
			"data": {
				"mcp_server_list": [
					{
						"id": "@demo/server",
						"name": "演示协议服务",
						"description": "用于测试的协议服务",
						"developer": "演示作者",
						"category": "工具",
						"categories": ["工具", "开发"],
						"view_count": 42,
						"download_count": 7,
						"url": "https://www.modelscope.cn/mcp/servers/@demo/server",
						"source_url": "https://github.com/example/demo",
						"locales": {"zh": "不应透传的本地化大字段"}
					}
				],
				"total_count": 1
			}
		}`)
	})
	c := newToolTestClient(t, testToolAPIKey, handler)
	session := newToolTestSession(t, c)

	result := callToolForTest(t, session, "modelscope_search_mcps", map[string]any{
		"search":      "演示",
		"page_number": 2,
		"page_size":   3,
		"category":    "工具",
		"is_hosted":   true,
	})
	if result.IsError {
		t.Fatalf("搜索失败：%s", toolResultText(t, result))
	}

	if method != http.MethodPut {
		t.Errorf("请求方法为 %s，期望 PUT", method)
	}
	if path != "/openapi/v1/mcp/servers" {
		t.Errorf("请求路径为 %s", path)
	}
	if rawQuery != "" {
		t.Errorf("协议搜索不应带查询参数，实际为 %s", rawQuery)
	}
	if authorization != "Bearer "+testToolAPIKey {
		t.Errorf("认证请求头为 %q", authorization)
	}
	if requestBody["search"] != "演示" {
		t.Errorf("search 参数为 %#v", requestBody["search"])
	}
	if requestBody["page_number"] != float64(2) || requestBody["page_size"] != float64(3) {
		t.Errorf("分页参数不正确：%#v", requestBody)
	}
	filter, ok := requestBody["filter"].(map[string]any)
	if !ok {
		t.Fatalf("filter 参数类型为 %T", requestBody["filter"])
	}
	if filter["category"] != "工具" || filter["is_hosted"] != true {
		t.Errorf("filter 参数不正确：%#v", filter)
	}

	output := decodeToolResult(t, result)
	if output["total_count"] != float64(1) {
		t.Errorf("total_count 为 %#v", output["total_count"])
	}
	if output["page_number"] != float64(2) || output["page_size"] != float64(3) {
		t.Errorf("分页回填不正确：%#v", output)
	}
	items, ok := output["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items 为 %#v", output["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("条目类型为 %T", items[0])
	}
	if _, leaked := item["locales"]; leaked {
		t.Error("精简条目不应包含 locales 大字段")
	}
	if item["id"] != "@demo/server" || item["name"] != "演示协议服务" {
		t.Errorf("条目基本字段不正确：%#v", item)
	}
	categories, ok := item["categories"].([]any)
	if !ok || len(categories) != 2 || categories[0] != "工具" || categories[1] != "开发" {
		t.Errorf("条目未保留真实 categories 数组：%#v", item["categories"])
	}
	if item["source_url"] != "https://github.com/example/demo" {
		t.Errorf("条目源码地址不正确：%#v", item["source_url"])
	}
	if item["view_count"] != float64(42) || item["download_count"] != float64(7) {
		t.Errorf("条目计数不正确：%#v", item)
	}
}

func TestSearchMCPsNullListBecomesEmptyArray(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":true,"data":{"mcp_server_list":null,"total_count":0}}`)
	})
	c := newToolTestClient(t, testToolAPIKey, handler)
	session := newToolTestSession(t, c)

	result := callToolForTest(t, session, "modelscope_search_mcps", map[string]any{})
	if result.IsError {
		t.Fatalf("搜索失败：%s", toolResultText(t, result))
	}
	if strings.Contains(toolResultText(t, result), `"items": null`) {
		t.Error("列表为 null 时应输出空数组")
	}
	output := decodeToolResult(t, result)
	items, ok := output["items"].([]any)
	if !ok {
		t.Fatalf("items 类型为 %T，期望数组", output["items"])
	}
	if len(items) != 0 {
		t.Errorf("items 应为空数组，实际为 %#v", items)
	}
	if output["page_number"] != float64(1) || output["page_size"] != float64(20) {
		t.Errorf("缺失分页时应回填默认值：%#v", output)
	}
}

func TestSearchSkillsRequestAndResult(t *testing.T) {
	var method, path, authorization string
	var query map[string]string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		authorization = r.Header.Get("Authorization")
		values := r.URL.Query()
		query = map[string]string{
			"search":            values.Get("search"),
			"page_number":       values.Get("page_number"),
			"page_size":         values.Get("page_size"),
			"filter.developer":  values.Get("filter.developer"),
			"filter.category":   values.Get("filter.category"),
			"filter.license":    values.Get("filter.license"),
			"filter.custom_tag": values.Get("filter.custom_tag"),
			"filter.owner":      values.Get("filter.owner"),
		}
		fmt.Fprint(w, `{
			"success": true,
			"data": {
				"skills": [
					{
						"id": "@demo/skill",
						"name": "演示技能",
						"description": "用于测试的技能",
						"developer": "演示作者",
						"category": "工具",
						"categories": ["工具", "文档"],
						"license": "MIT",
						"custom_tags": ["测试"],
						"view_count": 5,
						"download_count": 2,
						"url": "https://www.modelscope.cn/skills/@demo/skill",
						"source_url": "https://github.com/example/skill",
						"locales": {"zh": "不应透传的本地化大字段"}
					}
				],
				"total": 1
			}
		}`)
	})
	c := newToolTestClient(t, testToolAPIKey, handler)
	session := newToolTestSession(t, c)

	result := callToolForTest(t, session, "modelscope_search_skills", map[string]any{
		"search":      "演示",
		"page_number": 3,
		"page_size":   10,
		"developer":   "演示作者",
		"category":    "工具",
		"license":     "MIT",
		"custom_tag":  "测试",
		"owner":       "owner-demo",
	})
	if result.IsError {
		t.Fatalf("技能搜索失败：%s", toolResultText(t, result))
	}
	if method != http.MethodGet {
		t.Errorf("请求方法为 %s，期望 GET", method)
	}
	if path != "/openapi/v1/skills" {
		t.Errorf("请求路径为 %s", path)
	}
	if authorization != "Bearer "+testToolAPIKey {
		t.Errorf("认证请求头为 %q", authorization)
	}
	wantQuery := map[string]string{
		"search":            "演示",
		"page_number":       "3",
		"page_size":         "10",
		"filter.developer":  "演示作者",
		"filter.category":   "工具",
		"filter.license":    "MIT",
		"filter.custom_tag": "测试",
		"filter.owner":      "owner-demo",
	}
	for key, want := range wantQuery {
		if query[key] != want {
			t.Errorf("查询参数 %s 为 %q，期望 %q", key, query[key], want)
		}
	}

	output := decodeToolResult(t, result)
	if output["total_count"] != float64(1) {
		t.Errorf("技能总数未从 total 字段提取：%#v", output["total_count"])
	}
	items, ok := output["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items 为 %#v", output["items"])
	}
	item := items[0].(map[string]any)
	if _, leaked := item["locales"]; leaked {
		t.Error("精简条目不应包含 locales 大字段")
	}
	if item["id"] != "@demo/skill" || item["license"] != "MIT" {
		t.Errorf("条目字段不正确：%#v", item)
	}
	categories, ok := item["categories"].([]any)
	if !ok || len(categories) != 2 || categories[0] != "工具" || categories[1] != "文档" {
		t.Errorf("技能条目未保留真实 categories 数组：%#v", item["categories"])
	}
}

func TestGetMCPDetailUsageAndAddresses(t *testing.T) {
	var method, path, rawQuery, authorization string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		rawQuery = r.URL.RawQuery
		authorization = r.Header.Get("Authorization")
		fmt.Fprint(w, `{
			"success": true,
			"data": {
				"id": "@demo/server",
				"name": "演示协议服务",
				"description": "用于测试的协议服务",
				"developer": "演示作者",
				"category": "工具",
				"categories": ["工具", "开发"],
				"readme": "# 演示服务\n完整使用说明内容",
				"server_config": [
					{
						"mcpServers": {
							"fetch": {"command": "uvx", "args": ["mcp-server-fetch"]}
						}
					}
				],
				"env_schema": {
					"type": "object",
					"properties": {"DEMO_TOKEN": {"type": "string", "description": "访问令牌"}}
				},
				"is_hosted": true,
				"source_url": "https://github.com/example/demo",
				"url": "https://www.modelscope.cn/mcp/servers/@demo/server",
				"operational_urls": ["https://example.com/mcp"]
			}
		}`)
	})
	c := newToolTestClient(t, testToolAPIKey, handler)
	session := newToolTestSession(t, c)

	result := callToolForTest(t, session, "modelscope_get_mcp", map[string]any{
		"id":                  "@demo/server",
		"get_operational_url": true,
	})
	if result.IsError {
		t.Fatalf("获取详情失败：%s", toolResultText(t, result))
	}
	if method != http.MethodGet {
		t.Errorf("请求方法为 %s，期望 GET", method)
	}
	if path != "/openapi/v1/mcp/servers/@demo/server" {
		t.Errorf("请求路径为 %s", path)
	}
	if rawQuery != "get_operational_url=true" {
		t.Errorf("查询参数为 %q", rawQuery)
	}
	if authorization != "Bearer "+testToolAPIKey {
		t.Errorf("认证请求头为 %q", authorization)
	}

	output := decodeToolResult(t, result)
	if output["readme"] != "# 演示服务\n完整使用说明内容" {
		t.Errorf("readme 不完整：%#v", output["readme"])
	}
	if _, ok := output["env_schema"].(map[string]any); !ok {
		t.Errorf("env_schema 未返回：%#v", output["env_schema"])
	}
	configList, ok := output["server_config"].([]any)
	if !ok || len(configList) != 1 {
		t.Fatalf("server_config 原始数组未保留：%#v", output["server_config"])
	}
	firstConfig, ok := configList[0].(map[string]any)
	if !ok {
		t.Fatalf("server_config 元素类型为 %T", configList[0])
	}
	if _, ok := firstConfig["mcpServers"].(map[string]any); !ok {
		t.Errorf("server_config 原始内容不完整：%#v", firstConfig)
	}
	categories, ok := output["categories"].([]any)
	if !ok || len(categories) != 2 || categories[0] != "工具" || categories[1] != "开发" {
		t.Errorf("详情未保留真实 categories 数组：%#v", output["categories"])
	}
	if output["is_hosted"] != true {
		t.Errorf("is_hosted 为 %#v", output["is_hosted"])
	}
	if output["source_url"] != "https://github.com/example/demo" {
		t.Errorf("source_url 为 %#v", output["source_url"])
	}
	if output["url"] != "https://www.modelscope.cn/mcp/servers/@demo/server" {
		t.Errorf("页面地址为 %#v", output["url"])
	}
	urls, ok := output["operational_urls"].([]any)
	if !ok || len(urls) != 1 || urls[0] != "https://example.com/mcp" {
		t.Errorf("operational_urls 为 %#v", output["operational_urls"])
	}

	usage, ok := output["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage 类型为 %T", output["usage"])
	}
	if usage["config_source"] != "server_config" {
		t.Errorf("usage.config_source 为 %#v", usage["config_source"])
	}
	notes, ok := usage["notes"].([]any)
	if !ok || len(notes) < 3 {
		t.Fatalf("usage.notes 不完整：%#v", usage["notes"])
	}
	joinedNotes := fmt.Sprint(notes)
	if !strings.Contains(joinedNotes, "server_config") || !strings.Contains(joinedNotes, "不编造") {
		t.Errorf("usage.notes 未说明配置来源与不编造约束：%s", joinedNotes)
	}

	sources, ok := output["download_sources"].(map[string]any)
	if !ok {
		t.Fatalf("download_sources 类型为 %T", output["download_sources"])
	}
	if sources["source_url"] != "https://github.com/example/demo" {
		t.Errorf("download_sources.source_url 为 %#v", sources["source_url"])
	}
	if note, _ := sources["note"].(string); !strings.Contains(note, "不是可直接下载的安装包链接") {
		t.Errorf("download_sources.note 未澄清 source_url 性质：%q", note)
	}
	packageInfo, ok := sources["package_from_config"].(map[string]any)
	if !ok {
		t.Fatalf("package_from_config 类型为 %T", sources["package_from_config"])
	}
	if packageInfo["command"] != "uvx" {
		t.Errorf("包来源命令为 %#v", packageInfo["command"])
	}
	args, ok := packageInfo["args"].([]any)
	if !ok || len(args) != 1 || args[0] != "mcp-server-fetch" {
		t.Errorf("包来源参数为 %#v", packageInfo["args"])
	}
}

func TestGetMCPOperationalURLRequiresToken(t *testing.T) {
	// 不启动服务器：该分支应在发出请求前直接拒绝。
	c := &hubClient{baseURL: "http://127.0.0.1:1", skillsDir: t.TempDir(), http: &http.Client{Timeout: time.Second}}
	session := newToolTestSession(t, c)

	result := callToolForTest(t, session, "modelscope_get_mcp", map[string]any{
		"id":                  "@demo/server",
		"get_operational_url": true,
	})
	if !result.IsError {
		t.Fatal("未配置访问令牌时请求托管地址应失败")
	}
	if text := toolResultText(t, result); !strings.Contains(text, "MODELSCOPE_API_KEY") {
		t.Errorf("错误信息未说明所需环境变量：%s", text)
	}
}

func TestGetSkillDetailAndOptionalContent(t *testing.T) {
	var rawRequests int32
	var detailPath, rawPath, rawQuery string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/repo/raw") {
			atomic.AddInt32(&rawRequests, 1)
			rawPath = r.URL.Path
			rawQuery = r.URL.RawQuery
			if r.Method != http.MethodGet {
				t.Errorf("技能正文请求方法为 %s，期望 GET", r.Method)
			}
			fmt.Fprint(w, `{"Code":200,"Data":{"Content":"# 技能正文\n这是正文内容。"}}`)
			return
		}
		detailPath = r.URL.Path
		fmt.Fprint(w, `{
			"success": true,
			"data": {
				"id": "@demo/skill",
				"name": "演示技能",
				"description": "用于测试的技能",
				"developer": "演示作者",
				"category": "工具",
				"license": "MIT",
				"install_command": [
					"npx skills add https://modelscope.cn/skills/@demo/skill",
					"curl -fsSL https://modelscope.cn/skills/install.sh | bash -s -- @demo/skill",
					"pip install --upgrade modelscope & modelscope skills add @demo/skill"
				],
				"url": "https://www.modelscope.cn/skills/@demo/skill",
				"readme": "# 技能说明"
			}
		}`)
	})
	c := newToolTestClient(t, testToolAPIKey, handler)
	session := newToolTestSession(t, c)

	// 未请求正文时不应访问兼容接口。
	result := callToolForTest(t, session, "modelscope_get_skill", map[string]any{"id": "@demo/skill"})
	if result.IsError {
		t.Fatalf("获取技能详情失败：%s", toolResultText(t, result))
	}
	if got := atomic.LoadInt32(&rawRequests); got != 0 {
		t.Fatalf("include_content=false 时请求了正文接口 %d 次", got)
	}
	if detailPath != "/openapi/v1/skills/@demo/skill" {
		t.Errorf("详情请求路径为 %s", detailPath)
	}

	output := decodeToolResult(t, result)
	downloadURL, _ := output["download_url"].(string)
	if !strings.Contains(downloadURL, "/archive/zip/master") || !strings.Contains(downloadURL, "@demo/skill") {
		t.Errorf("download_url 未按官方压缩包路径生成：%q", downloadURL)
	}
	warnings, ok := output["warnings"].([]any)
	if !ok || len(warnings) == 0 {
		t.Fatalf("warnings 为 %#v", output["warnings"])
	}
	if !strings.Contains(fmt.Sprint(warnings), "内部接口") {
		t.Errorf("warnings 未说明下载路径属于内部接口：%v", warnings)
	}
	wantInstallCommands := []string{
		"npx skills add https://modelscope.cn/skills/@demo/skill",
		"curl -fsSL https://modelscope.cn/skills/install.sh | bash -s -- @demo/skill",
		"pip install --upgrade modelscope & modelscope skills add @demo/skill",
	}
	installCommands, ok := output["install_command"].([]any)
	if !ok || len(installCommands) != len(wantInstallCommands) {
		t.Fatalf("install_command 未原样保留数组：%#v", output["install_command"])
	}
	for index, want := range wantInstallCommands {
		if installCommands[index] != want {
			t.Errorf("install_command[%d] 为 %#v，期望 %q", index, installCommands[index], want)
		}
	}
	if output["url"] != "https://www.modelscope.cn/skills/@demo/skill" {
		t.Errorf("页面地址为 %#v", output["url"])
	}
	skill, ok := output["skill"].(map[string]any)
	if !ok || skill["id"] != "@demo/skill" {
		t.Errorf("原始技能元数据未返回：%#v", output["skill"])
	}
	nestedCommands, ok := skill["install_command"].([]any)
	if !ok || len(nestedCommands) != len(wantInstallCommands) {
		t.Errorf("嵌套 skill 原始 install_command 未保留：%#v", skill["install_command"])
	} else if nestedCommands[0] != wantInstallCommands[0] {
		t.Errorf("嵌套 install_command[0] 为 %#v，期望 %q", nestedCommands[0], wantInstallCommands[0])
	}

	// include_content=true 时应请求正文并用第二段文本展示。
	result = callToolForTest(t, session, "modelscope_get_skill", map[string]any{
		"id":              "@demo/skill",
		"include_content": true,
	})
	if result.IsError {
		t.Fatalf("获取技能正文失败：%s", toolResultText(t, result))
	}
	if got := atomic.LoadInt32(&rawRequests); got != 1 {
		t.Fatalf("include_content=true 时正文请求次数为 %d", got)
	}
	if rawPath != "/api/v1/skills/@demo/skill/repo/raw" {
		t.Errorf("正文请求路径为 %s", rawPath)
	}
	if rawQuery != "FilePath=SKILL.md" {
		t.Errorf("正文请求参数为 %q", rawQuery)
	}
	if text := allResultText(t, result); !strings.Contains(text, "技能正文") || !strings.Contains(text, "这是正文内容") {
		t.Errorf("结果未包含技能正文：%s", text)
	}
}

func TestDownloadSkillValidationWithoutNetwork(t *testing.T) {
	// 这些用例都必须在调用下载实现之前被拦截；不应产生任何网络请求。
	c := &hubClient{skillsDir: t.TempDir(), http: &http.Client{Timeout: time.Second}}
	session := newToolTestSession(t, c)

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"缺少 id", map[string]any{}, "id"},
		{"非法 id", map[string]any{"id": "https://example.com/a"}, "id"},
		{"任意路径参数", map[string]any{"id": "@demo/skill", "path": "/tmp/other"}, "不支持"},
		{"任意网址参数", map[string]any{"id": "@demo/skill", "url": "https://example.com/a.zip"}, "不支持"},
		{"覆盖参数", map[string]any{"id": "@demo/skill", "overwrite": true}, "不支持"},
		{"revision 路径跳转", map[string]any{"id": "@demo/skill", "revision": "../main"}, "revision"},
		{"revision 类型错误", map[string]any{"id": "@demo/skill", "revision": 123}, "字符串"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := callToolForTest(t, session, "modelscope_download_skill", testCase.args)
			if !result.IsError {
				t.Fatalf("应拒绝参数：%#v", testCase.args)
			}
			if text := toolResultText(t, result); !strings.Contains(text, testCase.want) {
				t.Errorf("错误信息为 %q，期望包含 %q", text, testCase.want)
			}
		})
	}
}

func TestToolValidationWithoutNetwork(t *testing.T) {
	// 使用不可连接的本地地址，任何漏检都会以快速失败暴露。
	c := &hubClient{baseURL: "http://127.0.0.1:1", skillsDir: t.TempDir(), http: &http.Client{Timeout: time.Second}}
	session := newToolTestSession(t, c)

	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"modelscope_search_mcps", map[string]any{"page_number": 34, "page_size": 3}, "乘积"},
		{"modelscope_search_mcps", map[string]any{"page_size": 0}, "page_size"},
		{"modelscope_search_mcps", map[string]any{"page_size": "20"}, "整数"},
		{"modelscope_search_mcps", map[string]any{"foo": "bar"}, "不支持"},
		{"modelscope_search_mcps", map[string]any{"is_hosted": "true"}, "布尔"},
		{"modelscope_search_skills", map[string]any{"page_number": 31, "page_size": 100}, "乘积"},
		{"modelscope_search_skills", map[string]any{"page_size": "10"}, "整数"},
		{"modelscope_search_skills", map[string]any{"unknown": 1}, "不支持"},
		{"modelscope_get_mcp", map[string]any{}, "id"},
		{"modelscope_get_mcp", map[string]any{"id": "../escape"}, "id"},
		{"modelscope_get_mcp", map[string]any{"id": "@demo/server", "get_operational_url": "true"}, "布尔"},
		{"modelscope_get_mcp", map[string]any{"id": "@demo/server", "extra": true}, "不支持"},
		{"modelscope_get_skill", map[string]any{"id": "a/b/c"}, "id"},
		{"modelscope_get_skill", map[string]any{"id": "@demo/skill", "include_content": "yes"}, "布尔"},
	}
	for _, testCase := range cases {
		t.Run(testCase.tool+"-"+testCase.want, func(t *testing.T) {
			result := callToolForTest(t, session, testCase.tool, testCase.args)
			if !result.IsError {
				t.Fatalf("应拒绝参数：%#v", testCase.args)
			}
			if text := toolResultText(t, result); !strings.Contains(text, testCase.want) {
				t.Errorf("错误信息为 %q，期望包含 %q", text, testCase.want)
			}
		})
	}
}

func TestToolResultsRedactToken(t *testing.T) {
	const secret = "unit-test-secret-value"
	t.Run("错误信息", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, `{"message":"请求被拒绝，令牌 %s 无效"}`, secret)
		})
		c := newToolTestClient(t, secret, handler)
		session := newToolTestSession(t, c)
		result := callToolForTest(t, session, "modelscope_search_mcps", map[string]any{})
		if !result.IsError {
			t.Fatal("上游拒绝时应返回业务错误")
		}
		if strings.Contains(toolResultText(t, result), secret) {
			t.Error("错误结果泄漏了令牌")
		}
	})

	t.Run("成功结果", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"success":true,"data":{"mcp_server_list":[{"id":"@demo/server","name":"包含 %s 的结果"}],"total_count":1}}`, secret)
		})
		c := newToolTestClient(t, secret, handler)
		session := newToolTestSession(t, c)
		result := callToolForTest(t, session, "modelscope_search_mcps", map[string]any{})
		if result.IsError {
			t.Fatalf("搜索失败：%s", toolResultText(t, result))
		}
		if strings.Contains(allResultText(t, result), secret) {
			t.Error("成功结果泄漏了令牌")
		}
	})
}
