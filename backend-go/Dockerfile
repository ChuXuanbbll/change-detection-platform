# ---- 编译阶段 ----
FROM golang:1.22-alpine AS build
WORKDIR /src
ENV GOFLAGS=-mod=readonly
ENV CGO_ENABLED=0
ENV GOPROXY=https://goproxy.cn,direct
# 使用国内 Go 代理，避免 proxy.golang.org 在容器内网络受限
COPY go.mod go.sum* ./
RUN go mod download && go mod verify
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ---- 运行阶段 ----
FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/server /app/server
COPY config.yaml /app/config.yaml
ENV TZ=Asia/Shanghai
# 以 root 运行：共享卷 /data 属主为 root，非 root 用户无法写数据目录
# （单机内网部署，容器间无敏感进程边界；如需最小权限可改用具名卷属主方案）
EXPOSE 8080
ENTRYPOINT ["/app/server"]