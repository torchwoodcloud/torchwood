# syntax=docker/dockerfile:1
# webhook-example 镜像：单文件 Go 接收端（torchwood webhooks 示例，README §0）。
# 与主仓 Dockerfile 同底（alpine:3.21 + 非 root uid 10001）。

FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/webhook-example .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 example
COPY --from=builder /out/webhook-example /usr/local/bin/webhook-example
USER example
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/webhook-example"]
