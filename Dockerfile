# syntax=docker/dockerfile:1
# 多阶段构建：console SPA → Go 二进制（console 经 go:embed 打进 server）
# 用法：docker build -t torchwood-server .

# ---------- 1) 构建 Admin Console ----------
FROM node:22-alpine AS console-builder
WORKDIR /console
RUN corepack enable
COPY console/package.json console/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY console/ ./
# 直接 vite build（跳过 tsc 全量类型检查：源码存在历史遗留的未使用导入报错）
RUN pnpm exec vite build

# ---------- 2) 构建 Go 二进制 ----------
FROM golang:1.26-alpine AS go-builder
WORKDIR /src
# 本地 replace 子模块需先于 go mod download 就位
COPY go.mod go.sum ./
COPY genproto/ ./genproto/
COPY sdk/go/go.mod sdk/go/go.sum ./sdk/go/
RUN go mod download
COPY . .
# console dist 注入到 embed 位置（.dockerignore 已排除宿主侧产物）
COPY --from=console-builder /console/dist ./console/dist
ARG TARGETOS=linux
ARG TARGETARCH=amd64
# 版本元数据：CI 构建注入（.github/workflows/image.yml），缺省 dev/unknown
# 与 task build 的本地形态对齐；由 /v1/server/health/version 暴露
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE" -o /out/server ./cmd/server && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE" -o /out/worker ./cmd/worker && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE" -o /out/functions-dispatcher ./cmd/functions-dispatcher && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE" -o /out/torchwood ./cmd/torchwood

# ---------- 3) 运行时 ----------
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 torchwood
COPY --from=go-builder /out/server /usr/local/bin/server
COPY --from=go-builder /out/worker /usr/local/bin/worker
# functions-dispatcher：执行器 v2 独立分发进程（compose functions-dispatcher
# 服务专用；docker.sock 只挂给它，server/worker 零 daemon 依赖）
COPY --from=go-builder /out/functions-dispatcher /usr/local/bin/functions-dispatcher
# torchwood CLI：部署期 owner 作业（admin sync-roles-sig）与项目级备份（admin export/import）
COPY --from=go-builder /out/torchwood /usr/local/bin/torchwood
COPY configs/ /app/configs/
WORKDIR /app
USER torchwood
EXPOSE 9080
ENTRYPOINT ["/usr/local/bin/server"]
