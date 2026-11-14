package main

// 本文件实现魔搭（ModelScope）MCP 市场与技能市场的查询、详情整理和安全下载工具。
// 查询结果统一整理为人类与模型都可读的 JSON 文本，并通过 hubClient.redact 隐藏令牌。
// 下载只允许调用 hubClient.downloadSkill 写入固定目录，工具参数不能指定任意网址或路径。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// 单次上游查询的超时时间。每个上游请求分别创建，避免互相抢占剩余时间。
	toolQueryTimeout = 30 * time.Second

	// 协议服务搜索的页码与页大小乘积上限。
	mcpPageProductLimit = 100
	// 技能搜索的页码与页大小乘积上限。
	skillPageProductLimit = 3000

	defaultPageNumber = 1
	defaultPageSize   = 20
	maxPageSize       = 100

	// 技能压缩包默认版本，与官方安装脚本使用的默认分支一致。
	defaultSkillRevision = "master"

	// 公开接口路径；站点基础地址由 hubClient.baseURL 提供。
	mcpServersAPIPath = "/openapi/v1/mcp/servers"
	skillsAPIPath     = "/openapi/v1/skills"
	// 技能正文兼容接口前缀，完整路径为 /api/v1/skills/{id}/repo/raw。
	skillRawAPIPathPrefix = "/api/v1/skills/"
)

// ---------- 参数解析：严格校验类型、未知属性和取值范围 ----------

// toolParseArgs 将工具参数解码为 JSON 对象，并拒绝未声明的属性。
// 解码使用 json.Number，避免大整数经 float64 转换后失去精度。
func toolParseArgs(req *mcp.CallToolRequest, allowed ...string) (map[string]any, error) {
	if req == nil || req.Params == nil || len(bytes.TrimSpace(req.Params.Arguments)) == 0 {
		return map[string]any{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(req.Params.Arguments))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, errors.New("参数必须是合法的 JSON 对象")
	}
	args, ok := decoded.(map[string]any)
	if !ok {
		return nil, errors.New("参数必须是 JSON 对象")
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = struct{}{}
	}
	unknown := make([]string, 0)
	for name := range args {
		if _, exists := allowedSet[name]; !exists {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("不支持的参数：%s", strings.Join(unknown, ", "))
	}
	return args, nil
}

// toolStringArg 读取可选字符串参数。缺失或 null 视为未提供，类型不符时报错。
func toolStringArg(args map[string]any, name string) (string, error) {
	value, exists := args[name]
	if !exists || value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("参数 %q 必须是字符串", name)
	}
	return strings.TrimSpace(text), nil
}

// toolIntArg 读取可选整数参数。缺失或 null 使用默认值，非整数时报错。
func toolIntArg(args map[string]any, name string, defaultValue int) (int, error) {
	value, exists := args[name]
	if !exists || value == nil {
		return defaultValue, nil
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("参数 %q 必须是整数", name)
	}
	parsed, err := strconv.Atoi(number.String())
	if err != nil {
		return 0, fmt.Errorf("参数 %q 必须是整数", name)
	}
	return parsed, nil
}

// toolBoolArg 读取可选布尔参数，第二个返回值表示参数是否实际提供。
func toolBoolArg(args map[string]any, name string) (bool, bool, error) {
	value, exists := args[name]
	if !exists || value == nil {
		return false, false, nil
	}
	flag, ok := value.(bool)
	if !ok {
		return false, false, fmt.Errorf("参数 %q 必须是布尔值", name)
	}
	return flag, true, nil
}

// toolRequiredMarketID 读取并规范化必填的市场标识，空值或非法格式直接报错。
func toolRequiredMarketID(args map[string]any) (string, error) {
	id, err := toolStringArg(args, "id")
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", errors.New("参数 \"id\" 不能为空")
	}
	normalized, err := normalizeMarketID(id)
	if err != nil {
		return "", err
	}
	return normalized, nil
}

