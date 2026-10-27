# mcp-server 示例

用 `mcp.Server` 几十行代码就能实现一个 stdio MCP 服务器。本示例提供三个工具：

| 工具 | 说明 | 参数 |
| --- | --- | --- |
| `now` | 当前日期时间 | `timezone`（可选，如 `Asia/Shanghai`） |
| `random` | `[min, max]` 内的随机整数 | `min`、`max` |
| `word_count` | 统计字符数、单词数（中文按字计）与行数 | `text` |

## 构建

```bash
go build -o bin/mcp-demo ./examples/mcp-server
```

## 在 goagent 中使用

在项目的 `.goagent/config.json`（或用户级 `~/.goagent/config.json`）中加入：

```json
{
  "mcp_servers": {
    "demo": {
      "command": "/绝对路径/goagent/bin/mcp-demo"
    }
  }
}
```

也可以不预先构建，直接用 `go run`（首次启动较慢）：

```json
{
  "mcp_servers": {
    "demo": {
      "command": "go",
      "args": ["run", "github.com/dmxj/goagent/examples/mcp-server@latest"]
    }
  }
}
```

启动 `goagent` 后执行 `/tools`，可以看到 `mcp__demo__now`、`mcp__demo__random`、`mcp__demo__word_count`。
工具命名规则与 Claude Code 一致：`mcp__<服务器名>__<工具名>`。

三个工具都标注了 `readOnlyHint`，因此在 default 权限模式下无需审批，并且可以并行执行。
如果想让它们也需要确认，可以在权限配置中加入 `"ask": ["mcp__demo__*"]`。

## 在 Claude Code 中使用

```bash
claude mcp add demo -- /绝对路径/goagent/bin/mcp-demo
```

## 手动调试

MCP 的 stdio 传输就是"每行一个 JSON-RPC 消息"，可以直接用管道测试：

```bash
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"word_count","arguments":{"text":"你好 world"}}}' \
  | go run ./examples/mcp-server
```

注意：stdout 专用于协议消息，服务器的日志写到 stderr。
