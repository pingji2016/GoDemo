# 第一阶段：构建阶段
FROM golang:1.23-alpine AS builder

# 设置环境变量，确保构建出的二进制文件是静态链接的
ENV CGO_ENABLED=0
ENV GOOS=linux
ENV GOARCH=amd64

# 设置工作目录
WORKDIR /app

# 复制依赖文件并下载依赖
# 利用 Docker 缓存层，如果 go.mod/go.sum 没变，这步会使用缓存
COPY go.mod go.sum ./
RUN go mod download

# 复制源代码
COPY . .

# 构建应用
# -ldflags="-w -s" 用于减小二进制文件体积（去掉调试信息）
RUN go build -ldflags="-w -s" -o simple-sfu main.go

# 第二阶段：运行阶段
FROM alpine:latest

# 安装 ca-certificates 以支持 HTTPS 请求（如果需要）
RUN apk --no-cache add ca-certificates

WORKDIR /app

# 从构建阶段复制二进制文件
COPY --from=builder /app/simple-sfu .

# 复制静态资源文件 (代码中强依赖 index.html)
COPY --from=builder /app/index.html .

# 暴露 HTTP 端口
EXPOSE 9090

# 暴露 WebRTC UDP 端口范围 (对应代码中的 udp-min 和 udp-max)
EXPOSE 50000-50050/udp

# 运行应用
# 建议在运行容器时通过参数传入 public-ip，例如:
# docker run -p 9090:9090 -p 50000-50050:50000-50050/udp my-sfu ./simple-sfu -public-ip <YOUR_HOST_IP>
CMD ["./simple-sfu"]