// toolPageValues 校验页码和页大小，并用除法判断乘积上限以避免整数溢出。
func toolPageValues(args map[string]any, productLimit int) (int, int, error) {
	pageNumber, err := toolIntArg(args, "page_number", defaultPageNumber)
	if err != nil {
		return 0, 0, err
	}
	pageSize, err := toolIntArg(args, "page_size", defaultPageSize)
	if err != nil {
		return 0, 0, err
	}
	if pageNumber < 1 {
		return 0, 0, errors.New("参数 \"page_number\" 必须大于等于 1")
	}
	if pageSize < 1 || pageSize > maxPageSize {
		return 0, 0, fmt.Errorf("参数 \"page_size\" 必须在 1 到 %d 之间", maxPageSize)
	}
	if pageNumber > productLimit/pageSize {
		return 0, 0, fmt.Errorf("参数 \"page_number\" 与 \"page_size\" 的乘积不能超过 %d", productLimit)
	}
	return pageNumber, pageSize, nil
}

// toolValidateRevision 校验技能版本号。规则与下载实现的版本名校验保持一致：
// 只允许字母、数字、点、短横线、下划线和加号，禁止路径分隔符和路径跳转，
// 避免工具层接受而下载层拒绝的输入。
func toolValidateRevision(revision string) error {
	if revision == "" {
		return errors.New("参数 \"revision\" 不能为空")
	}
	if len(revision) > 128 {
		return errors.New("参数 \"revision\" 过长")
	}
	if revision == "." || revision == ".." {
		return errors.New("参数 \"revision\" 不能是 . 或 ..")
	}
	for _, r := range revision {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_', r == '+':
		default:
			return errors.New("参数 \"revision\" 只能包含字母、数字、点、短横线、下划线和加号，且不能包含路径分隔符")
		}
	}
	return nil
}

// ---------- 结果构造：只返回 TextContent，并统一脱敏 ----------

// toolEncodeJSON 将结果编码为不转义 HTML 的缩进 JSON 文本。
func toolEncodeJSON(value any) (string, error) {
	var builder strings.Builder
	encoder := json.NewEncoder(&builder)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimSuffix(builder.String(), "\n"), nil
}

func toolJSONResult(c *hubClient, value any) *mcp.CallToolResult {
	text, err := toolEncodeJSON(value)
	if err != nil {
		return toolErrorResult(c, fmt.Errorf("无法编码结果：%w", err))
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: c.redact(text)}}}
}

func toolErrorResult(c *hubClient, err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: c.redact(err.Error())}},
	}
}

// toolPartsResult 将多段文本合并为一个结果；每段都经过令牌脱敏。
func toolPartsResult(c *hubClient, parts ...string) *mcp.CallToolResult {
	content := make([]mcp.Content, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		content = append(content, &mcp.TextContent{Text: c.redact(part)})
	}
	if len(content) == 0 {
		content = append(content, &mcp.TextContent{Text: "上游未返回可展示的内容。"})
	}
	return &mcp.CallToolResult{Content: content}
}

func toolBoolPtr(value bool) *bool {
	return &value
}

// ---------- 上游响应字段提取 ----------

// toolFirstValue 返回第一个存在且非 null 的字段值。
func toolFirstValue(source map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		if value, exists := source[key]; exists && value != nil {
			return value, true
		}
	}
	return nil, false
}

// toolFirstString 返回第一个非空字符串字段，兼容常见的下划线与驼峰命名。
func toolFirstString(source map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, exists := source[key]; exists {
			if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
				return text
			}
		}
	}
	return ""
}

// toolFirstNumber 将第一个可解析的计数字段转换为整数。
func toolFirstNumber(source map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		value, exists := source[key]
		if !exists || value == nil {
			continue
		}
		switch number := value.(type) {
		case float64:
			return int64(number), true
		case float32:
			return int64(number), true
		case int:
			return int64(number), true
		case int64:
			return number, true
		case json.Number:
			if parsed, err := number.Int64(); err == nil {
				return parsed, true
			}
		case string:
			if parsed, err := strconv.ParseInt(strings.TrimSpace(number), 10, 64); err == nil {
				return parsed, true
			}
		}
	}
	return 0, false
}

// toolFirstBool 返回第一个布尔字段。
func toolFirstBool(source map[string]any, keys ...string) (bool, bool) {
	for _, key := range keys {
		if value, exists := source[key]; exists {
			if flag, ok := value.(bool); ok {
				return flag, true
			}
		}
	}
	return false, false
}

