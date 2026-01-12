package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

// 全局变量区
// 在这个简单的 SFU (Selective Forwarding Unit) Demo 中，我们使用最简化的模型：
// 1. 假设只有一个推流者 (Publisher)
// 2. 所有的拉流者 (Subscriber) 都观看这个推流者的画面
var (
	// localTrack 是一个 RTP 数据包的“中转站”。
	// Publisher 的数据写入这里，Subscriber 从这里读取数据。
	localTrack *webrtc.TrackLocalStaticRTP

	// publisherPC 保存推流者的 PeerConnection 连接对象。
	// 我们需要它来向推流者发送 RTCP 控制包（比如请求关键帧 PLI）。
	publisherPC *webrtc.PeerConnection

	// mu 是一个读写锁，保证多线程访问 publisherPC 时的安全。
	mu sync.RWMutex
)

func main() {
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

	fmt.Println("WebRTC SFU Server started at http://localhost:8080")
	// 3. 启动 HTTP 服务，监听 8080 端口
	if err := http.ListenAndServe(":8080", nil); err != nil {
		panic(err)
	}
}

// handlePublish 处理推流请求 (浏览器 -> 服务端)
// 这里的核心逻辑是：接收浏览器的视频流，并写入到全局的 localTrack 中。
func handlePublish(w http.ResponseWriter, r *http.Request) {
	// 1. 创建 PeerConnection (PC)
	// PC 是 WebRTC 的核心对象，代表一条 P2P 连接。
	// 这里使用默认配置，在生产环境中，你通常需要配置 STUN/TURN 服务器以穿透 NAT。
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
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
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
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
			requestKeyFrame()
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
	json.NewEncoder(w).Encode(pc.LocalDescription())
}
