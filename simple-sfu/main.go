package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

// 全局变量区
// 在这个简单的 SFU (Selective Forwarding Unit) Demo 中，我们使用最简化的模型：
// 1. 假设只有一个推流者 (Publisher)
// 2. 所有的拉流者 (Subscriber) 都观看这个推流者的画面
var (
	localTrack  *webrtc.TrackLocalStaticRTP
	publisherPC *webrtc.PeerConnection
	mu          sync.RWMutex

	// 命令行参数
	addr        = flag.String("addr", ":9090", "http service address")
	certFile    = flag.String("cert", "", "tls certificate file")
	keyFile     = flag.String("key", "", "tls key file")
	publicIP    = flag.String("public-ip", "", "public IP address (to avoid STUN)")
	udpMin      = flag.Int("udp-min", 50000, "min UDP port")
	udpMax      = flag.Int("udp-max", 50050, "max UDP port")
	turnAddr    = flag.String("turn", "", "TURN server address for Client (e.g. 106.14.31.105:3478)")
	turnAddrInt = flag.String("turn-internal", "", "TURN server address for Server (e.g. 127.0.0.1:3478)")
	turnUser    = flag.String("turn-user", "", "TURN username (for static auth)")
	turnPwd     = flag.String("turn-pwd", "", "TURN password (for static auth)")
	turnSecret  = flag.String("turn-secret", "", "TURN shared secret (for dynamic auth)")
)

func main() {
	flag.Parse()

	// 0. 检查 index.html 是否存在
	if _, err := os.Stat("index.html"); os.IsNotExist(err) {
		fmt.Printf("【严重警告】当前目录 (%s) 下找不到 index.html！\n", func() string { s, _ := os.Getwd(); return s }())
		fmt.Println("请务必在包含 index.html 的目录下运行程序，否则网页无法访问。")
		fmt.Println("正确做法示例: cd /path/to/app && ./simple-sfu-linux")
	}

	// 1. 初始化全局 Track (视频轨道)
	// 我们指定使用 VP8 编码格式，这是 WebRTC 中兼容性最好的视频编码之一。
	// "video" 是 StreamID (流ID), "pion" 是 TrackID (轨道ID)。
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8},
		"video", "pion",
	)
	if err != nil {
		panic(err)
	}
	localTrack = track

	// 2. 设置 HTTP 路由
	// "/" 			-> 返回 index.html 前端页面
	// "/publish" 	-> 处理推流请求 (Websocket/HTTP-Post 用于交换 SDP)
	// "/subscribe" -> 处理拉流请求
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html")
	})
	http.HandleFunc("/publish", handlePublish)
	http.HandleFunc("/subscribe", handleSubscribe)

	// 3. 启动服务
	if *certFile != "" && *keyFile != "" {
		fmt.Printf("WebRTC SFU Server (HTTPS) started at https://localhost%s\n", *addr)
		if err := http.ListenAndServeTLS(*addr, *certFile, *keyFile, nil); err != nil {
			panic(err)
		}
	} else {
		fmt.Printf("WebRTC SFU Server (HTTP) started at http://localhost%s\n", *addr)
		if err := http.ListenAndServe(*addr, nil); err != nil {
			panic(err)
		}
	}
}

