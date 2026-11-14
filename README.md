# modelscope-hub-mcp

基于官方 [modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk) 的 [ModelScope](https://www.modelscope.cn) 市场查询 MCP 服务，覆盖 MCP 广场与技能市场，提供 5 个工具：

| 工具 | 说明 |
|---|---|
| `modelscope_search_mcps` | 搜索或分页浏览 MCP 广场中的协议服务，返回总数、分页信息和精简条目 |
| `modelscope_get_mcp` | 获取 MCP 详情：完整 `readme`、`server_config`、`env_schema`、托管状态、来源与可选托管调用地址 |
| `modelscope_search_skills` | 搜索或分页浏览技能市场，支持开发者、分类、许可证等过滤 |
| `modelscope_get_skill` | 获取技能元数据、安装命令、压缩包地址，可选读取 `SKILL.md` 正文 |
| `modelscope_download_skill` | 下载技能压缩包并安全解压到本地目录，不安装、不执行 |

`id` 使用搜索结果中的原始值，例如 `@modelcontextprotocol/fetch`、`@anthropics/skill-creator`。保留开头的 `@`，不要传网页链接、本地路径或带路径跳转的值。

## 工具参数

### modelscope_search_mcps

| 参数 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `search` | 否 | 空 | 搜索关键词；留空表示按默认顺序浏览 |
| `page_number` | 否 | `1` | 页码；与 `page_size` 的乘积不得超过 100 |
| `page_size` | 否 | `20` | 每页条数，范围 1 到 100；与 `page_number` 的乘积不得超过 100 |
| `category` | 否 | 空 | 分类过滤；留空表示不过滤 |
| `is_hosted` | 否 | 空 | 是否只筛选托管服务；缺省表示不过滤 |

### modelscope_get_mcp

| 参数 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `id` | 是 | - | 市场标识，格式为 `作者/名称` 或 `@作者/名称` |
| `get_operational_url` | 否 | `false` | 是否获取托管调用地址；为 `true` 时必须配置访问令牌 |

### modelscope_search_skills

| 参数 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `search` | 否 | 空 | 搜索关键词；留空表示按默认顺序浏览 |
| `page_number` | 否 | `1` | 页码；与 `page_size` 的乘积不得超过 3000 |
| `page_size` | 否 | `20` | 每页条数，范围 1 到 100；与 `page_number` 的乘积不得超过 3000 |
| `developer` | 否 | 空 | 开发者过滤；留空表示不过滤 |
| `category` | 否 | 空 | 分类过滤；留空表示不过滤 |
| `license` | 否 | 空 | 许可证过滤；留空表示不过滤 |
| `custom_tag` | 否 | 空 | 自定义标签过滤；留空表示不过滤 |
| `owner` | 否 | 空 | 所有者过滤；留空表示不过滤 |

按技能作者筛选通常使用 `developer`，不要与 `owner` 混淆。

### modelscope_get_skill

| 参数 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `id` | 是 | - | 技能标识，格式为 `作者/名称` 或 `@作者/名称` |
| `include_content` | 否 | `false` | 是否同时获取 `SKILL.md` 正文；正文仅展示，不执行 |

### modelscope_download_skill

| 参数 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `id` | 是 | - | 技能标识，格式为 `作者/名称` 或 `@作者/名称` |
| `revision` | 否 | `master` | 技能版本（Git 分支或标签名）；只允许安全单段名称，不接受网址、路径分隔符或路径跳转 |

下载工具不接受任意下载网址、任意输出路径或覆盖开关。工具只在用户明确要求下载技能后调用。

## 返回示例

搜索结果只保留必要字段，返回缩进 JSON 文本，例如 `modelscope_search_mcps`：

```json
{
  "items": [
    {
      "categories": [
        "browser-automation"
      ],
      "category": "",
      "description": "该服务器使大型语言模型能够检索和处理网页内容，将HTML转换为markdown格式，以便于更轻松地使用。",
      "developer": "",
      "download_count": 0,
      "id": "@modelcontextprotocol/fetch",
      "name": "Fetch网页内容抓取",
      "source_url": "",
      "url": "https://www.modelscope.cn/mcp/servers/@modelcontextprotocol/fetch",
      "view_count": 650216
    }
  ],
  "page_number": 1,
  "page_size": 2,
  "total_count": 45
}
```

技能下载成功返回实际目录、文件数量、字节数、下载地址和压缩包 SHA-256；目标目录名由技能标识生成，例如 `@anthropics/skill-creator` 下载到 `skills/anthropics--skill-creator`。原有同名目录存在时会直接报错，不会覆盖。

## 调用示例

以下仅展示工具参数，不会自行执行查询结果中的命令：

```json
{"name":"modelscope_search_mcps","arguments":{"search":"fetch","page_size":5}}
```

```json
{"name":"modelscope_get_mcp","arguments":{"id":"@modelcontextprotocol/fetch"}}
```

```json
{"name":"modelscope_search_skills","arguments":{"search":"pdf","developer":"anthropics","page_size":5}}
```

```json
{"name":"modelscope_get_skill","arguments":{"id":"@anthropics/skill-creator","include_content":true}}
```

仅在用户明确要求下载这个技能后，调用：

```json
{"name":"modelscope_download_skill","arguments":{"id":"@anthropics/skill-creator","revision":"master"}}
```

## 构建

```bash
go build -o modelscope-hub-mcp.exe .
```

依赖 Go 1.27.3+、官方 go-sdk v1.7.0。编译后 `go vet ./...` 应无本项目代码告警。

本机依赖已存在时，可使用离线构建与验证：

```powershell
$env:GOTOOLCHAIN = 'local'
$env:GOPROXY = 'off'
$env:GOSUMDB = 'off'
$env:GOFLAGS = '-mod=readonly'

go mod verify
go vet ./...
go test ./...
go build -trimpath -o .\modelscope-hub-mcp.exe .
```

不需要执行 `go mod tidy`。如果本机缺少依赖，离线构建会失败；应先确认缺失项，再决定是否安装，不能在这里自动补包。

## 环境变量

| 变量 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `MODELSCOPE_API_KEY` | 否 | 空 | 魔搭访问令牌；公开搜索与详情目前可以匿名读取 |
| `MODELSCOPE_TOKEN` | 否 | 空 | 兼容变量，仅在前一项为空时使用 |
| `MODELSCOPE_HUB_BASE_URL` | 否 | `https://www.modelscope.cn` | 站点根地址，必须不带接口路径、查询参数和凭据；本地测试可用 `http://127.0.0.1:端口` |
| `MODELSCOPE_SKILLS_DIR` | 否 | 进程工作目录下的 `skills` | 技能下载根目录，启动时转换为绝对路径，建议显式配置 |

`get_operational_url=true` 时必须配置 `MODELSCOPE_API_KEY` 或 `MODELSCOPE_TOKEN`。生图服务使用的 `MODELSCOPE_BASE_URL` 不用于本项目，避免误连推理接口。

令牌只从进程环境读取，不会自动保存。示例中不含真实令牌；已在聊天或其他公开位置发送过的令牌应先撤销，再通过客户端的进程环境注入新令牌，不要提交到代码仓库。

## 调用流程

所有请求都发往 `MODELSCOPE_HUB_BASE_URL` 指向的固定站点，工具参数不能替换主机或注入请求头。

```
① 搜索 MCP：PUT /openapi/v1/mcp/servers
   body: {"search":"fetch","page_number":1,"page_size":20,"filter":{"category":"...","is_hosted":true}}
   → data.mcp_server_list / data.total_count

② MCP 详情：GET /openapi/v1/mcp/servers/{id}
   ?get_operational_url=true（需要 Authorization: Bearer <key>）
   → data.server_config / data.env_schema / data.readme / data.is_hosted

③ 技能搜索：GET /openapi/v1/skills?page_number=1&page_size=20&search=pdf&filter.developer=anthropics
   → data.skills / data.total

④ 技能详情：GET /openapi/v1/skills/{id}
   → data.install_command 等
   正文另取：GET /api/v1/skills/{id}/repo/raw?FilePath=SKILL.md

⑤ 技能下载：GET /skills/{id}/archive/zip/{revision}
   → 校验压缩包大小、展开大小、条目数和路径后解压
```

单次上游查询超时 30 秒，技能下载总超时 120 秒。MCP 搜索受上游限制，`page_number × page_size` 不能超过 100，不能通过不断翻页遍历全市场。技能搜索本服务将分页乘积限制为 3000，上游仍可能施加额外限制。

## MCP 客户端接入

### opencode

在 opencode 配置（`opencode.jsonc`，全局或项目级）的 `mcp.servers` 段添加。当前官方格式是 `mcp.servers`，不要与旧版直接在 `mcp` 下放服务器名称的结构混用：

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "servers": {
      "modelscope-hub": {
        "type": "local",
        "disabled": false,
        "command": ["E:/code/test/go/mcp/modelscope-hub-mcp/modelscope-hub-mcp.exe"],
        "environment": {
          "MODELSCOPE_API_KEY": "ms-xxxx",
          "MODELSCOPE_SKILLS_DIR": "E:/code/test/go/mcp/modelscope-hub-mcp/skills"
        },
        "timeout": {"execution": 180000}
      }
    }
  }
}
```

公开查询不需要令牌时，可以删除 `MODELSCOPE_API_KEY`。保存后重启 opencode，或运行 `opencode mcp list` 检查连接。以上配置只供合并参考，本服务不会自动修改现有客户端配置。

### Claude Code

项目级配置放根目录 `.mcp.json`（随仓库共享）；用户级配置放 `~/.claude.json` 的 `mcpServers` 键下。`command` 是**字符串**（非数组），`env` 支持 `${VAR}` 展开（从 shell 环境取）。其他支持 `mcpServers` 结构的客户端也可使用相同配置：

```json
{
  "mcpServers": {
    "modelscope-hub": {
      "type": "stdio",
      "command": "E:/code/test/go/mcp/modelscope-hub-mcp/modelscope-hub-mcp.exe",
      "args": [],
      "env": {
        "MODELSCOPE_API_KEY": "ms-xxxx",
        "MODELSCOPE_SKILLS_DIR": "E:/code/test/go/mcp/modelscope-hub-mcp/skills"
      }
    }
  }
}
```

也可以用 CLI 添加：`claude mcp add modelscope-hub -- E:/code/test/go/mcp/modelscope-hub-mcp/modelscope-hub-mcp.exe`，之后用 `claude mcp get modelscope-hub` 查看。修改后重启 Claude Code 生效。

### OpenAI Codex CLI

配置在 `~/.codex/config.toml`，`[mcp_servers.<名字>]` 段，`command` 为字符串，环境变量用 `env` 表：

```toml
[mcp_servers.modelscope-hub]
command = "E:/code/test/go/mcp/modelscope-hub-mcp/modelscope-hub-mcp.exe"
args = []
env = { MODELSCOPE_API_KEY = "ms-xxxx", MODELSCOPE_SKILLS_DIR = "E:/code/test/go/mcp/modelscope-hub-mcp/skills" }
```

也可以用 CLI 添加：`codex mcp add modelscope-hub -- E:/code/test/go/mcp/modelscope-hub-mcp/modelscope-hub-mcp.exe`（加 `--scope project` 则写入项目级配置）。修改后重启 Codex 生效。

> 三个客户端差异速记：opencode 用 `command` 数组 + `environment`；Claude Code 用 `command` 字符串 + `env`；Codex 用 TOML 段 + `env` 表。都通过 stdio 拉起本进程，环境变量注入令牌与技能目录。

## 手动测试(stdio)

```powershell
$env:MODELSCOPE_SKILLS_DIR = 'E:/code/test/go/mcp/modelscope-hub-mcp/skills'
& { '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}'; Start-Sleep -Seconds 2 } | E:\code\test\go\mcp\modelscope-hub-mcp\modelscope-hub-mcp.exe
```

`Start-Sleep` 用于让管道保持打开；如果直接一次性写入后立即 EOF，进程可能在输出响应前关闭。正常时会返回 `serverInfo` 和协商后的 `protocolVersion`。

## 传输

支持 **stdio**（默认，三个客户端都用这种）和 **http**：

```bash
modelscope-hub-mcp.exe                                      # stdio
modelscope-hub-mcp.exe -transport http -http-addr 127.0.0.1:8080
```

HTTP 传输使用 StreamableHTTP，连接地址为 `http://127.0.0.1:8080/`。本服务未实现客户端鉴权；**不要直接暴露到公网**。所有可连接的客户端都能使用服务配置的上游令牌并调用下载工具；远程使用需要自行部署鉴权和安全连接网关。