// toolCategoriesValue 返回真实分类数组。协议市场的分类主要在 categories 数组中；
// 若上游只给出单个 category 字符串，则将其作为唯一分类返回，避免丢失分类信息。
func toolCategoriesValue(source map[string]any) []any {
	if value, exists := source["categories"]; exists && value != nil {
		return toolAnyList(value)
	}
	if category := toolFirstString(source, "category"); category != "" {
		return []any{category}
	}
	return []any{}
}

// toolFirstList 返回指定键的对象数组。字段为 null 或缺失时返回空数组，
// 以满足“列表 null 视为空列表”的要求；非对象元素会被忽略。
func toolFirstList(source map[string]any, key string) []map[string]any {
	value, exists := source[key]
	if !exists || value == nil {
		return []map[string]any{}
	}
	switch list := value.(type) {
	case []map[string]any:
		if list == nil {
			return []map[string]any{}
		}
		return list
	case []any:
		result := make([]map[string]any, 0, len(list))
		for _, item := range list {
			if object, ok := item.(map[string]any); ok {
				result = append(result, object)
			}
		}
		return result
	}
	return []map[string]any{}
}

// toolTotalCount 提取指定键的总数。协议市场使用 total_count，技能市场使用 total。
func toolTotalCount(data map[string]any, key string) int64 {
	total, _ := toolFirstNumber(data, key)
	return total
}

// toolPageURL 优先采用上游返回的页面地址；缺失时按市场页面路径回填。
func toolPageURL(c *hubClient, raw map[string]any, kind, id string) string {
	if page := toolFirstString(raw, "url", "page_url", "pageUrl", "html_url", "htmlUrl", "detail_url", "detailUrl"); page != "" {
		return page
	}
	if id == "" {
		return ""
	}
	if kind == "mcp" {
		return c.baseURL + "/mcp/servers/" + id
	}
	return c.baseURL + "/skills/" + id
}

// toolCompactMCPServer 只保留协议服务搜索结果的必要字段，避免把每条完整的
// locales 等大字段透传给客户端。
func toolCompactMCPServer(c *hubClient, raw map[string]any) map[string]any {
	id := toolFirstString(raw, "id", "Id", "ID", "server_id", "serverId")
	item := map[string]any{
		"id":          id,
		"name":        toolFirstString(raw, "name", "Name", "display_name", "displayName"),
		"description": toolFirstString(raw, "description", "Description", "summary"),
		"developer":   toolFirstString(raw, "developer", "Developer", "author", "owner"),
		"category":    toolFirstString(raw, "category", "Category"),
		"categories":  toolCategoriesValue(raw),
		"url":         toolPageURL(c, raw, "mcp", id),
		"source_url":  toolFirstString(raw, "source_url", "sourceUrl", "repo_url", "repoUrl", "repository", "repository_url", "repositoryUrl", "homepage"),
	}
	viewCount, _ := toolFirstNumber(raw, "view_count", "viewCount", "views", "visit_count", "visitCount")
	downloadCount, _ := toolFirstNumber(raw, "download_count", "downloadCount", "downloads")
	item["view_count"] = viewCount
	item["download_count"] = downloadCount
	return item
}

// toolCompactSkill 只保留技能搜索结果的必要字段。
func toolCompactSkill(c *hubClient, raw map[string]any) map[string]any {
	id := toolFirstString(raw, "id", "Id", "ID", "skill_id", "skillId")
	item := map[string]any{
		"id":          id,
		"name":        toolFirstString(raw, "name", "Name", "display_name", "displayName"),
		"description": toolFirstString(raw, "description", "Description", "summary"),
		"developer":   toolFirstString(raw, "developer", "Developer", "author", "owner"),
		"category":    toolFirstString(raw, "category", "Category"),
		"categories":  toolCategoriesValue(raw),
		"license":     toolFirstString(raw, "license", "License"),
		"url":         toolPageURL(c, raw, "skill", id),
		"source_url":  toolFirstString(raw, "source_url", "sourceUrl", "repo_url", "repoUrl", "repository", "repository_url", "repositoryUrl", "homepage"),
	}
	viewCount, _ := toolFirstNumber(raw, "view_count", "viewCount", "views", "visit_count", "visitCount")
	downloadCount, _ := toolFirstNumber(raw, "download_count", "downloadCount", "downloads")
	item["view_count"] = viewCount
	item["download_count"] = downloadCount
	if tags, exists := toolFirstValue(raw, "custom_tags", "customTags", "tags"); exists {
		item["custom_tags"] = tags
	}
	return item
}

