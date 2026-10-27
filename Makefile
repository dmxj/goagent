VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo v0.1.0)
BIN     := bin/goagent
PKG     := ./cmd/goagent
LDFLAGS := -s -w -X main.version=$(VERSION)
ARGS    ?=

.PHONY: all build test race cover lint fmt vet run serve examples clean help

all: lint test build ## 格式检查 + 测试 + 构建

build: ## 构建 bin/goagent
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

test: ## 运行全部测试（带竞态检测）
	go test -race ./...

cover: ## 生成覆盖率报告 coverage.html
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1
	go tool cover -html=coverage.out -o coverage.html

lint: fmt vet ## gofmt 检查 + go vet

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "以下文件需要 gofmt："; echo "$$out"; exit 1; fi

vet:
	go vet ./...

run: build ## 交互式运行（make run ARGS="-mode plan"）
	./$(BIN) $(ARGS)

serve: build ## 启动 HTTP 服务（make serve ARGS="-addr 127.0.0.1:8787"）
	./$(BIN) serve $(ARGS)

examples: ## 构建全部示例
	go build -o bin/ ./examples/...

clean: ## 清理构建产物
	rm -rf bin/ coverage.out coverage.html

help: ## 显示帮助
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'
