---
name: commit-message
description: 根据暂存区的改动生成规范的 Git 提交信息（Conventional Commits，中文描述）。当用户要求写 commit message、提交说明或帮忙提交时使用。
allowed-tools: [bash, read_file]
---

# 生成提交信息

## 步骤

1. 运行 `git status --short` 和 `git diff --cached --stat` 了解改动范围。
   - 如果暂存区为空，告诉用户先 `git add`，或询问是否要暂存全部改动；**不要自作主张执行 `git add -A`**。
2. 运行 `git diff --cached` 阅读具体改动；改动很大时按文件分批阅读。
3. 运行 `git log --oneline -10` 参考项目已有的提交风格，风格不同时以项目为准。
4. 按下面的格式写出提交信息，展示给用户确认。
5. 只有用户明确要求时才执行 `git commit`，并使用 `git commit -F -` 通过标准输入传入信息，避免引号转义问题。

## 格式

```
<type>(<scope>): <一句话摘要，不超过 50 个字符，不加句号>

<正文：说明为什么改、改了什么，每行不超过 72 个字符>

<脚注：关联的 issue，或 BREAKING CHANGE: 说明>
```

**type** 取值：

| type | 含义 |
| --- | --- |
| feat | 新功能 |
| fix | 修复 bug |
| refactor | 重构（不改变行为） |
| perf | 性能优化 |
| test | 测试 |
| docs | 文档 |
| build / ci | 构建与 CI |
| chore | 杂项 |

**scope** 使用改动的包名，例如 `agent`、`llm`、`tool/builtin`。

## 示例

```
fix(agent): 清理历史中不合法的工具参数 JSON

模型输出被 max_tokens 截断时，tool_use 的参数可能是不完整的 JSON。
原样写入会话历史会导致下一次请求与会话保存都因序列化失败而中断。
现在执行前先记录原始参数用于报错，再把历史中的参数替换为 {}。
```

## 注意

- 一个提交只做一件事；改动混杂时建议用户拆分提交。
- 不要在提交信息中包含密钥、内部地址等敏感信息。