## 实现说明

- 非类型化 handler（`server.AddTool` + `mcp.ToolHandler`），结果只发 `TextContent`，不发 `structuredContent`（部分客户端要求 record，字符串会导致校验失败）
- 错误统一转 `IsError: true` result，LLM 可直接看到错误信息
- 参数严格校验：拒绝未知属性、类型不符和越界值
- 令牌只从进程环境读取；错误和结果统一脱敏
- 所有上游请求使用固定站点地址；跨源重定向不会转发 `Authorization` 和 `Cookie`
- 技能 ID 只允许 `作者/名称` 两段；`revision` 只允许字母、数字、点、短横线、下划线和加号
- 同一进程内下载串行执行；下载、校验和解压先在临时目录完成，成功后发布最终目录，失败会清理本次临时内容
- 下载限制：压缩包 64 MiB、展开总大小 256 MiB、条目数 5000
- 目标目录名由技能标识生成：去掉开头的 `@`，把 `/` 替换为 `--`

## 技能下载安全

- 文件只能落在 `MODELSCOPE_SKILLS_DIR` 中，不覆盖已有目标，不执行下载内容
- 拒绝路径穿越、绝对路径、反斜杠路径、符号链接及特殊文件、重复路径、大小写冲突和不兼容的 Windows 文件名
- 兼容官方压缩包外层目录，最终技能根目录必须包含普通文件 `SKILL.md`，保留合法的子目录资源
- 返回实际目录、文件数量、字节数、下载地址及压缩包 SHA-256，便于核对
- 下载完成不等于安装到所有客户端，需要按客户端约定配置技能目录；技能中的脚本和指令仍应先审查再使用
- 不要启动多个进程同时写入同一个技能目录，也不要让不可信用户修改该目录

