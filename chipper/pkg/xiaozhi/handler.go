package xiaozhi

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kercre123/wire-pod/chipper/pkg/logger"
)

type WebSocketServer struct {
	requestCounter atomic.Int64
}

func NewWebSocketServer() *WebSocketServer {
	return &WebSocketServer{}
}

func (s *WebSocketServer) wsConnect(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	upgrader := &websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
	conn, err := upgrader.Upgrade(w, r, w.Header())
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (s *WebSocketServer) RealTime(w http.ResponseWriter, r *http.Request) {
	var err error
	
	// Log thông tin device khi kết nối và lưu MAC address
	deviceID := r.Header.Get("Device-Id")
	clientID := r.Header.Get("Client-Id")
	
	// Nếu PC kết nối mà không có Device-Id, tạo một Device-Id dựa trên IP address
	if deviceID == "" {
		// Lấy IP address của client
		ip := r.RemoteAddr
		// Xử lý format "IP:port" hoặc chỉ IP
		if idx := len(ip); idx > 0 {
			// Tạo Device-Id từ IP (cho PC kết nối như ESP32)
			// Format: "pc_<hash_of_ip>" để dễ nhận biết là PC
			deviceID = fmt.Sprintf("pc_%s", ip)
			logger.Println(fmt.Sprintf("[Xiaozhi] PC connecting without Device-Id header - Auto-generated Device-Id: %s (from IP: %s)", deviceID, ip))
		} else {
			deviceID = "pc_unknown"
			logger.Println("[Xiaozhi] PC connecting without Device-Id header - Using default: pc_unknown")
		}
	} else {
		logger.Println(fmt.Sprintf("[Xiaozhi] Device connecting - Device-Id (MAC): %s, Client-Id: %s", deviceID, clientID))
	}
	
	// Lưu MAC address/Device-Id khi device kết nối (bao gồm cả PC)
	RegisterConnectedDevice(deviceID, clientID)
	
		// Kiểm tra xem device đã được activate chưa (trong wirepodxiaozhi)
		if IsDeviceActivated(deviceID) {
			device, _ := GetActivatedDevice(deviceID)
			logger.Println(fmt.Sprintf("[Xiaozhi] Device %s already activated at %s", deviceID, device.ActivatedAt.Format(time.RFC3339)))
			UpdateDeviceLastSeen(deviceID)
		} else {
			// Nếu device kết nối thành công đến upstream server, có nghĩa là đã được activate ở upstream
			// Tự động lưu vào local storage để lần sau check nhanh hơn
			// (Upstream server sẽ từ chối nếu device chưa được activate)
			logger.Println(fmt.Sprintf("[Xiaozhi] Device %s connecting - assuming activated at upstream server, registering locally", deviceID))
			// Lấy Client-Id từ request header hoặc config
			clientID := r.Header.Get("Client-Id")
			if clientID == "" {
				clientID = GetClientIDFromConfig()
			}
			RegisterActivatedDevice(deviceID, clientID, "token_from_upstream_connection")
		}
	
	conn, err := s.wsConnect(w, r)
	if err != nil {
		logger.Println("Error upgrading connection:", err)
		http.Error(w, "Error upgrading connection", http.StatusInternalServerError)
		return
	}

	defer func() {
		// Xóa device khỏi danh sách connected khi disconnect
		if deviceID != "" {
			RemoveConnectedDevice(deviceID)
			logger.Println(fmt.Sprintf("[Xiaozhi] Device %s disconnected", deviceID))
		}
		_ = conn.Close()
		conn = nil
	}()

	ctx := r.Context()
	connWrapper, err := s.NewConnWrapper(ctx, conn, r)
	if err != nil {
		logger.Println("Error creating connection wrapper:", err)
		return
	}

	// Cập nhật last seen trong một goroutine riêng
	if deviceID != "" {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					UpdateConnectedDeviceLastSeen(deviceID)
				}
			}
		}()
	}

	_ = connWrapper.ReadLoop(ctx)
}

func (s *WebSocketServer) NewConnWrapper(
	ctx context.Context, conn *websocket.Conn, r *http.Request) (WsConnWrapper, error) {
	// Switch between xiaozhi and openai based on config
	provider := GetProvider()
	if provider == ProviderOpenAI {
		return NewOpenAIConnWrapper(ctx, conn, r)
	}
	// Default to xiaozhi
	return NewXiaozhiConnWrapper(ctx, conn, r)
}
