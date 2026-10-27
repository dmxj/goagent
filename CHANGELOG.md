# 更新日志

本项目遵循 [语义化版本](https://semver.org/lang/zh-CN/)，格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。

## [v0.1.1] - 2026-10-27

### 新增

- **配置驱动 + 热更新**：模型、参数（`params.max_tokens / temperature / top_p / top_k / thinking_budget / stop / stream / extra_headers / extra_body`）、
  系统提示词（`system_prompt`、`system_prompt_file`、`prompt.append / append_file / sections / extra`）、工具绑定（`tools.enabled / disabled / options`）、
  技能（`skills.dirs / enabled / disabled`）、子 Agent（`agents.*`）、MCP、权限、钩子全部来自配置。`goagent` 与 `goagent serve`
  监视配置文件、提示词文件、技能 / 子 Agent 目录与 AGENTS.md，保存后下一条消息即使用新配置；运行中的轮次继续使用旧快照；
  非法配置保留上一份可用配置并报告错误。新增 `app.Runtime`、`App.Run / Reload / Status / Subscribe / SetModel / SetMode`。
  `reload.enabled: false` 时只留意配置文件本身，运行中改回 `true` 即恢复热更新。
- **HTTP**：`GET /v1/config`（脱敏后的生效配置、来源、版本、最近错误）、`POST /v1/reload`、`GET /v1/events`（`config_reloaded` / `config_error`）、
  `GET /v1/permissions`（等待中的审批）、`GET /v1/traces/{id}/spans/{span}`；`/v1/traces/{id}/tree` 改为返回 JSON 树（`?format=text` 返回文本）；
  `/v1/traces` 支持 `light=1` 并包含进行中的 Span；会话列表与详情返回 `created_at`、`model`、`usage`、`running` 等字段。
- **完整的模型调用 Trace**：`llm.request.params / url / method / headers / body`、`llm.response.status / headers / body / raw_stream / message / bytes`、
  `llm.usage`、`llm.latency_ms`、`llm.retries`、`llm.attempts`；工具 Span 记录完整的 `tool.input` / `tool.output`；大字段上限
  `trace.max_field_bytes`（默认 4MB）并带截断标记；实时事件只带轻量快照。新增 `llm.Recorder` / `llm.Exchange`、`trace.BuildTree`、`trace.DailyFile`。
- **非流式调用**：`params.stream: false` 时 Anthropic 与 OpenAI 兼容协议都走非流式接口，Trace 记录完整响应体。
- 子 Agent 定义支持 `profile`（独立 Provider）；覆盖内置子 Agent 时未写出的字段继承原定义。
- REPL 新增 `/config`、`/reload`；`/model` 可以切换到其它 Provider 的 profile。

### 修复

- **安全**：bash 工具与命令钩子不再继承完整环境变量，默认只传白名单变量并剔除 `*KEY*`、`*TOKEN*`、`*SECRET*`、`*PASSWORD*`、
  `GOAGENT_*`、`ANTHROPIC_*`、`OPENAI_*` 等密钥，可用 `tools.bash_env` 调整（新包 `procenv`）。
- **安全**：bash 权限规则可以被命令拼接绕过（`bash(go test:*)` 放行 `go test ./... && rm -rf ~`，`bash(rm -rf:*)` 挡不住 `cd x && rm -rf y`）。
  现在先把命令拆成子命令：allow 必须覆盖每个子命令，deny / ask 命中任一子命令（含 `$(...)`、`eval`、`sh -c`、`sudo`、`xargs`、`find -exec`）即生效，
  无法分析的结构至少询问；前缀匹配按单词边界进行。
- **安全**："本会话总是允许"存放在进程级共享的 Checker 中，`goagent serve` 下对所有会话生效。现在按会话记录（`permission.Grants`），
  bash 只记住子命令前缀。
- **安全**：文件工具可以通过工作目录内指向外部的符号链接越界访问。
- **安全**：Trace 中可能出现配置中的密钥；现在写入属性时脱敏。
- `server`：同一个审批 ID 提交 3 次以上时，多出的 HTTP 处理 goroutine 永久阻塞。现在提交非阻塞且幂等，重复提交返回 409、未知 ID 返回 404。
- `session` / `agent`：执行工具时进程被 kill -9，保存的会话以悬空 `tool_use` 结尾，`-resume` 后请求被 API 拒绝。现在保存时先以"被中断"
  的结果占位、加载与运行前自动修复；会话文件写入增加 fsync。
- `cmd/goagent`：REPL 空闲时按下的 Ctrl+C 留在信号 channel 中，下一个任务一开始就被中断；现在空闲时提示、2 秒内连按两次退出。
- `cmd/goagent`：CLI 审批器没有串行化，并行子 Agent 的审批提示会交错；`run -json` 把 `main>explore` 转义为 `main\u003eexplore`。
- `llm`：Go 1.23+ 中拨号超时同样满足 `errors.Is(err, context.DeadlineExceeded)`，单次连接超时不会被重试。现在依据调用方 ctx 判断（`RetryableContext`）。
- `llm` / `agent`：重试事件依赖对最外层 `*RetryProvider` 的类型断言，外面再包一层 Provider 就收不到；现在通过 ctx 传递（`llm.WithRetryNotify`）。
- `subagent`：`task` 不是只读工具，plan 模式下连只读的 explore 也无法委派；现在按参数判断（`tool.InputReadOnly`）。
- `subagent`：`subagent.*` Span 不发实时事件，Trace 视图需要手动刷新才能看到子 Agent 子树；子 Agent Span 补充 input/output tokens。
- `agent`：不响应 ctx 的工具超时后会让整个 Agent 永久阻塞；现在超时 5 秒后放弃等待并回填错误。
- `mcp`：服务器发给客户端的请求（如 `ping`）被当成响应，可能顶替 id 相同的在途请求；现在按协议回复（ping → 空结果，其它 → -32601）。
- `memory`：名称不同但文件名相同的记忆会互相覆盖；记忆索引没有上限（现在最多 200 行 / 25KB）；空白的 AGENTS.md 会遮住同目录的 CLAUDE.md；
  修改 AGENTS.md 需要重启才生效（现在热更新）。
- `skill` / `internal/frontmatter`：修改技能 description 需要重启；`description: >` 多行写法被解析成 `>`；嵌套映射会覆盖顶层同名键；带 BOM 或前导空行的文件解析失败；
  `skills/` 下指向目录的符号链接被当成普通文件跳过（现在跟随链接，热更新的指纹也跟随一层目录链接）。
- `trace`：进行中 Span 的 `end` 输出为 `0001-01-01T00:00:00Z`（改用 `omitzero`）；trace 文件名在启动时确定，跨天后仍写旧文件（改为按天滚动）；
  内存导出器只按条数限制（增加字节上限）。
- `server`：关闭服务时不取消正在运行的对话；运行中允许删除 / 压缩会话。

### 变更

- `app.App` 的 `Agent`、`Config`、`Skills`、`Subagents`、`Warnings`、`Memory` 由字段改为方法（返回当前配置版本的值）；运行对话请使用 `App.Run`。
- `permission.Rule.Match` 对 bash 使用"每个子命令都命中"的语义，新增 `MatchAny`；`permission.Request` 增加 `SessionID`。

## [v0.1.0] - 2026-10-27

首个公开版本。

### 新增

- **Agent Loop**：流式调用模型 → 解析工具调用 → 钩子 / 权限 → 执行 → 回填结果的完整循环；支持最大轮数、运行中插话（Steer）、取消、同会话并发保护（`ErrBusy`）。
- **多 Provider**：Anthropic Messages API 与 OpenAI Chat Completions 兼容协议（DeepSeek、通义千问、Kimi、智谱、Ollama、vLLM……），统一的流式事件；带指数退避与 `Retry-After` 的重试包装；离线 `MockProvider`。
- **工具系统**：`tool.Func` 泛型工具与反射生成 JSON Schema；内置 `read_file`、`write_file`、`edit_file`、`list_dir`、`glob`、`grep`、`bash`、`web_fetch`、`todo_write`；只读工具自动并行；输出截断、超时、panic 保护、"先读后改"保护、工作目录越界拒绝。
- **权限**：`default` / `accept_edits` / `plan` / `bypass` 四种模式，`allow` / `ask` / `deny` 规则（`bash(go test:*)`、`write_file(src/**)`、`web_fetch(domain:x.com)`），CLI 与 Web 审批。
- **Hooks**：`SessionStart`、`UserPromptSubmit`、`PreToolUse`、`PostToolUse`、`Stop`、`SubagentStop`、`PreCompact`；支持 Go 函数与 shell 命令（与 Claude Code 协议一致）。
- **记忆**：AGENTS.md / GOAGENT.md / CLAUDE.md 项目指令逐级加载；基于 Markdown 文件的长期记忆与 `memory_*` 工具；自动 / 手动上下文压缩，切分点保证不拆开工具调用与结果。
- **Skills**：兼容 Anthropic Agent Skills 规范，渐进式披露加载。
- **子 Agent**：`task` 工具委派任务，独立上下文、工具过滤、并行执行，事件与 Trace 挂在父 Agent 之下；支持 Markdown 自定义子 Agent。
- **MCP**：stdio 与 Streamable HTTP（JSON / SSE 响应）客户端；`mcp.Server` 用于编写自己的 MCP 服务器。
- **Trace**：树状 Span，内存与 JSONL 文件导出，终端树形查看。
- **HTTP 服务**：`/v1/chat` SSE 流式接口、会话管理、审批、Trace 查询，内置 Web 界面，可选访问令牌。
- **CLI**：交互式 REPL 与斜杠命令、`run` 非交互执行（支持 JSONL 事件输出）、`serve`、`trace`、`sessions`、`config`。
- **配置**：用户级 / 项目级 / 环境变量分层合并，命名模型配置（profiles），可导入 Claude Code 配置，密钥脱敏展示。
- 示例：`quickstart`、`custom-tool`、`mcp-server`、`hooks`、`embed-server`。
- 全部包的表驱动单元测试（MockProvider + httptest，无需网络与密钥），`go test -race` 通过。

### 修复（发布前测试中发现）

- `agent`：同一 Agent 被多个会话并发运行时，`defaults()` 写字段导致数据竞争。
- `agent`：模型输出被截断产生的非法工具参数 JSON 会写入历史，导致后续请求、会话保存与 SSE 推送序列化失败。
- `agent`：调用模型期间被取消时 `StopReason` 为 `error` 而不是 `cancelled`。
- `agent` / `subagent`：`SessionStart` 与 `SubagentStop` 钩子从未被触发。
- `config`：对整个配置文件做 `os.ExpandEnv`，会把钩子命令中的 `$GOAGENT_TOOL_NAME` 等运行时变量提前替换为空；值中含引号时还会破坏 JSON。
- `config`：`Redacted()` 未隐藏 MCP 服务器 env / headers 与 provider headers 中的令牌，短密钥会泄露大部分字符。
- `server`：通过 `/cancel` 取消运行时，收尾的 `error` / `run_end` 事件可能被随机丢弃。
- `server`：内置 Web 界面使用绝对路径且不发送令牌，设置 `-token` 或挂载到子路径后无法使用。
- `mcp`：子进程 stderr 缓冲区存在数据竞争；服务器退出时可能丢失已到达的响应；`mcp.Server` 工具返回 nil 结果时 panic，空工具列表返回 `null`。
- `tool/builtin`：长行截断与 grep 结果截断可能切开 UTF-8 字符；没有 FileTracker 时"先读后改"检查误报。
- `tool` / `prompt`：nil 工具注册表导致 panic；MCP 说明按 map 随机顺序输出，系统提示词不稳定（影响提示词缓存）。
- `llm`：Anthropic 流中缺少 `content_block` 的事件导致 panic。
- `cmd/goagent`：`goagent -h` / `--help` 无法显示帮助。
- `bash` 工具与命令钩子在 Windows 上无法编译（`Setpgid` / `syscall.Kill`），抽出 `internal/procutil` 按平台实现。

[v0.1.1]: https://github.com/dmxj/goagent/releases/tag/v0.1.1
[v0.1.0]: https://github.com/dmxj/goagent/releases/tag/v0.1.0
