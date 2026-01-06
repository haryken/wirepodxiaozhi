package xiaozhi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/sync/errgroup"
)

type OpenAIConnWrapper struct {
	ctx         context.Context
	conn        *websocket.Conn
	apiConn     *websocket.Conn
	done        chan struct{}
	idleTimeout time.Duration
	idleTimer   *time.Timer
	originReq   *http.Request
}

func NewOpenAIConnWrapper(ctx context.Context, conn *websocket.Conn, r *http.Request, ops ...WsConnOption) (*OpenAIConnWrapper, error) {
	baseURL, apiKey, model, _ := GetOpenAIConfig()
	if apiKey == "" {
		return nil, errors.New("OPENAI_API_KEY is required for OpenAI provider")
	}

	wsConn := &OpenAIConnWrapper{
		ctx:         ctx,
		conn:        conn,
		done:        make(chan struct{}),
		idleTimeout: 0,
		originReq:   r,
	}
	// OpenAI wrapper doesn't support options, ignore them
	_ = ops

	// Connect to OpenAI Realtime API
	if err := wsConn.ConnectToOpenAI(baseURL, apiKey, model); err != nil {
		return nil, fmt.Errorf("failed to connect to OpenAI: %w", err)
	}

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { wsConn.WriteLoop(ctx); return nil })
	g.Go(func() error { wsConn.WatchIdle(ctx); return nil })

	conn.SetPingHandler(wsConn.Pong)
	return wsConn, nil
}

func (w *OpenAIConnWrapper) ConnectToOpenAI(baseURL, apiKey, model string) error {
	headers := http.Header{}
	headers.Add("Authorization", "Bearer "+apiKey)
	wsURL := baseURL + "?model=" + model
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		return err
	}
	w.apiConn = conn
	
	// Start reading from OpenAI API in background
	go func() {
		for {
			if w.apiConn == nil {
				return
			}
			msgType, message, err := w.apiConn.ReadMessage()
			if err != nil {
				select {
				case w.done <- struct{}{}:
				default:
				}
				return
			}
			// Forward to client
			if w.conn != nil {
				_ = w.conn.WriteMessage(msgType, message)
			}
		}
	}()
	
	return nil
}

func (w *OpenAIConnWrapper) Ping(data string) error {
	if w.conn == nil {
		return nil
	}
	w.resetIdleTimer()
	return w.conn.WriteControl(websocket.PingMessage, []byte(data), time.Now().Add(time.Second))
}

func (w *OpenAIConnWrapper) Pong(data string) error {
	if w.conn == nil {
		return nil
	}
	w.resetIdleTimer()
	return w.conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(time.Second))
}

func (w *OpenAIConnWrapper) Close() error {
	_ = w.conn.Close()
	if w.apiConn != nil {
		_ = w.apiConn.Close()
	}
	if w.idleTimer != nil {
		w.idleTimer.Stop()
	}
	return nil
}

func (w *OpenAIConnWrapper) WriteLoop(ctx context.Context) {
	// The actual message forwarding from OpenAI API to client
	// is handled in the goroutine started in ConnectToOpenAI
	// This function just waits for context cancellation
	select {
	case <-ctx.Done():
		return
	case <-w.done:
		return
	}
}

func (w *OpenAIConnWrapper) ReadLoop(ctx context.Context) (err error) {
	defer func() {
		_ = w.Close()
	}()

	for {
		if w.conn == nil {
			return nil
		}
		
		// Use goroutine to make ReadMessage non-blocking with context
		done := make(chan bool, 1)
		var msgType int
		var msg []byte
		var merr error
		
		go func() {
			msgType, msg, merr = w.conn.ReadMessage()
			done <- true
		}()
		
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.done:
			return nil
		case <-done:
			if merr != nil {
				select {
				case w.done <- struct{}{}:
				default:
				}
				return merr
			}

			if w.apiConn == nil {
				continue
			}

			// Forward client message to OpenAI API
			switch msgType {
			case websocket.TextMessage:
				err = w.apiConn.WriteMessage(websocket.TextMessage, msg)
			case websocket.BinaryMessage:
				err = w.apiConn.WriteMessage(websocket.BinaryMessage, msg)
			}
			if err != nil {
				return err
			}
			w.resetIdleTimer()
		}
	}
}

func (w *OpenAIConnWrapper) WatchIdle(ctx context.Context) {
	if w.idleTimeout == 0 {
		return
	}
	if w.idleTimer == nil {
		return
	}

	select {
	case <-w.idleTimer.C:
		_ = w.conn.Close()
	case <-ctx.Done():
		return
	}
}

func (w *OpenAIConnWrapper) resetIdleTimer() {
	if w.idleTimeout == 0 {
		return
	}
	if w.idleTimer != nil {
		w.idleTimer.Reset(w.idleTimeout)
	}
}
