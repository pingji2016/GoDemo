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

// localTrack 是我们全局唯一的视频轨道
// 在这个简单的 Demo 中，我们只支持一个发布者，所有订阅者都看这个轨道
var (
	localTrack  *webrtc.TrackLocalStaticRTP
	publisherPC *webrtc.PeerConnection
	mu          sync.RWMutex
)

func main() {
	// 1. 初始化全局 Track
	// 使用 VP8 编码，这是浏览器最常用的视频编码之一
	// "video" 是 StreamID, "pion" 是 TrackID
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8},
		"video", "pion",
	)
	if err != nil {
		panic(err)
	}
	localTrack = track

	// 2. 设置 HTTP 路由
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html")
	})
	http.HandleFunc("/publish", handlePublish)     // 推流接口
	http.HandleFunc("/subscribe", handleSubscribe) // 拉流接口

	fmt.Println("WebRTC SFU Server started at http://localhost:8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		panic(err)
	}
}

// handlePublish 处理推流请求 (浏览器 -> 服务端)
func handlePublish(w http.ResponseWriter, r *http.Request) {
	// 创建 PeerConnection
	// 这里使用默认配置，实际上你应该配置 STUN/TURN 服务器
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	mu.Lock()
	publisherPC = peerConnection
	mu.Unlock()

	// 处理客户端发送过来的 Track
	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		fmt.Printf("收到新的 Track: %s (Type: %d)\n", track.Codec().MimeType, track.PayloadType())

		// 只处理 VP8
		if track.Codec().MimeType != webrtc.MimeTypeVP8 {
			fmt.Println("本 Demo 只支持 VP8 视频")
			return
		}

		// 启动一个循环，专门读取 Publisher 的 RTCP 包 (例如 Receiver Reports)
		go func() {
			ticker := time.NewTicker(time.Second * 3)
			for range ticker.C {
				if rtcpErr := peerConnection.WriteRTCP([]rtcp.Packet{
					&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())},
				}); rtcpErr != nil {
					// fmt.Println(rtcpErr)
				}
			}
		}()

		// 循环读取远端发送的 RTP 包，并写入本地 Track
		// 这样本地 Track 就有了数据，可以转发给订阅者
		buf := make([]byte, 1500)
		for {
			n, _, readErr := track.Read(buf)
			if readErr != nil {
				if readErr != io.EOF {
					fmt.Println("Read Error:", readErr)
				}
				return
			}

			// 将数据写入全局 Track
			// 注意：这里没有处理并发写入的问题（假设只有一个 Publisher）
			// 也没有处理关键帧请求（PLI），实际场景需要转发 RTCP PLI 给 Publisher
			if _, writeErr := localTrack.Write(buf[:n]); writeErr != nil && writeErr != io.ErrClosedPipe {
				fmt.Println("Write Error:", writeErr)
				return
			}
		}
	})

	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("Publisher ICE State: %s\n", connectionState.String())
	})

	// 处理信令交换 (SDP)
	doSignaling(w, r, peerConnection)
}

// handleSubscribe 处理拉流请求 (服务端 -> 浏览器)
func handleSubscribe(w http.ResponseWriter, r *http.Request) {
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 将全局 Track 添加到 PeerConnection 中，发送给订阅者
	rtpSender, err := peerConnection.AddTrack(localTrack)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 必须读取 RTCP 包，否则接收缓冲区会堆积导致卡死
	// 这里我们通过读取并丢弃来保持连接活跃
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
		if connectionState == webrtc.ICEConnectionStateConnected {
			// 连接成功后，立即请求关键帧
			requestKeyFrame()
		}
	})

	doSignaling(w, r, peerConnection)
}

// requestKeyFrame 向 Publisher 请求关键帧 (PLI)
func requestKeyFrame() {
	mu.RLock()
	defer mu.RUnlock()

	if publisherPC == nil {
		return
	}

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
func doSignaling(w http.ResponseWriter, r *http.Request, pc *webrtc.PeerConnection) {
	// 1. 读取客户端发来的 Offer
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
	// 这会触发 ICE 收集
	if err := pc.SetLocalDescription(answer); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 5. 等待 ICE 收集完成
	// 这是一个阻塞操作，直到所有 Candidates 都收集完毕
	// 在生产环境中，建议使用 Trickle ICE (分批发送 Candidates) 以加快连接速度
	<-webrtc.GatheringCompletePromise(pc)

	// 6. 返回包含完整 ICE Candidates 的 Answer
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(pc.LocalDescription())
}

// 简单的辅助函数，模拟关键帧请求（实际未用到，保留作参考）
func sendPLI(peerConnection *webrtc.PeerConnection) {
	// 实际需要找到对应的 RTCP Sender 并发送 PLI 包
}