// toolAnyList 将字符串切片或单个字符串转换为 []any，便于输出统一结构。
func toolAnyList(value any) []any {
	switch list := value.(type) {
	case []any:
		if list == nil {
			return []any{}
		}
		return list
	case []string:
		result := make([]any, 0, len(list))
		for _, item := range list {
			result = append(result, item)
		}
		return result
	case string:
		if strings.TrimSpace(list) == "" {
			return []any{}
		}
		return []any{list}
	}
	return []any{}
}

// toolOperationalURLs 提取托管调用地址，缺失时返回空数组，绝不编造地址。
func toolOperationalURLs(data map[string]any) []any {
	if value, exists := toolFirstValue(data, "operational_urls", "operationalUrls"); exists {
		return toolAnyList(value)
	}
	if value := toolFirstString(data, "operational_url", "operationalUrl"); value != "" {
		return []any{value}
	}
	return []any{}
}

// toolFindCommand 在 server_config 中递归查找启动命令与参数。
// 官方详情可能把 server_config 返回为数组（数组元素内含 mcpServers 包装），
// 因此入口同时支持对象和数组；先检查常见包装键，再按字典序遍历其余对象。
func toolFindCommand(value any, depth int) (string, []any) {
	if depth > 8 {
		return "", nil
	}
	switch node := value.(type) {
	case []any:
		for _, item := range node {
			if command, args := toolFindCommand(item, depth+1); command != "" || len(args) > 0 {
				return command, args
			}
		}
		return "", nil
	case map[string]any:
		if command, ok := node["command"].(string); ok && strings.TrimSpace(command) != "" {
			return command, toolAnyList(node["args"])
		}
		for _, key := range []string{"mcpServers", "mcp_servers", "servers", "config", "server_config"} {
			if child, exists := node[key]; exists {
				if command, args := toolFindCommand(child, depth+1); command != "" || len(args) > 0 {
					return command, args
				}
			}
		}
		keys := make([]string, 0, len(node))
		for key := range node {
			if key != "command" {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			switch child := node[key].(type) {
			case map[string]any, []any:
				if command, args := toolFindCommand(child, depth+1); command != "" || len(args) > 0 {
					return command, args
				}
			}
		}
	}
	return "", nil
}

// toolUsageAndSources 生成协议服务的使用说明与下载来源。
// server_config 原样保留供客户端导入；source_url 只作为源码仓库或主页说明，
// 不被包装成可直接下载的安装包地址；托管地址缺失时明确说明，不做编造。
func toolUsageAndSources(data map[string]any, serverConfig any, requestedOperationalURL bool) (map[string]any, map[string]any) {
	sourceURL := toolFirstString(data, "source_url", "sourceUrl", "repo_url", "repoUrl", "repository", "repository_url", "repositoryUrl", "homepage")
	notes := []string{
		"上游启动配置请直接采用 server_config，本工具不编造命令、凭据或托管地址。",
		"本工具不会执行部署、安装或启动任何上游服务。",
		"server_config 中的敏感环境变量（例如访问令牌）需要客户端或用户自行填写，工具不会回显令牌。",
	}
	if requestedOperationalURL {
		notes = append(notes, "已按请求获取托管地址；若上游未返回 operational_urls，本工具不会编造。")
	} else {
		notes = append(notes, "未请求托管地址；如需托管调用地址，请设置 get_operational_url=true 并配置访问令牌。")
	}
	usage := map[string]any{
		"config_source": "server_config",
		"notes":         notes,
	}

	command, args := toolFindCommand(serverConfig, 0)
	sources := map[string]any{
		"source_url": sourceURL,
	}
	if command != "" || len(args) > 0 {
		packageInfo := map[string]any{
			"note": "command/args 原样取自上游 server_config，可用于确认包来源；请按上游文档安装，本工具不执行这些命令。",
		}
		if command != "" {
			packageInfo["command"] = command
		}
		if len(args) > 0 {
			packageInfo["args"] = args
		}
		sources["package_from_config"] = packageInfo
		sources["note"] = "source_url 是上游源码仓库或项目主页，不是可直接下载的安装包链接；安装包来源请以 command/args 指向的包管理器或上游发布页为准。"
	} else {
		sources["note"] = "source_url 是上游源码仓库或项目主页，不是可直接下载的安装包链接；上游配置中未发现可见的 command/args 包来源，也未发现可直接下载的安装包地址。"
	}
	return usage, sources
}

// ---------- modelscope_search_mcps ----------

func handleSearchMCPs(c *hubClient) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := toolParseArgs(req, "search", "page_number", "page_size", "category", "is_hosted")
		if err != nil {
			return toolErrorResult(c, err), nil
		}
		search, err := toolStringArg(args, "search")
		if err != nil {
			return toolErrorResult(c, err), nil
		}
		category, err := toolStringArg(args, "category")
		if err != nil {
			return toolErrorResult(c, err), nil
		}
		pageNumber, pageSize, err := toolPageValues(args, mcpPageProductLimit)
		if err != nil {
			return toolErrorResult(c, err), nil
		}
		isHosted, hasHosted, err := toolBoolArg(args, "is_hosted")
		if err != nil {
			return toolErrorResult(c, err), nil
		}

		payload := map[string]any{
			"search":      search,
			"page_number": pageNumber,
			"page_size":   pageSize,
		}
		filter := map[string]any{}
		if category != "" {
			filter["category"] = category
		}
		if hasHosted {
			filter["is_hosted"] = isHosted
		}
		if len(filter) > 0 {
			payload["filter"] = filter
		}

		queryCtx, cancel := context.WithTimeout(ctx, toolQueryTimeout)
		defer cancel()
		data, err := c.api(queryCtx, http.MethodPut, mcpServersAPIPath, nil, payload)
		if err != nil {
			return toolErrorResult(c, err), nil
		}

		rawItems := toolFirstList(data, "mcp_server_list")
		items := make([]map[string]any, 0, len(rawItems))
		for _, raw := range rawItems {
			items = append(items, toolCompactMCPServer(c, raw))
		}
		return toolJSONResult(c, map[string]any{
			"total_count": toolTotalCount(data, "total_count"),
			"page_number": pageNumber,
			"page_size":   pageSize,
			"items":       items,
		}), nil
	}
}

