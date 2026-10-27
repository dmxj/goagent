# AGENTS.md —— goagent 项目开发约定

> 本文件会被 goagent（以及其他兼容 AGENTS.md 的编程 Agent）自动加载进系统提示词，
> 同时也是写给人类贡献者的开发约定。

## 项目概况

- 模块：`github.com/dmxj/goagent`，Go 1.24。
- 这是一个**教学项目**：读者会跟着中文教程从零实现它。代码要清晰、规范、可读性优先，避免炫技。
- 目录职责见 `docs/ARCHITECTURE.md`。

## 硬性约束

1. **零第三方依赖**。只能使用 Go 标准库，`go.mod` 中不允许出现 `require`。需要 YAML、JSON Schema、SSE 等能力时自己实现最小子集（参考 `internal/frontmatter`、`tool.SchemaOf`、`llm.ReadSSE`）。
2. 不要在代码、测试、示例或文档中写入任何真实密钥。示例统一使用 `${ENV_VAR}` 或明显的假值。
3. 必须同时能在 Linux、macOS、Windows 上编译；平台相关代码放在 `internal/procutil` 这类带构建标签的文件中。

## 代码风格

- 注释、错误信息、日志使用**中文**；标识符使用英文。
- 每个包都有包注释，说明它解决什么问题、核心设计思路。
- 导出的类型与函数都要有文档注释。
- 错误包装用 `fmt.Errorf("...: %w", err)`；工具执行失败用 `tool.Errorf` 返回给模型，而不是返回 Go error。
- 字符串截断必须保证不切开 UTF-8 字符。
- 并发访问的状态必须加锁，并用 `go test -race` 验证。

## 测试

- 每个包都要有表驱动测试，失败信息用中文写清"得到什么、期望什么"。
- 测试不访问真实网络、不依赖 API Key：模型用 `llm.MockProvider`，HTTP 用 `httptest`，文件用 `t.TempDir()`。
- 涉及配置的测试设置临时 `HOME` 与 `GOAGENT_NO_CLAUDE_SETTINGS=1`，避免读到本机配置。
- 提交前运行：

```bash
gofmt -l . && go vet ./... && go test -race ./...
```

## 常用命令

```bash
make build    # 构建到 bin/goagent
make test     # go test -race ./...
make cover    # 覆盖率报告
make lint     # gofmt + go vet
make run      # 交互式运行
make serve    # 启动 HTTP 服务
```

## 修改指南

- 新增内置工具：在 `tool/builtin` 中用 `tool.Func` 实现，在 `All()` 中注册，补充测试；只读工具设置 `readOnly=true` 以便并行执行与免审批。
- 新增模型厂商：实现 `llm.Provider` 接口，在 `llm.NewProvider` 中注册，用 `httptest` 模拟 SSE 流编写测试。
- 修改 Agent 循环（`agent/agent.go`）时，务必保持事件序列与 `docs/ARCHITECTURE.md` 中的描述一致，并同步更新 README 的事件表。
