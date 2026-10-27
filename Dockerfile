# syntax=docker/dockerfile:1

# ---------- 构建阶段 ----------
FROM golang:1.24-alpine AS build

# 国内镜像加速：goproxy.cn 是七牛云提供的 Go 模块代理。
# goagent 零第三方依赖，这里主要加速 Go 工具链可能需要的下载；海外环境可通过
#   docker build --build-arg GOPROXY=https://proxy.golang.org,direct .
# 覆盖。如果 apk 也很慢，可以取消下面 sed 一行的注释，切换到阿里云 Alpine 镜像。
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY} CGO_ENABLED=0
# RUN sed -i 's/dl-cdn.alpinelinux.org/mirrors.aliyun.com/g' /etc/apk/repositories

ARG VERSION=v0.1.0
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/goagent ./cmd/goagent

# ---------- 运行阶段 ----------
FROM alpine:3.20

# Agent 的 bash 工具需要常用命令行工具；git 用于代码类任务
RUN apk add --no-cache bash git ca-certificates tzdata \
    && adduser -D -h /home/agent agent
ENV TZ=Asia/Shanghai

COPY --from=build /out/goagent /usr/local/bin/goagent

USER agent
WORKDIR /workspace
EXPOSE 8787

# 容器内需要监听 0.0.0.0；请务必通过 GOAGENT_SERVER_TOKEN 设置访问令牌：
#   docker run -p 8787:8787 -e GOAGENT_API_KEY=... -e GOAGENT_SERVER_TOKEN=... -v $PWD:/workspace goagent
ENTRYPOINT ["goagent"]
CMD ["serve", "-addr", "0.0.0.0:8787"]