// ---------- modelscope_get_mcp ----------

func handleGetMCP(c *hubClient) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := toolParseArgs(req, "id", "get_operational_url")
		if err != nil {
			return toolErrorResult(c, err), nil
		}
		id, err := toolRequiredMarketID(args)
		if err != nil {
			return toolErrorResult(c, err), nil
		}
		requestedOperationalURL, _, err := toolBoolArg(args, "get_operational_url")
		if err != nil {
			return toolErrorResult(c, err), nil
		}
		if requestedOperationalURL && c.apiKey == "" {
			return toolErrorResult(c, errors.New("获取托管地址需要配置 MODELSCOPE_API_KEY 或 MODELSCOPE_TOKEN")), nil
		}

		var query url.Values
		if requestedOperationalURL {
			query = url.Values{"get_operational_url": []string{"true"}}
		}
		queryCtx, cancel := context.WithTimeout(ctx, toolQueryTimeout)
		defer cancel()
		data, err := c.api(queryCtx, http.MethodGet, mcpServersAPIPath+"/"+id, query, nil)
		if err != nil {
			return toolErrorResult(c, err), nil
		}

		serverConfig, hasConfig := toolFirstValue(data, "server_config", "serverConfig")
		if !hasConfig {
			serverConfig = map[string]any{}
		}
		envSchema, hasEnvSchema := toolFirstValue(data, "env_schema", "envSchema")
		if !hasEnvSchema {
			envSchema = map[string]any{}
		}
		isHosted, hasIsHosted := toolFirstBool(data, "is_hosted", "isHosted")
		usage, sources := toolUsageAndSources(data, serverConfig, requestedOperationalURL)

		result := map[string]any{
			"id":               id,
			"name":             toolFirstString(data, "name", "Name", "display_name", "displayName"),
			"description":      toolFirstString(data, "description", "Description", "summary"),
			"developer":        toolFirstString(data, "developer", "Developer", "author", "owner"),
			"category":         toolFirstString(data, "category", "Category"),
			"categories":       toolCategoriesValue(data),
			"url":              toolPageURL(c, data, "mcp", id),
			"readme":           toolFirstString(data, "readme", "Readme", "README"),
			"server_config":    serverConfig,
			"env_schema":       envSchema,
			"source_url":       sources["source_url"],
			"operational_urls": toolOperationalURLs(data),
			"usage":            usage,
			"download_sources": sources,
		}
		if hasIsHosted {
			result["is_hosted"] = isHosted
		} else {
			// 上游未提供时输出 null，避免把未知状态伪装成“非托管”。
			result["is_hosted"] = nil
		}
		return toolJSONResult(c, result), nil
	}
}

