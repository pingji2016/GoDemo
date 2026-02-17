# Simple Go WebRTC SFU

这是一个极简的 WebRTC SFU (Selective Forwarding Unit) 示例，使用 Go 语言和 [pion/webrtc](https://github.com/pion/webrtc) 库实现。

它演示了 SFU 的核心原理：**接收一个推流 (Publisher)，并将媒体数据转发给多个拉流者 (Subscriber)。**

## 功能特性
- **一对多直播**：支持一个发布者，无限个订阅者。
- **屏幕共享**：默认推流源为屏幕共享（可改回摄像头）。
- **VP8 编码**：使用通用性最好的 VP8 视频编码。
- **关键帧请求 (PLI)**：实现了自动 PLI 请求，解决拉流黑屏/花屏问题。

## 1. 编译与运行 (本地开发)

### 前置要求
- 安装 [Go 1.20+](https://go.dev/dl/) 环境。

### 运行步骤
go env -w GOPROXY=https://goproxy.cn,direct
1. **下载依赖**
   ```bash
   go mod tidy
   ```

2. **直接运行源码**
   ```bash
   go run main.go
   ```

3. **访问测试**
   - 打开浏览器访问: `http://localhost:8080`
   - 点击 **[开始推流]** (选择屏幕或窗口)
   - 打开新标签页，访问同一地址，点击 **[开始拉流]**

## 2. 编译为可执行文件 (部署模式)

如果你想把程序部署到服务器，建议编译为二进制文件。

### Windows
```powershell
# 编译生成 simple-sfu.exe
go build -o simple-sfu.exe main.go

# 运行
.\simple-sfu.exe
```

### Linux (交叉编译)
如果你在 Windows 上开发，但要部署到 Linux 服务器：

```powershell
# 设置环境变量进行交叉编译
$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -o simple-sfu-linux main.go

# 将 simple-sfu-linux 和 index.html 上传到服务器后：
# chmod +x simple-sfu-linux
# ./simple-sfu-linux
```

### macOS
```bash
GOOS=darwin GOARCH=arm64 go build -o simple-sfu-mac main.go
```

## 3. 部署注意事项

1. **端口开放**：
   - **TCP 8080**: HTTP 信令服务。
   - **UDP 10000-60000** (默认范围): WebRTC 媒体传输需要开放大量 UDP 端口。
     - *注意：公网部署时，防火墙必须放行这些 UDP 端口，否则无法建立连接。*

2. **公网访问 (NAT 穿透)**：
   - 如果部署在云服务器，需配置 STUN/TURN 服务器。
   - 在 `main.go` 的 `webrtc.Configuration{}` 中添加 `ICEServers` 配置。

3. **HTTPS**:
   - 浏览器要求 WebRTC 必须在 **HTTPS** (或 localhost) 下才能获取摄像头/屏幕权限。
   - 生产环境建议使用 Nginx 反向代理 HTTPS，或者在 Go 代码中配置 TLS 证书。
