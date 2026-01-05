package xiaozhi

import (
	"context"
	"log"
	"net/http"
	"sync/atomic"

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
	conn, err := s.wsConnect(w, r)
	if err != nil {
		logger.Println("Error upgrading connection:", err)
		http.Error(w, "Error upgrading connection", http.StatusInternalServerError)
		return
	}

	defer func() {
		_ = conn.Close()
		conn = nil
	}()

	ctx := r.Context()
	connWrapper, err := s.NewConnWrapper(ctx, conn, r)
	if err != nil {
		logger.Println("Error creating connection wrapper:", err)
		return
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