// ---------- modelscope_search_skills ----------

func handleSearchSkills(c *hubClient) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := toolParseArgs(req,
			"search", "page_number", "page_size", "developer", "category", "license", "custom_tag", "owner")
		if err != nil {
			return toolErrorResult(c, err), nil
		}
		search, err := toolStringArg(args, "search")
		if err != nil {
			return toolErrorResult(c, err), nil
		}
		pageNumber, pageSize, err := toolPageValues(args, skillPageProductLimit)
		if err != nil {
			return toolErrorResult(c, err), nil
		}

		query := url.Values{}
		query.Set("page_number", strconv.Itoa(pageNumber))
		query.Set("page_size", strconv.Itoa(pageSize))
		if search != "" {
			query.Set("search", search)
		}
		filterNames := []struct {
			argument string
			queryKey string
		}{
			{"developer", "filter.developer"},
			{"category", "filter.category"},
			{"license", "filter.license"},
			{"custom_tag", "filter.custom_tag"},
			{"owner", "filter.owner"},
		}
		for _, item := range filterNames {
			value, err := toolStringArg(args, item.argument)
			if err != nil {
				return toolErrorResult(c, err), nil
			}
			if value != "" {
				query.Set(item.queryKey, value)
			}
		}

		queryCtx, cancel := context.WithTimeout(ctx, toolQueryTimeout)
		defer cancel()
		data, err := c.api(queryCtx, http.MethodGet, skillsAPIPath, query, nil)
		if err != nil {
			return toolErrorResult(c, err), nil
		}

		rawItems := toolFirstList(data, "skills")
		items := make([]map[string]any, 0, len(rawItems))
		for _, raw := range rawItems {
			items = append(items, toolCompactSkill(c, raw))
		}
		return toolJSONResult(c, map[string]any{
			"total_count": toolTotalCount(data, "total"),
			"page_number": pageNumber,
			"page_size":   pageSize,
			"items":       items,
		}), nil
	}
}

// ---------- modelscope_get_skill ----------

func handleGetSkill(c *hubClient) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := toolParseArgs(req, "id", "include_content")
		if err != nil {
			return toolErrorResult(c, err), nil
		}
		id, err := toolRequiredMarketID(args)
		if err != nil {
			return toolErrorResult(c, err), nil
		}
		includeContent, _, err := toolBoolArg(args, "include_content")
		if err != nil {
			return toolErrorResult(c, err), nil
		}

		detailCtx, cancelDetail := context.WithTimeout(ctx, toolQueryTimeout)
		data, err := c.api(detailCtx, http.MethodGet, skillsAPIPath+"/"+id, nil, nil)
		cancelDetail()
		if err != nil {
			return toolErrorResult(c, err), nil
		}

		// install_command 原样保留上游值：官方主要返回命令数组，个别条目可能是字符串。
		// 这里只做展示，不解析、不执行，也不改写命令内容。
		installCommand, _ := toolFirstValue(data, "install_command", "installCommand")
		result := map[string]any{
			"id":              id,
			"name":            toolFirstString(data, "name", "Name", "display_name", "displayName"),
			"description":     toolFirstString(data, "description", "Description", "summary"),
			"developer":       toolFirstString(data, "developer", "Developer", "author", "owner"),
			"category":        toolFirstString(data, "category", "Category"),
			"license":         toolFirstString(data, "license", "License"),
			"url":             toolPageURL(c, data, "skill", id),
			"install_command": installCommand,
			"download_url":    c.archiveURL(id, defaultSkillRevision),
			"warnings": []string{
				"download_url 对应的压缩包下载路径属于官方安装脚本依赖的内部接口，不属于公开 API 规范，可能随上游调整；请优先遵照官方安装说明。",
				"本工具不会执行安装命令，也不会执行压缩包中的任何内容。",
			},
			"skill": data,
		}
		parts := make([]string, 0, 2)
		detailText, err := toolEncodeJSON(result)
		if err != nil {
			return toolErrorResult(c, fmt.Errorf("无法编码结果：%w", err)), nil
		}
		parts = append(parts, detailText)

		if includeContent {
			contentCtx, cancelContent := context.WithTimeout(ctx, toolQueryTimeout)
			contentData, err := c.legacy(contentCtx, skillRawAPIPathPrefix+id+"/repo/raw", url.Values{"FilePath": []string{"SKILL.md"}})
			cancelContent()
			if err != nil {
				return toolErrorResult(c, err), nil
			}
			content := toolFirstString(contentData, "Content", "content", "SkillContent", "skillContent")
			if content == "" {
				parts = append(parts, "上游未返回 SKILL.md 正文。")
			} else {
				parts = append(parts, "SKILL.md 正文（原文展示，未执行）：\n"+content)
			}
		}
		return toolPartsResult(c, parts...), nil
	}
}

