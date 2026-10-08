# ---- 构建阶段 ----
# 用 golang 镜像编译静态二进制。零第三方依赖，产物不依赖 libc。
FROM golang:1.23-alpine AS builder

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# 先只拷 go.mod 让依赖层可缓存（本项目无外部依赖，这一步几乎瞬时完成）
COPY go.mod ./
RUN go mod download 2>/dev/null || true

COPY . .

# CGO_ENABLED=0 产出纯静态二进制，才能放进 scratch/alpine 里跑
# -trimpath 去掉构建机的绝对路径（可复现构建，也顺手去掉本机信息）
# -ldflags "-s -w" 去符号表，体积从 ~11MB 降到 ~7MB
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" \
    -o /out/wb2go ./cmd/wb2go

# ---- 运行阶段 ----
FROM alpine:3.20

# ca-certificates：走 HTTPS 上游与热更新下载时需要根证书
# tzdata：容器时区，影响定时任务的"整点"判定
# curl：HEALTHCHECK 用
RUN apk add --no-cache ca-certificates tzdata curl \
    && adduser -D -u 10001 wb2go

ENV TZ=Asia/Shanghai \
    HOST=0.0.0.0 \
    PORT=8788

WORKDIR /app

COPY --from=builder /out/wb2go /app/wb2go
RUN chmod +x /app/wb2go \
    && mkdir -p /app/accounts /app/data /app/conf \
    && chown -R wb2go:wb2go /app

USER wb2go

EXPOSE 8788

# 用 /healthz 而非 /status：前者恒不鉴权，探针不需要持密钥
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -fsS "http://127.0.0.1:${PORT}/healthz" || exit 1

# exec 形式让二进制成为 PID 1，直接收到 SIGTERM 实现优雅退出
CMD ["/app/wb2go", "-config", "/app/conf/config.json"]

# 面板热更新（容器自升级）需要额外挂载 docker.sock，见 docker-compose.yml：
#   -v /var/run/docker.sock:/var/run/docker.sock
# 不挂也能正常跑，只是面板里点"立即更新"会提示改用 docker pull 升级
