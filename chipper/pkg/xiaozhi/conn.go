package xiaozhi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/sync/errgroup"
)

type WsConnWrapper interface {
	Ping(data string) error
	Pong(data string) error
	Close() error
	WriteLoop(ctx context.Context)
	ReadLoop(ctx context.Context) error
	WatchIdle(ctx context.Context)
}

type XiaozhiConnWrapper struct {
	ctx         context.Context
	conn        *websocket.Conn
	proxyConn   *websocket.Conn
	done        chan struct{}
	idleTimeout time.Duration
	idleTimer   *time.Timer
	originReq   *http.Request
	baseURL     string
}

type WsConnOption func(*XiaozhiConnWrapper)

func WithIdleTimeout(idleTimeout time.Duration) WsConnOption {
	return func(w *XiaozhiConnWrapper) {
		w.idleTimeout = idleTimeout
		w.idleTimer = time.NewTimer(w.idleTimeout)
	}
}

func WithBaseURL(baseURL string) WsConnOption {
	return func(w *XiaozhiConnWrapper) {
		w.baseURL = baseURL
	}
}

func NewXiaozhiConnWrapper(ctx context.Context, conn *websocket.Conn, r *http.Request, ops ...WsConnOption) (*XiaozhiConnWrapper, error) {
	wsConn := &XiaozhiConnWrapper{
		ctx:         ctx,
		conn:        conn,
		done:        make(chan struct{}),
		idleTimeout: 0,
		originReq:   r,
		baseURL:     GetBaseURL(),
	}
	for _, op := range ops {
		op(wsConn)
	}

	if wsConn.originReq == nil {
		return nil, errors.New("origin request is nil")
	}

	if err := wsConn.ConnectProxy(); err != nil {
		return nil, err
	}

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { wsConn.WriteLoop(ctx); return nil })
	g.Go(func() error { wsConn.WatchIdle(ctx); return nil })

	conn.SetPingHandler(wsConn.Pong)
	return wsConn, nil
}

func (w *XiaozhiConnWrapper) ConnectProxy() error {
	baseUrl := w.originReq.URL.String()
	if !strings.Contains(w.originReq.URL.String(), "wss://") {
		baseUrl = w.baseURL
	}

	headers := http.Header{}
	headers.Add("Authorization", w.originReq.Header.Get("Authorization"))
	headers.Add("Protocol-Version", w.originReq.Header.Get("Protocol-Version"))
	headers.Add("Device-Id", w.originReq.Header.Get("Device-Id"))
	headers.Add("Client-Id", w.originReq.Header.Get("Client-Id"))
	proxyConn, resp, err := websocket.DefaultDialer.Dial(baseUrl, headers)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return errors.New("invalid request")
	}
	w.proxyConn = proxyConn
	return nil
}

func (w *XiaozhiConnWrapper) Ping(data string) error {
	if w.conn == nil {
		return nil
	}
	w.resetIdleTimer()
	return w.conn.WriteControl(websocket.PingMessage, []byte(data), time.Now().Add(time.Second))
}

func (w *XiaozhiConnWrapper) Pong(data string) error {
	if w.conn == nil {
		return nil
	}
	w.resetIdleTimer()
	return w.conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(time.Second))
}

func (w *XiaozhiConnWrapper) Close() error {
	_ = w.conn.Close()
	if w.proxyConn != nil {
		_ = w.proxyConn.Close()
	}
	if w.idleTimer != nil {
		w.idleTimer.Stop()
	}
	return nil
}

func (w *XiaozhiConnWrapper) WriteLoop(ctx context.Context) {
	for {
		if w.proxyConn == nil {
			return
		}
		
		// Use goroutine to make ReadMessage non-blocking with context
		done := make(chan bool, 1)
		var msgType int
		var msg []byte
		var merr error
		
		go func() {
			msgType, msg, merr = w.proxyConn.ReadMessage()
			done <- true
		}()
		
		select {
		case <-ctx.Done():
			return
		case <-w.done:
			return
		case <-done:
			if merr != nil {
				select {
				case w.done <- struct{}{}:
				default:
				}
				return
			}

			if w.conn == nil {
				return
			}

			var err error
			switch msgType {
			case websocket.TextMessage:
				err = w.conn.WriteMessage(websocket.TextMessage, msg)
			case websocket.BinaryMessage:
				err = w.conn.WriteMessage(websocket.BinaryMessage, msg)
			}
			if err != nil {
				return
			}
		}
	}
}

func (w *XiaozhiConnWrapper) ReadLoop(ctx context.Context) (err error) {
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

			if w.proxyConn == nil {
				continue
			}

			switch msgType {
			case websocket.TextMessage:
				err = w.proxyConn.WriteMessage(websocket.TextMessage, msg)
			case websocket.BinaryMessage:
				err = w.proxyConn.WriteMessage(websocket.BinaryMessage, msg)
			}
			if err != nil {
				return err
			}
			w.resetIdleTimer()
		}
	}
}

func (w *XiaozhiConnWrapper) WatchIdle(ctx context.Context) {
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

func (w *XiaozhiConnWrapper) resetIdleTimer() {
	if w.idleTimeout == 0 {
		return
	}
	if w.idleTimer != nil {
		w.idleTimer.Reset(w.idleTimeout)
	}
}