// ---------- modelscope_download_skill ----------

func handleDownloadSkill(c *hubClient) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// 只接受 id 和 revision；网址、任意路径、overwrite 等参数会因未知属性被拒绝。
		args, err := toolParseArgs(req, "id", "revision")
		if err != nil {
			return toolErrorResult(c, err), nil
		}
		id, err := toolRequiredMarketID(args)
		if err != nil {
			return toolErrorResult(c, err), nil
		}
		revision, err := toolStringArg(args, "revision")
		if err != nil {
			return toolErrorResult(c, err), nil
		}
		if revision == "" {
			revision = defaultSkillRevision
		}
		if err := toolValidateRevision(revision); err != nil {
			return toolErrorResult(c, err), nil
		}

		result, err := c.downloadSkill(ctx, id, revision)
		if err != nil {
			return toolErrorResult(c, err), nil
		}
		if result == nil {
			return toolErrorResult(c, errors.New("下载结束但未返回结果")), nil
		}
		resultText, err := toolEncodeJSON(result)
		if err != nil {
			return toolErrorResult(c, fmt.Errorf("无法编码下载结果：%w", err)), nil
		}
		header := fmt.Sprintf("已下载技能 %s（版本 %s）。文件写入 MODELSCOPE_SKILLS_DIR 指定的目录，未覆盖同名文件，也未执行任何内容。", id, revision)
		return toolPartsResult(c, header, resultText), nil
	}
}

// ---------- 工具注册 ----------

func registerTools(server *mcp.Server, c *hubClient) {
	server.AddTool(&mcp.Tool{
		Name:        "modelscope_search_mcps",
		Title:       "搜索魔搭 MCP 协议服务",
		Description: "搜索魔搭（ModelScope）MCP 市场中的协议服务。可传关键词、分类和是否托管筛选，返回总数、分页信息及精简条目（ID、名称描述、作者分类、浏览/下载计数、页面地址、源码地址）。只读查询，不部署、不安装、不执行任何服务。",
		InputSchema: searchMCPsSchema,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
			OpenWorldHint:  toolBoolPtr(true),
		},
	}, handleSearchMCPs(c))

	server.AddTool(&mcp.Tool{
		Name:        "modelscope_get_mcp",
		Title:       "获取魔搭 MCP 协议服务详情",
		Description: "获取魔搭 MCP 服务的完整 readme、server_config、env_schema、is_hosted、source_url、页面地址和可选托管调用地址。返回 usage 与 download_sources：启动配置以 server_config 为准，本工具不编造命令、凭据或托管地址，不执行部署；source_url 只是源码仓库或主页，不会被冒充为可直接下载的安装包链接。server_config 可原样导入兼容的客户端，敏感环境变量请自行填写。",
		InputSchema: getMCPSchema,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
			OpenWorldHint:  toolBoolPtr(true),
		},
	}, handleGetMCP(c))

	server.AddTool(&mcp.Tool{
		Name:        "modelscope_search_skills",
		Title:       "搜索魔搭技能",
		Description: "搜索魔搭（ModelScope）技能市场。支持关键词、开发者、分类、许可证、自定义标签和所有者过滤，返回总数、分页信息及精简条目（ID、名称描述、作者分类、许可证、浏览/下载计数、页面地址、源码地址）。只读查询，不下载、不安装。",
		InputSchema: searchSkillsSchema,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
			OpenWorldHint:  toolBoolPtr(true),
		},
	}, handleSearchSkills(c))

	server.AddTool(&mcp.Tool{
		Name:        "modelscope_get_skill",
		Title:       "获取魔搭技能详情",
		Description: "获取魔搭技能元数据、install_command、网页地址，以及由官方压缩包路径生成的 download_url。download_url 属于官方安装脚本依赖的内部接口，不是公开 API 规范，可能随上游调整。include_content=true 时同时获取并展示 SKILL.md 正文；正文仅展示，不执行。下载技能请使用 modelscope_download_skill。",
		InputSchema: getSkillSchema,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
			OpenWorldHint:  toolBoolPtr(true),
		},
	}, handleGetSkill(c))

	server.AddTool(&mcp.Tool{
		Name:        "modelscope_download_skill",
		Title:       "下载魔搭技能到本地目录",
		Description: "下载指定魔搭技能到 MODELSCOPE_SKILLS_DIR 指定的本地目录，revision 默认 master。仅在用户明确要求下载技能后调用；不接受任意网址、任意目标路径或覆盖参数，不覆盖同名文件，不执行下载内容。返回实际保存位置。",
		InputSchema: downloadSkillSchema,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: toolBoolPtr(false),
			IdempotentHint:  false,
			OpenWorldHint:   toolBoolPtr(true),
		},
	}, handleDownloadSkill(c))
}