// newPeerConnection 创建一个新的 PC，并根据情况配置 IP
func newPeerConnection() (*webrtc.PeerConnection, error) {
	config := webrtc.Configuration{}

	// 1. 创建 MediaEngine 并注册默认 Codec
	// 当我们自定义 API 时，必须手动注册 Codec，否则会报 "no codecs" 错误
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}

	// 2. 创建 Interceptor (拦截器，用于 RTCP 处理等)
	i := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		return nil, err
	}

	// 3. 配置 SettingEngine
	settingEngine := webrtc.SettingEngine{}

	// 限制 UDP 端口范围 (如果不想限制，可以不传参数)
	if *udpMin > 0 && *udpMax > 0 {
		settingEngine.SetEphemeralUDPPortRange(uint16(*udpMin), uint16(*udpMax))
	}

	// 4. 配置 ICE Servers (STUN/TURN)
	if *turnAddr != "" {
		// ... (TURN 逻辑保持不变，但我们这次不用它)
	}

	// [重要修复] 如果指定了 public-ip，我们应该忽略 STUN/TURN，强制使用 NAT 1:1
	// 这在阿里云/AWS 等 1:1 NAT 环境下是最稳的。
	if *publicIP != "" {
		// 清空 ICEServers，防止 Pion 去请求 STUN/TURN，导致 Candidate 混乱
		config.ICEServers = []webrtc.ICEServer{}

		// 强制只使用 Host 类型的 Candidate (即直接用 IP)
		settingEngine.SetNAT1To1IPs([]string{*publicIP}, webrtc.ICECandidateTypeHost)
	} else if *turnAddr == "" {
		// 既没 TURN 也没 PublicIP，才用 Google STUN
		config.ICEServers = []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		}
	}

	// 5. 创建 API 对象
	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(m),
		webrtc.WithInterceptorRegistry(i),
		webrtc.WithSettingEngine(settingEngine),
	)

	return api.NewPeerConnection(config)
}

// handlePublish 处理推流请求 (浏览器 -> 服务端)
// 这里的核心逻辑是：接收浏览器的视频流，并写入到全局的 localTrack 中。
func handlePublish(w http.ResponseWriter, r *http.Request) {
	// 1. 创建 PeerConnection (PC)
	peerConnection, err := newPeerConnection()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 2. 保存 Publisher 的 PC，以便后续发送 PLI 请求
	mu.Lock()
	publisherPC = peerConnection
	mu.Unlock()

	// 3. 监听 "OnTrack" 事件
	// 当浏览器成功推流并发送数据过来时，会触发这个回调。
	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		fmt.Printf("收到新的 Track: %s (Type: %d)\n", track.Codec().MimeType, track.PayloadType())

		// 本 Demo 只处理 VP8 视频流，忽略其他类型（如音频或 VP9/H264）
		if track.Codec().MimeType != webrtc.MimeTypeVP8 {
			fmt.Println("本 Demo 只支持 VP8 视频")
			return
		}

		// [重要] 启动一个协程，定期发送 PLI (Picture Loss Indication)
		// 这是一个简单的“保底”策略：每 3 秒请求一次关键帧。
		// 这样即使拉流端错过了之前的关键帧（导致花屏或黑屏），最长 3 秒后也能恢复。
		go func() {
			ticker := time.NewTicker(time.Second * 3)
			for range ticker.C {
				if rtcpErr := peerConnection.WriteRTCP([]rtcp.Packet{
					&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())},
				}); rtcpErr != nil {
					// 忽略发送错误（可能是连接已断开）
				}
			}
		}()

		// 4. 数据转发循环
		// 从远程 Track 读取 RTP 包，写入本地 localTrack
		buf := make([]byte, 1500) // RTP 包通常小于 1500 字节 (MTU 限制)
		for {
			n, _, readErr := track.Read(buf)
			if readErr != nil {
				if readErr != io.EOF {
					fmt.Println("Read Error:", readErr)
				}
				return // 退出循环，结束处理
			}

			// 将读取到的数据写入全局 Track
			// 这一步完成了“从 Publisher 接收 -> 存入转发器”的过程
			if _, writeErr := localTrack.Write(buf[:n]); writeErr != nil && writeErr != io.ErrClosedPipe {
				fmt.Println("Write Error:", writeErr)
				return
			}
		}
	})

	// 监听 ICE 连接状态变化，用于调试
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("Publisher ICE State: %s\n", connectionState.String())
	})

	// 处理信令交换 (SDP Offer/Answer)
	doSignaling(w, r, peerConnection)
}