## 常见错误

| 现象 | 原因 |
|---|---|
| `MODELSCOPE_HUB_BASE_URL 必须是无路径、凭据和查询参数的站点地址` | 填了接口路径、查询参数或凭据 |
| `站点地址必须使用安全连接，本地测试地址除外` | 使用了非 https 且不是本机地址的站点 |
| `id 必须是 @作者/名称 或 作者/名称，不接受网址及路径跳转` | 传了网页链接、路径或多段标识 |
| `参数 "id" 不能为空` | 没有传 `id` |
| `参数 "page_number" 与 "page_size" 的乘积不能超过 100` | MCP 搜索翻页过深 |
| `参数 "page_number" 与 "page_size" 的乘积不能超过 3000` | 技能搜索翻页过深 |
| `获取托管地址需要配置 MODELSCOPE_API_KEY 或 MODELSCOPE_TOKEN` | 请求 `get_operational_url=true` 但没配令牌 |
| `不支持的参数：...` | 传了未声明的参数 |
| `上游接口返回状态 401` | 令牌无效或已过期 |
| `上游接口返回状态 429` | 触发上游限流，稍后重试 |
| `下载技能失败：目标技能目录已存在，拒绝覆盖：...` | 同名目录已存在；需自行移动或删除后再下载 |
| `下载技能失败：技能压缩包缺少根目录 SKILL.md 文件` | 压缩包不是有效技能包 |
| `下载技能失败：技能版本名只能包含字母、数字、点、短横线、下划线和加号，且不能包含路径分隔符` | `revision` 含路径分隔符或非法字符 |