// ---------- 输入参数 Schema（中文说明，严格限制类型和范围） ----------

var searchMCPsSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"search": map[string]any{
			"type":        "string",
			"description": "搜索关键词；留空表示按默认顺序浏览",
		},
		"page_number": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"description": "页码，默认 1；与 page_size 的乘积不得超过 100",
		},
		"page_size": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"maximum":     100,
			"description": "每页条数，默认 20，范围 1 到 100；与 page_number 的乘积不得超过 100",
		},
		"category": map[string]any{
			"type":        "string",
			"description": "分类过滤；留空表示不过滤",
		},
		"is_hosted": map[string]any{
			"type":        "boolean",
			"description": "是否只筛选托管服务；缺省表示不过滤",
		},
	},
	"additionalProperties": false,
}

var getMCPSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"id": map[string]any{
			"type":        "string",
			"description": "市场标识，格式为 作者/名称 或 @作者/名称",
		},
		"get_operational_url": map[string]any{
			"type":        "boolean",
			"description": "是否获取托管调用地址，默认 false；为 true 时必须配置访问令牌",
		},
	},
	"required":             []string{"id"},
	"additionalProperties": false,
}

var searchSkillsSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"search": map[string]any{
			"type":        "string",
			"description": "搜索关键词；留空表示按默认顺序浏览",
		},
		"page_number": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"description": "页码，默认 1；与 page_size 的乘积不得超过 3000",
		},
		"page_size": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"maximum":     100,
			"description": "每页条数，默认 20，范围 1 到 100；与 page_number 的乘积不得超过 3000",
		},
		"developer": map[string]any{
			"type":        "string",
			"description": "开发者过滤；留空表示不过滤",
		},
		"category": map[string]any{
			"type":        "string",
			"description": "分类过滤；留空表示不过滤",
		},
		"license": map[string]any{
			"type":        "string",
			"description": "许可证过滤；留空表示不过滤",
		},
		"custom_tag": map[string]any{
			"type":        "string",
			"description": "自定义标签过滤；留空表示不过滤",
		},
		"owner": map[string]any{
			"type":        "string",
			"description": "所有者过滤；留空表示不过滤",
		},
	},
	"additionalProperties": false,
}

var getSkillSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"id": map[string]any{
			"type":        "string",
			"description": "技能标识，格式为 作者/名称 或 @作者/名称",
		},
		"include_content": map[string]any{
			"type":        "boolean",
			"description": "是否同时获取 SKILL.md 正文，默认 false；正文仅展示，不执行",
		},
	},
	"required":             []string{"id"},
	"additionalProperties": false,
}

var downloadSkillSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"id": map[string]any{
			"type":        "string",
			"description": "技能标识，格式为 作者/名称 或 @作者/名称",
		},
		"revision": map[string]any{
			"type":        "string",
			"description": "技能版本（Git 分支或标签名），默认 master；不接受网址、路径分隔符或路径跳转",
		},
	},
	"required":             []string{"id"},
	"additionalProperties": false,
}