// handleSubscribe 处理拉流请求 (服务端 -> 浏览器)
// 这里的核心逻辑是：创建一个新的 PC，把全局的 localTrack 添加进去，发送给浏览器。
func handleSubscribe(w http.ResponseWriter, r *http.Request) {
	peerConnection, err := newPeerConnection()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 1. 添加 Track 到 PeerConnection
	// 这样浏览器就能从这个 PC 接收到 localTrack 中的数据
	rtpSender, err := peerConnection.AddTrack(localTrack)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 2. 读取 RTCP 包 (必须做)
	// 即使我们只是发送数据，也必须读取对方发来的 RTCP 包（如 NACK, RR）。
	// 如果不读取，底层的 UDP 缓冲区会填满，导致连接断开或卡死。
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}()

	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("Subscriber ICE State: %s\n", connectionState.String())
		// 3. 连接成功后，立即请求关键帧
		// 这是一个优化：当新用户加入时，不要干等 3 秒的定时器，而是立刻请求最新的画面。
		if connectionState == webrtc.ICEConnectionStateConnected {
			// 启动一个协程，连续请求几次关键帧，确保万无一失
			go func() {
				for i := 0; i < 5; i++ {
					requestKeyFrame()
					time.Sleep(time.Millisecond * 500)
				}
			}()
		}
	})

	doSignaling(w, r, peerConnection)
}

// requestKeyFrame 向 Publisher 请求关键帧 (PLI)
// 关键帧 (KeyFrame/I-Frame) 是一张完整的图片。如果丢失关键帧，后续的 P-Frame (增量帧) 无法解码，会导致花屏。
func requestKeyFrame() {
	mu.RLock()
	defer mu.RUnlock()

	if publisherPC == nil {
		return
	}

	// 遍历 Publisher 的所有接收器，向它们发送 PLI
	for _, receiver := range publisherPC.GetReceivers() {
		if receiver.Track() == nil {
			continue
		}

		_ = publisherPC.WriteRTCP([]rtcp.Packet{
			&rtcp.PictureLossIndication{MediaSSRC: uint32(receiver.Track().SSRC())},
		})
		fmt.Println("发送了 PLI 请求关键帧")
	}
}

// doSignaling 完成 SDP 的交换 (Offer/Answer 模式)
// 这是 WebRTC 建立连接的标准流程：
// 1. 客户端发送 Offer (包含它支持的编码、加密算法等)
// 2. 服务端设置 RemoteDescription
// 3. 服务端创建 Answer (包含服务端选定的参数)
// 4. 服务端设置 LocalDescription
// 5. 交换 ICE Candidates (这里简化为等待所有 Candidates 收集完再一次性返回)
func doSignaling(w http.ResponseWriter, r *http.Request, pc *webrtc.PeerConnection) {
	// 1. 读取客户端发来的 Offer JSON
	var offer webrtc.SessionDescription
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 2. 设置远端描述
	if err := pc.SetRemoteDescription(offer); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 3. 创建 Answer
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 4. 设置本地描述
	// 这会触发 ICE Agent 开始收集 Candidates
	if err := pc.SetLocalDescription(answer); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 5. 等待 ICE 收集完成
	// 这是一个阻塞操作，直到所有 Candidates 都收集完毕。
	// 在生产环境中，为了更快的连接速度，通常使用 Trickle ICE (分批发送)，而不是阻塞等待。
	<-webrtc.GatheringCompletePromise(pc)

	// 6. 返回包含完整 ICE Candidates 的 Answer
	w.Header().Set("Content-Type", "application/json")

	// [Hack] 替换 Answer 中的 TURN 地址
	// 因为我们在服务端使用了内网 TURN 地址 (127.0.0.1)，生成的 SDP 里也会包含这个地址。
	// 发给浏览器前，必须把它替换成公网地址 (106...)，否则浏览器连不上。
	answer = *pc.LocalDescription()
	if *turnAddrInt != "" && *turnAddr != "" {
		// 提取 IP 部分
		internalIP := strings.Split(*turnAddrInt, ":")[0]
		externalIP := strings.Split(*turnAddr, ":")[0]

		// 简单替换 SDP 中的 IP
		// 注意：这可能会误伤其他部分，但在受控环境下通常没问题
		answer.SDP = strings.ReplaceAll(answer.SDP, internalIP, externalIP)
	}

	json.NewEncoder(w).Encode(answer)
}
