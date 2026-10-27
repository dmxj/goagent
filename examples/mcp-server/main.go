// mcp-server 是一个用 goagent 的 mcp.Server 实现的 stdio MCP 服务器，提供三个工具：
//
//	now         返回当前时间（可指定时区）
//	random      生成指定范围内的随机整数
//	word_count  统计文本的字符数、单词数与行数
//
// 它可以被 goagent、Claude Code 或任何支持 MCP 的客户端使用。配置方法见同目录 README.md。
//
// 注意：stdio 传输中 stdout 专门用于协议消息，日志必须写到 stderr。
package main

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dmxj/goagent/mcp"
	"github.com/dmxj/goagent/tool"
)

type nowArgs struct {
	Timezone string `json:"timezone,omitempty" desc:"IANA 时区名，例如 Asia/Shanghai，默认本地时区"`
}

type randomArgs struct {
	Min int `json:"min" desc:"最小值（包含）"`
	Max int `json:"max" desc:"最大值（包含）"`
}

type wordCountArgs struct {
	Text string `json:"text" desc:"要统计的文本"`
}

func tools() []tool.Tool {
	return []tool.Tool{
		tool.Func("now", "返回当前日期和时间", true, func(ctx context.Context, in nowArgs, c *tool.Call) (*tool.Result, error) {
			loc := time.Local
			if in.Timezone != "" {
				l, err := time.LoadLocation(in.Timezone)
				if err != nil {
					return tool.Errorf("未知时区 %q：%v", in.Timezone, err), nil
				}
				loc = l
			}
			return tool.Text(time.Now().In(loc).Format("2006-01-02 15:04:05 Monday MST")), nil
		}),
		tool.Func("random", "生成 [min, max] 范围内的随机整数", true, func(ctx context.Context, in randomArgs, c *tool.Call) (*tool.Result, error) {
			if in.Max < in.Min {
				return tool.Errorf("max(%d) 不能小于 min(%d)", in.Max, in.Min), nil
			}
			return tool.Text(fmt.Sprint(in.Min + rand.IntN(in.Max-in.Min+1))), nil
		}),
		tool.Func("word_count", "统计文本的字符数、单词数（中文按字计）与行数", true, func(ctx context.Context, in wordCountArgs, c *tool.Call) (*tool.Result, error) {
			words := 0
			for _, f := range strings.Fields(in.Text) {
				han := 0
				for _, r := range f {
					if unicode.Is(unicode.Han, r) {
						han++
					}
				}
				if han > 0 {
					words += han // 中文按字计数
					if han < utf8.RuneCountInString(f) {
						words++ // 中英混排的片段，把非中文部分算作一个词
					}
				} else {
					words++
				}
			}
			lines := 0
			if in.Text != "" {
				lines = strings.Count(in.Text, "\n") + 1
			}
			return tool.Text(fmt.Sprintf("字符数：%d\n单词数：%d\n行数：%d", utf8.RuneCountInString(in.Text), words, lines)), nil
		}),
	}
}

func main() {
	log.SetOutput(os.Stderr) // stdout 留给 MCP 协议
	srv := &mcp.Server{Name: "goagent-demo", Version: "0.1.0", Tools: tools()}
	log.Printf("goagent-demo MCP 服务器已启动（stdio），提供 %d 个工具", len(srv.Tools))
	if err := srv.ServeStdio(context.Background(), os.Stdin, os.Stdout); err != nil {
		log.Fatal(err)
	}
}
