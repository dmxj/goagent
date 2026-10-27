# 更新日志

本项目遵循 [语义化版本](https://semver.org/lang/zh-CN/)，格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。

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

[v0.1.0]: https://github.com/dmxj/goagent/releases/tag/v0.1.0
