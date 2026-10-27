// embed-server 演示如何把 goagent 的 HTTP/SSE 接口嵌入你自己的 Web 服务：
// 业务路由与 Agent 路由共用一个 http.Server，Agent 挂在 /agent/ 前缀下。
//
// 运行：
//
//	go run ./examples/embed-server            # 没有 GOAGENT_API_KEY 时使用 mock 模型（复述输入）
//	open http://127.0.0.1:9090/agent/         # 内置 Web 界面
//
//	curl -N http://127.0.0.1:9090/agent/v1/chat -H 'Authorization: Bearer demo-token' \
//	     -d '{"message":"你好"}'
//
// 安全提示：Agent 能执行命令、读写文件。示例设置了访问令牌并只监听本机；
// 部署到公网前务必加上鉴权、限流，并把工作目录限制在沙箱中。
package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/dmxj/goagent/app"
	"github.com/dmxj/goagent/config"
	"github.com/dmxj/goagent/llm"
	"github.com/dmxj/goagent/server"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cfg, err := config.Load("") // 当前目录 + ~/.goagent + 环境变量
	if err != nil {
		log.Fatal(err)
	}
	opts := app.Options{}
	if os.Getenv("GOAGENT_API_KEY") == "" {
		log.Println("未设置 GOAGENT_API_KEY，使用 mock 模型")
		opts.Provider = &llm.MockProvider{Handler: llm.EchoHandler}
	}
	// WebApprover 把需要审批的工具调用以 permission_request 事件推给前端，
	// 前端调用 POST /v1/permissions/{id} 做决定。它必须与 app 共用同一个实例。
	approver := server.NewWebApprover()
	opts.Approver = approver
	a, err := app.New(ctx, cfg, opts)
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()

	agentSrv := server.New(a, approver)
	agentSrv.Token = envOr("DEMO_TOKEN", "demo-token") // /v1/* 需要 Authorization: Bearer <token>
	agentSrv.Log = slog.Default()

	mux := http.NewServeMux()
	// 你自己的业务路由
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { // {$} 只匹配根路径本身
		_, _ = w.Write([]byte("这是我的业务服务。Agent 界面在 /agent/\n"))
	})
	mux.HandleFunc("GET /api/hello", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"hello":"world"}`))
	})
	// 挂载 goagent：StripPrefix 后，/agent/v1/chat 会被路由到 goagent 的 /v1/chat
	mux.Handle("/agent/", http.StripPrefix("/agent", agentSrv.Handler()))

	hs := &http.Server{Addr: envOr("ADDR", "127.0.0.1:9090"), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	log.Printf("服务已启动：http://%s （Agent：/agent/，令牌 %s）", hs.Addr, agentSrv.Token)
	if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