## 已知限制

- MCP 搜索的 `page_number × page_size` 不能超过 100，无法遍历全市场；技能搜索本服务限制为 3000，上游还可能额外限制
- 每页最多 100 条，默认 20 条
- `source_url` 是上游仓库或项目主页，**不是保证可直接下载的安装包**；`server_config` 中的命令和参数也只是使用线索，本服务不执行
- `modelscope_get_mcp` 原样保留上游 `server_config`、`env_schema` 和 `readme`；不同客户端的配置结构可能需要转换。不存在直接下载地址时会明确说明，不拼接不存在的二进制地址
- `download_url` 对应 `/skills/{id}/archive/zip/{revision}`，是官方安装脚本依赖的内部接口，不属于公开 API 规范，稳定性弱于正式公开接口
- 未启用 `include_content` 时不会额外请求 `SKILL.md` 正文
- `get_operational_url` 只读取已有托管实例地址；本项目不创建部署，也不保证所有条目都有可用的托管地址
- 默认版本为 `master`，不能假设每个技能都发布了同名标签
- 下载工具不安装技能、不执行压缩包内容，也不会替用户删除已有目录
- HTTP 传输没有鉴权，只适合本机或受控网络
- `MODELSCOPE_BASE_URL` 不属于本项目，配置它不会改变魔搭市场接口地址

## 测试

默认测试只访问本机模拟服务，压缩包由测试程序生成，不安装依赖、不下载真实技能。覆盖工具协议、查询参数与输出、错误处理、认证跳转及下载路径安全。

若要单独验证官方公共查询，可显式启用：

```powershell
$env:MODELSCOPE_LIVE_TEST = '1'
go test -run TestLivePublicAPI -v -count=1 .
Remove-Item Env:\MODELSCOPE_LIVE_TEST
```

此用例不使用真实令牌、不触发部署或下载。真实技能包分发、私有资源权限和各客户端加载行为不属于离线测试的保证范围。

## 官方依据

- [魔搭公开接口规范](https://www.modelscope.cn/.well-known/openapi.json)
- [魔搭技能安装说明](https://www.modelscope.cn/docs/skills/install)
- [魔搭 MCP 广场](https://www.modelscope.cn/mcp)
- [魔搭技能市场](https://www.modelscope.cn/skills)
- [OpenCode MCP 配置](https://opencode.ai/docs/mcp-servers)
