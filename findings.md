# 已确认信息

## 本地架构
- 新目录为 modelscope-hub-mcp，与生图服务独立。
- 沿用 go-sdk v1.7.0 和现有模块校验文件；本机 Go 1.27.3 与完整依赖缓存可离线验证。
- 使用非类型化工具处理器与文本结果，避免结构化结果兼容问题；日志只写标准错误。
- 提供标准输入输出及仅默认监听回环地址的流式网络传输。
- 查询和工具注册为一条写入任务，下载与解压为另一条独立写入任务，文档和最终验证由主会话负责。

## 官方接口
- 基础地址为 https://www.modelscope.cn/openapi/v1。
- 协议市场搜索采用 PUT /mcp/servers，请求体包含 search、filter、page_number、page_size。
- 协议服务详情采用 GET /mcp/servers/{id}，包含 readme、server_config、source_url、env_schema。
- 协议市场分页要求 page_number × page_size 不大于 100。
- 技能搜索采用 GET /skills，支持关键词及开发者、分类等查询参数。
- 技能元数据采用 GET /skills/{id}，不包含技能正文，包含安装命令与来源地址。
- 当前公开搜索和详情均可匿名读取；认证请求使用访问令牌请求头。
- 真实技能详情的 install_command 为字符串数组，应完整保留，不能按单个字符串提取；已匿名核对 skill-creator 返回三条安装命令及 32987 字符正文。
- 技能正文兼容接口为 /api/v1/skills/{id}/repo/raw?FilePath=SKILL.md。
- 官方安装脚本使用 /skills/{id}/archive/zip/master 下载技能压缩包；该路径不属于公开接口规范，应独立封装。
- 不自动部署、安装或执行查询结果中的任何命令。

## 证据
- https://www.modelscope.cn/.well-known/openapi.json
- https://www.modelscope.cn/llms.txt
- https://www.modelscope.cn/docs/skills/install
- 前一轮已对协议搜索、协议详情、技能搜索、技能详情及技能正文进行了公共读取验证。

## 参考架构与实现边界
- 沿用同级服务的 Go 协议库版本及非类型化工具注册，返回文本内容，业务错误通过结果错误标记表达。
- 入口、固定站点客户端、查询工具和下载工具分别成文件，没有引入新依赖。
- 模块依赖清单和校验和沿用同级生图服务；本机缓存齐全，可以禁用联网完成验证。
- 官方协议服务配置实际可能是数组包装，而不只是单个对象；包来源提取必须保留并处理该结构。
- 技能列表真实响应键为 skills 和 total；协议列表真实响应键为 mcp_server_list 和 total_count。
- 当前 OpenCode 官方配置使用 mcp.servers，环境替换语法为 {env:变量名}，执行超时使用对象中的 execution 字段。
