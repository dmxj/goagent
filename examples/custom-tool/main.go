// custom-tool 演示如何用 tool.Func 泛型定义类型安全的自定义工具并注册到 Agent。
//
// 参数结构体通过 struct tag 自动生成 JSON Schema：
//
//	json:"name,omitempty"  字段名，带 omitempty 视为可选
//	desc:"说明"            字段描述（模型据此理解参数含义）
//	enum:"a,b"             枚举值
//
// 运行：go run ./examples/custom-tool
package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"

	"github.com/dmxj/goagent/agent"
	"github.com/dmxj/goagent/llm"
	"github.com/dmxj/goagent/session"
	"github.com/dmxj/goagent/tool"
)

// ---------- 工具一：天气查询（假实现） ----------

type weatherArgs struct {
	City string `json:"city" desc:"城市名，例如 北京"`
	Unit string `json:"unit,omitempty" enum:"celsius,fahrenheit" desc:"温度单位，默认 celsius"`
}

var fakeWeather = map[string]float64{"北京": 18, "上海": 22, "广州": 27, "深圳": 28}

func weatherTool() tool.Tool {
	return tool.Func("get_weather", "查询城市当前天气（演示用假数据）", true,
		func(ctx context.Context, in weatherArgs, c *tool.Call) (*tool.Result, error) {
			temp, ok := fakeWeather[in.City]
			if !ok {
				// 工具层面的失败用 tool.Errorf 返回：模型能看到错误并自行修正
				return tool.Errorf("没有 %s 的天气数据，支持：北京、上海、广州、深圳", in.City), nil
			}
			unit := "°C"
			if in.Unit == "fahrenheit" {
				temp, unit = temp*9/5+32, "°F"
			}
			return &tool.Result{
				Content: fmt.Sprintf("%s：晴，%.0f%s", in.City, temp, unit),
				Meta:    map[string]any{"city": in.City}, // Meta 只给 UI/trace，不发给模型
			}, nil
		})
}

// ---------- 工具二：计算器 ----------

type calcArgs struct {
	Op string  `json:"op" enum:"add,sub,mul,div,pow,sqrt" desc:"运算类型"`
	A  float64 `json:"a" desc:"第一个操作数"`
	B  float64 `json:"b,omitempty" desc:"第二个操作数（sqrt 不需要）"`
}

func calculatorTool() tool.Tool {
	return tool.Func("calculator", "进行精确的四则运算、乘方与开方", true,
		func(ctx context.Context, in calcArgs, c *tool.Call) (*tool.Result, error) {
			var r float64
			switch in.Op {
			case "add":
				r = in.A + in.B
			case "sub":
				r = in.A - in.B
			case "mul":
				r = in.A * in.B
			case "div":
				if in.B == 0 {
					return tool.Errorf("除数不能为 0"), nil
				}
				r = in.A / in.B
			case "pow":
				r = math.Pow(in.A, in.B)
			case "sqrt":
				if in.A < 0 {
					return tool.Errorf("负数不能开平方"), nil
				}
				r = math.Sqrt(in.A)
			default:
				return tool.Errorf("未知运算 %q", in.Op), nil
			}
			return tool.Text(fmt.Sprintf("%g", r)), nil
		})
}

func main() {
	reg := tool.NewRegistry(weatherTool(), calculatorTool())

	// 打印自动生成的 Schema，看看模型会收到什么
	for _, spec := range reg.Specs() {
		fmt.Printf("工具 %s：%s\n  schema: %s\n", spec.Name, spec.Description, spec.InputSchema)
	}

	// 用 Mock 模型模拟"并行调用两个工具 → 汇总回答"，无需 API Key。
	// 两个工具都是只读的，Agent 会并行执行它们。
	mock := llm.NewMock(
		llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ToolCall("c1", "get_weather", weatherArgs{City: "北京"}),
			llm.ToolCall("c2", "calculator", calcArgs{Op: "pow", A: 2, B: 10}),
		}},
		llm.AssistantText("北京今天晴，18°C；2 的 10 次方是 1024。"),
	)
	a := &agent.Agent{Provider: mock, Model: "mock", Tools: reg}
	res, err := a.Run(context.Background(), session.New(), "北京天气怎么样？顺便算一下 2 的 10 次方。", func(ev agent.Event) {
		if ev.Type == agent.EventToolResult {
			fmt.Printf("⏺ %s(%s) → %s\n", ev.Tool.Name, ev.Tool.Input, ev.Tool.Output)
		}
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(strings.Repeat("─", 40))
	fmt.Println(res.Text)
}
