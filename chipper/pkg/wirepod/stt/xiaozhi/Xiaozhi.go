package wirepod_vosk

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kercre123/wire-pod/chipper/pkg/logger"
	"github.com/kercre123/wire-pod/chipper/pkg/vars"
	sr "github.com/kercre123/wire-pod/chipper/pkg/wirepod/speechrequest"
	"github.com/kercre123/wire-pod/chipper/pkg/xiaozhi"
	"gopkg.in/hraban/opus.v2"
)

var Name string = "xiaozhi"

// STTHandler implements MessageHandler interface for STT
// This handler processes STT-related messages from the single reader goroutine
type STTHandler struct {
	transcriptChan     chan string
	errChan            chan error
	errorOccurred      chan struct{}
	active             bool
	transcriptReceived bool
	mu                 sync.RWMutex
}

// HandleMessage processes messages from the WebSocket connection
func (h *STTHandler) HandleMessage(messageType int, message []byte) error {
	h.mu.RLock()
	active := h.active
	h.mu.RUnlock()

	if !active {
		return nil // Handler is not active, ignore message
	}

	if messageType == websocket.TextMessage {
		var event map[string]interface{}
		if err := json.Unmarshal(message, &event); err != nil {
			logger.Println(fmt.Sprintf("Xiaozhi STT Handler: ERROR - Failed to unmarshal message: %v", err))
			return err
		}

		eventType, ok := event["type"].(string)
		if !ok {
			return nil
		}

		switch eventType {
		case "stt":
			if text, ok := event["text"].(string); ok {
				if text != "" {
					// Non-empty transcript
					logger.Println(fmt.Sprintf("Xiaozhi STT Handler: ✅ STT transcript: '%s'", text))
					// Use recover to handle panic if channel is closed
					func() {
						defer func() {
							if r := recover(); r != nil {
								// Channel is closed - this can happen if STT request completed but ConnectionManager
								// still receives messages from the server
								logger.Println(fmt.Sprintf("Xiaozhi STT Handler: ⚠️  transcriptChan is closed, dropping transcript (recovered from panic: %v)", r))
							}
						}()
						select {
						case h.transcriptChan <- text:
							logger.Println(fmt.Sprintf("Xiaozhi STT Handler: ✅ Transcript sent to channel: '%s'", text))
							h.mu.Lock()
							h.transcriptReceived = true
							h.mu.Unlock()
						default:
							logger.Println(fmt.Sprintf("Xiaozhi STT Handler: ⚠️  transcriptChan is full, dropping transcript"))
						}
					}()
				} else {
					// Empty transcript from server - send to channel immediately to avoid timeout
					// Empty transcript có thể xảy ra khi:
					// 1. Audio không đủ rõ để transcribe
					// 2. Server không nhận đủ audio data
					// 3. Audio quá ngắn hoặc chỉ có noise
					logger.Println(fmt.Sprintf("Xiaozhi STT Handler: ⚠️  Received empty transcript from server (stt event with empty text) - server may not have received enough audio or audio was unclear"))
					func() {
						defer func() {
							if r := recover(); r != nil {
								logger.Println(fmt.Sprintf("Xiaozhi STT Handler: ⚠️  transcriptChan is closed, dropping empty transcript (recovered from panic: %v)", r))
							}
						}()
						select {
						case h.transcriptChan <- "":
							logger.Println(fmt.Sprintf("Xiaozhi STT Handler: ✅ Empty transcript sent to channel (server returned empty text) - handler will remain active for potential TTS messages"))
							h.mu.Lock()
							h.transcriptReceived = true
							h.mu.Unlock()
						default:
							logger.Println(fmt.Sprintf("Xiaozhi STT Handler: ⚠️  transcriptChan is full, dropping empty transcript"))
						}
					}()
				}
			} else {
				// Server sent "stt" event but no "text" field
				logger.Println(fmt.Sprintf("Xiaozhi STT Handler: ⚠️  Received 'stt' event but no 'text' field in event: %v", event))
			}
		case "error":
			// Use recover to handle panic if channels are closed
			func() {
				defer func() {
					if r := recover(); r != nil {
						logger.Println(fmt.Sprintf("Xiaozhi STT Handler: ⚠️  error channels are closed, dropping error (recovered from panic: %v)", r))
					}
				}()
				select {
				case h.errorOccurred <- struct{}{}:
				default:
				}
				errorMsg := "unknown error"
				if msg, ok := event["error"].(string); ok {
					errorMsg = msg
				} else if msg, ok := event["message"].(string); ok {
					errorMsg = msg
				}
				select {
				case h.errChan <- fmt.Errorf("xiaozhi error: %s", errorMsg):
				default:
				}
			}()
		}
	}
	return nil
}

// IsActive returns whether the handler is currently active
func (h *STTHandler) IsActive() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.active && !h.transcriptReceived
}

// SetActive sets the handler as active or inactive
func (h *STTHandler) SetActive(active bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.active = active
	if !active {
		h.transcriptReceived = false
	}
}

// XiaozhiSTT handles STT via xiaozhi WebSocket service
// This follows the xiaozhi protocol as defined in go-xiaozhi-main
func Init() error {
	// Check if xiaozhi is configured in Knowledge Graph
	if vars.APIConfig.Knowledge.Provider != "xiaozhi" {
		logger.Println("Xiaozhi STT: Knowledge Graph provider is not set to xiaozhi")
		return fmt.Errorf("xiaozhi not configured as knowledge provider")
	}
	logger.Println("Xiaozhi STT initialized!")
	return nil
}

func STT(sreq sr.SpeechRequest) (string, error) {
	// Helper function to safely log messages (with recover to prevent logger panics)
	safeLog := func(format string, args ...interface{}) {
		defer func() {
			if r := recover(); r != nil {
				// Log to stderr if logger panics
				fmt.Fprintf(os.Stderr, "[Xiaozhi STT] [logger panic recovered: %v] ", r)
				fmt.Fprintf(os.Stderr, format+"\n", args...)
			}
		}()
		logger.Println(fmt.Sprintf(format, args...))
	}

	safeLog("(Bot %s, Xiaozhi) Processing...", sreq.Device)

	// Get xiaozhi config
	baseURL, _, _ := xiaozhi.GetKnowledgeGraphConfig()
	if baseURL == "" {
		baseURL = "wss://api.tenclass.net/xiaozhi/v1/"
	}

	// Connect to xiaozhi WebSocket (using xiaozhi protocol)
	// Increased timeout to 90 seconds to allow longer speech input
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second) // Increased to 120s for long speech
	defer cancel()

	// Lấy Device-Id và Client-Id từ config
	deviceID := xiaozhi.GetDeviceIDFromConfig()
	// Client-Id: luôn gửi (giống ESP32 - line 109: websocket_->SetHeader("Client-Id", Board::GetInstance().GetUuid().c_str()))
	// ESP32 luôn gửi Client-Id, không optional
	clientID := xiaozhi.GetClientIDFromConfig()

	headers := http.Header{}

	// Gửi các headers giống ESP32 (theo xiaozhi-esp32-main/main/protocols/websocket_protocol.cc)
	// Protocol-Version: version của protocol (mặc định 1)
	headers.Add("Protocol-Version", "1")

	if deviceID != "" {
		headers.Add("Device-Id", deviceID)
		logger.Println(fmt.Sprintf("Xiaozhi STT: Using Device-Id from config: %s", deviceID))

		// Kiểm tra activation status từ server (không dùng local cache)
		logger.Println(fmt.Sprintf("Xiaozhi STT: Checking device activation status from server for Device-Id: %s, Client-Id: %s", deviceID, clientID))
		isActivated, statusMsg, err := xiaozhi.CheckDeviceActivationFromServer(deviceID, clientID)
		if err != nil {
			logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  WARNING - Failed to check activation status from server: %v", err))
		} else {
			if isActivated {
				logger.Println(fmt.Sprintf("Xiaozhi STT: ✅ Device-Id %s is ACTIVATED on server: %s", deviceID, statusMsg))
			} else {
				logger.Println(fmt.Sprintf("Xiaozhi STT: ❌ Device-Id %s is NOT ACTIVATED on server: %s", deviceID, statusMsg))
				logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  CRITICAL WARNING - Device must be activated before STT will work. This will cause 'Error occurred while processing message'."))
				logger.Println(fmt.Sprintf("Xiaozhi STT: Please pair/activate device %s with Client-Id %s on the server first.", deviceID, clientID))
			}
		}
	} else {
		logger.Println("Xiaozhi STT: WARNING - No Device-Id configured. Server may reject the connection.")
	}
	if clientID == "" {
		// Nếu chưa có Client-Id, generate mới (GetClientIDFromConfig() sẽ tự động generate nếu Knowledge.Provider == "xiaozhi")
		// Nhưng nếu Knowledge.Provider != "xiaozhi", cần generate thủ công
		clientID = xiaozhi.GenerateClientID()
		logger.Println(fmt.Sprintf("Xiaozhi STT: Generated new Client-Id: %s", clientID))
	}
	headers.Add("Client-Id", clientID)
	logger.Println(fmt.Sprintf("Xiaozhi STT: Using Client-Id: %s (giống ESP32 - bắt buộc)", clientID))

	// Authorization: chỉ gửi nếu có token (hiện tại chưa có token trong config)
	// Nếu device đã activate, server có thể yêu cầu token trong header

	// Bước 0: Kiểm tra xem có connection cũ có thể reuse không
	// CHỈ reuse nếu connection không đang được LLM sử dụng
	var conn *websocket.Conn
	var sessionID string
	var connReused bool

	if deviceID != "" {
		// First check if connection exists and is valid (regardless of in-use status)
		// This is important: we need to know if connection exists even if it's "in use"
		storedConn, storedSessionID, connectionExists := xiaozhi.CheckConnectionExists(deviceID)
		connectionValid := connectionExists && storedConn != nil && storedConn.RemoteAddr() != nil && xiaozhi.IsReaderRunning(deviceID)

		// SIMPLIFIED: No need to wait for "in use" (giống botkct.py - connection luôn available)
		// Connection can be used immediately if valid, routing handles which handler gets messages
		// Only check if connection is valid, not if it's "in use"
		if connectionValid {
			// Connection exists and is valid - can use immediately
			// No need to wait for "release" - connection is always available (giống botkct.py)
			safeLog("Xiaozhi STT: ✅ Connection for device %s is valid and available (giống botkct.py - no 'in use' concept)", deviceID)
		}

		// Re-check connection after waiting (it might have been closed)
		// Use CheckConnectionExists to check if connection still exists (regardless of in-use status)
		if connectionValid {
			storedConn, storedSessionID, connectionExists = xiaozhi.CheckConnectionExists(deviceID)
			connectionValid = connectionExists && storedConn != nil && storedConn.RemoteAddr() != nil && xiaozhi.IsReaderRunning(deviceID)
		}

		// Check if connection exists and can be reused (not in use)
		if connectionValid {
			safeLog("Xiaozhi STT: 🔍 Found existing connection for device %s (sessionID: %s), verifying validity...", deviceID, storedSessionID)

			// First check if reader goroutine is still running
			// If reader is not running, connection has likely failed
			if !xiaozhi.IsReaderRunning(deviceID) {
				logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Connection reader goroutine is not running, connection may have failed, creating new connection"))
				xiaozhi.CloseConnection(deviceID)
			} else if storedConn.RemoteAddr() != nil {
				// Reader is running, verify connection is still valid
				// Try to ping the connection to verify it's still alive
				// Set a short write deadline to test connection
				storedConn.SetWriteDeadline(time.Now().Add(1 * time.Second))
				pingErr := storedConn.WriteMessage(websocket.PingMessage, nil)
				storedConn.SetWriteDeadline(time.Time{}) // Clear deadline immediately

				if pingErr != nil {
					logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Reused connection failed ping test: %v, creating new connection", pingErr))
					// Connection is dead, remove it and create new one
					xiaozhi.CloseConnection(deviceID)
				} else {
					// Ping passed - connection is valid
					// IMPORTANT: Don't read from connection here - ConnectionManager is already reading from it
					// Reading from connection while ConnectionManager is also reading causes "repeated read on failed" panic
					// Just verify RemoteAddr is still valid (connection not closed)
					if storedConn.RemoteAddr() != nil {
						// Double-check reader is still running after ping
						if xiaozhi.IsReaderRunning(deviceID) {
							// Connection is valid and reader is running, reuse it
							conn = storedConn
							sessionID = storedSessionID
							connReused = true
							logger.Println(fmt.Sprintf("Xiaozhi STT: ✅ REUSING existing connection for device %s (sessionID: %s) - session kept alive for continuous conversation", deviceID, sessionID))
						} else {
							logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Reader goroutine stopped during ping test, connection may have failed, creating new connection"))
							xiaozhi.CloseConnection(deviceID)
						}
					} else {
						logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Reused connection became invalid after ping (RemoteAddr is nil), creating new connection"))
						xiaozhi.CloseConnection(deviceID)
					}
				}
			} else {
				logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Reused connection is invalid (RemoteAddr is nil), creating new connection"))
				xiaozhi.CloseConnection(deviceID)
			}
		} else {
			logger.Println(fmt.Sprintf("Xiaozhi STT: ℹ️  No existing connection found for device %s, will create new connection", deviceID))
		}
	}

	// Nếu không có connection để reuse, tạo connection mới
	if !connReused {
		// Check context before creating new connection
		select {
		case <-ctx.Done():
			safeLog("Xiaozhi STT: ⚠️  Context canceled before creating new connection: %v", ctx.Err())
			return "", fmt.Errorf("context canceled before creating new connection: %w", ctx.Err())
		default:
		}

		// Log tất cả headers được gửi để debug
		safeLog("Xiaozhi STT: Connecting to %s with headers:", baseURL)
		for key, values := range headers {
			for _, value := range values {
				safeLog("  %s: %s", key, value)
			}
		}

		var err error
		conn, _, err = websocket.DefaultDialer.DialContext(ctx, baseURL, headers)
		if err != nil {
			// Check if error is due to context cancellation
			if ctx.Err() != nil {
				safeLog("Xiaozhi STT: ⚠️  Context canceled during connection: %v", ctx.Err())
				return "", fmt.Errorf("context canceled during connection: %w", ctx.Err())
			}
			safeLog("Xiaozhi STT: Failed to connect: %v", err)
			return "", fmt.Errorf("failed to connect to xiaozhi: %w", err)
		}
		safeLog("Xiaozhi STT: ✅ New WebSocket connection created")

		// Set PongHandler to automatically respond to server pings
		// This helps keep connection alive - server may send ping and expect pong
		conn.SetPongHandler(func(appData string) error {
			logger.Println(fmt.Sprintf("Xiaozhi STT: ✅ Received pong from server for device %s", deviceID))
			return nil
		})

		// Set PingHandler to automatically respond to server pings (if server sends ping)
		conn.SetPingHandler(func(appData string) error {
			logger.Println(fmt.Sprintf("Xiaozhi STT: ✅ Received ping from server for device %s, sending pong", deviceID))
			// Respond with pong
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			err := conn.WriteMessage(websocket.PongMessage, []byte(appData))
			conn.SetWriteDeadline(time.Time{})
			return err
		})

		// KHÔNG đóng connection ngay - LLM sẽ dùng lại connection này (giống botkct.py)
		// Connection sẽ được đóng sau khi LLM xong hoặc sau timeout
		// defer conn.Close() // REMOVED - để LLM có thể dùng lại connection
	}
	logger.Println("Xiaozhi STT: Hello event sent successfully")

	// Step 1: Send hello event (chỉ nếu tạo connection mới, không cần nếu reuse)
	if !connReused {
		// Python client gửi: type, version, transport, audio_params, features, language
		// NOTE: Vector robot sends Opus audio at 16kHz (PROCESSED_SAMPLE_RATE = 16000)
		// We must send the ACTUAL sample rate of the audio in hello event (16kHz)
		// Server will create Opus decoder with this sample rate and then resample PCM to 24kHz internally
		// If we send 24kHz but audio is 16kHz, Opus decoder will fail!
		helloEvent := map[string]interface{}{
			"type":      "hello",
			"version":   1,
			"transport": "websocket", // ESP32/Python luôn gửi transport: "websocket"
			"features": map[string]interface{}{
				"mcp": true,
				"aec": true,
			},
			"language": "vi", // Vietnamese language (theo Python client)
			"audio_params": map[string]interface{}{
				"format":         "opus",
				"sample_rate":    16000, // Vector robot sends Opus at 16kHz - MUST match actual audio!
				"channels":       1,
				"frame_duration": 60, // Python client dùng 60ms, không phải 20ms
			},
		}
		// Log chi tiết hello event (giống botkct.py để debug)
		helloEventJSON, _ := json.Marshal(helloEvent)
		logger.Println(fmt.Sprintf("Xiaozhi STT: Sending hello event to %s with Device-Id: %s, Client-Id: %s", baseURL, deviceID, clientID))
		logger.Println(fmt.Sprintf("Xiaozhi STT: Hello event JSON: %s", string(helloEventJSON)))
		if err := conn.WriteJSON(helloEvent); err != nil {
			logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send hello: %v", err))
			return "", fmt.Errorf("failed to send hello: %w", err)
		}
		logger.Println("Xiaozhi STT: Hello event sent successfully")

		// Step 2: Read hello response
		var helloResp map[string]interface{}
		if err := conn.ReadJSON(&helloResp); err != nil {
			logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to read hello response: %v", err))
			return "", fmt.Errorf("failed to read hello response: %w", err)
		}

		// Log chi tiết hello response (giống botkct.py để debug)
		helloRespJSON, _ := json.MarshalIndent(helloResp, "", "  ")
		logger.Println(fmt.Sprintf("Xiaozhi STT: ========== HELLO RESPONSE FROM SERVER =========="))
		logger.Println(fmt.Sprintf("Xiaozhi STT: Hello response JSON:\n%s", string(helloRespJSON)))
		logger.Println(fmt.Sprintf("Xiaozhi STT: Hello response fields:"))
		for key, value := range helloResp {
			logger.Println(fmt.Sprintf("  %s: %v (type: %T)", key, value, value))
		}
		logger.Println(fmt.Sprintf("Xiaozhi STT: ================================================"))

		// Extract session_id from hello response (theo Python client)
		if sid, ok := helloResp["session_id"].(string); ok && sid != "" {
			sessionID = sid
			logger.Println(fmt.Sprintf("Xiaozhi STT: Extracted session_id from hello response: %s", sessionID))
		} else {
			logger.Println("Xiaozhi STT: ⚠️  WARNING - Hello response missing session_id field")
		}
	} else {
		logger.Println(fmt.Sprintf("Xiaozhi STT: ✅ Reusing connection, skipping hello event (sessionID: %s)", sessionID))
		// Verify connection is still valid before using it (ConnectionManager might have closed it)
		if conn != nil && conn.RemoteAddr() == nil {
			logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Reused connection became invalid (RemoteAddr is nil), will create new connection"))
			xiaozhi.CloseConnection(deviceID)
			connReused = false
			conn = nil
			sessionID = ""
			// Fall through to create new connection below
		} else if conn != nil {
			// Try to ping the connection to verify it's still alive
			conn.SetWriteDeadline(time.Now().Add(1 * time.Second))
			if pingErr := conn.WriteMessage(websocket.PingMessage, nil); pingErr != nil {
				logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Reused connection ping failed: %v, will create new connection", pingErr))
				xiaozhi.CloseConnection(deviceID)
				connReused = false
				conn = nil
				sessionID = ""
				// Fall through to create new connection below
			} else {
				conn.SetWriteDeadline(time.Time{}) // Clear deadline
				logger.Println(fmt.Sprintf("Xiaozhi STT: ✅ Reused connection ping successful, connection is still valid"))
			}
		}
	}

	// If connection became invalid after reuse check, create new one
	// This handles the case where ConnectionManager closed the connection between reuse check and usage
	if connReused && (conn == nil || conn.RemoteAddr() == nil) {
		logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Reused connection became invalid, creating new connection"))
		if deviceID != "" {
			xiaozhi.CloseConnection(deviceID)
		}
		connReused = false
		conn = nil
		sessionID = ""
	}

	// Step 3: Send listen start event
	// botkct.py: message = {"session_id": self.session_id, "type": "listen", "state": "start", "mode": "auto"}
	// go-xiaozhi-main: message = {"type": "listen", "mode": "manual", "state": "start"} (KHÔNG có session_id)
	// Áp dụng y chang go-xiaozhi-main: KHÔNG gửi session_id trong listen message
	listenStart := map[string]interface{}{
		"type":  "listen",
		"state": "start",
		"mode":  "auto", // go-xiaozhi-main dùng "manual", nhưng botkct.py dùng "auto" - giữ "auto" vì phù hợp với Vector robot
	}
	// go-xiaozhi-main KHÔNG gửi session_id trong listen message, nhưng botkct.py có gửi
	// Thử không gửi session_id để xem có khác biệt không
	// if sessionID != "" {
	// 	listenStart["session_id"] = sessionID
	// }
	// Verify connection is still valid before sending listen start
	// ConnectionManager might have closed it between reuse check and now
	if conn == nil || conn.RemoteAddr() == nil {
		logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Connection became invalid before sending listen start, creating new connection"))
		if connReused && deviceID != "" {
			xiaozhi.CloseConnection(deviceID)
		}
		connReused = false
		conn = nil
		sessionID = ""
		// Create new connection inline (similar to retry logic below)
		logger.Println(fmt.Sprintf("Xiaozhi STT: Creating new connection after reused connection became invalid"))
		var err2 error
		conn, _, err2 = websocket.DefaultDialer.DialContext(ctx, baseURL, headers)
		if err2 != nil {
			logger.Println("Xiaozhi STT: Failed to create new connection:", err2)
			return "", fmt.Errorf("failed to create new connection after reused connection became invalid: %w", err2)
		}
		logger.Println("Xiaozhi STT: ✅ New WebSocket connection created after reused connection became invalid")
		// Send hello event for new connection
		helloEvent := map[string]interface{}{
			"type":      "hello",
			"version":   1,
			"transport": "websocket",
			"features": map[string]interface{}{
				"audio": map[string]interface{}{
					"codecs":      []string{"opus"},
					"sample_rate": 16000,
					"channels":    1,
				},
			},
			"language": "vi",
		}
		if err2 := conn.WriteJSON(helloEvent); err2 != nil {
			logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send hello event on new connection: %v", err2))
			conn.Close()
			return "", fmt.Errorf("failed to send hello event on new connection: %w", err2)
		}
		logger.Println("Xiaozhi STT: Hello event sent successfully on new connection")
		// Read hello response
		var helloResp map[string]interface{}
		if err2 := conn.ReadJSON(&helloResp); err2 != nil {
			logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to read hello response on new connection: %v", err2))
			conn.Close()
			return "", fmt.Errorf("failed to read hello response on new connection: %w", err2)
		}
		// Extract session_id from hello response
		if sid, ok := helloResp["session_id"].(string); ok && sid != "" {
			sessionID = sid
			logger.Println(fmt.Sprintf("Xiaozhi STT: Extracted session_id from hello response on new connection: %s", sessionID))
		}
	}

	// Log chi tiết listen start event (giống botkct.py để debug)
	listenStartJSON, _ := json.Marshal(listenStart)
	logger.Println(fmt.Sprintf("Xiaozhi STT: Sending listen start event: %s", string(listenStartJSON)))
	// IMPORTANT: If connection will be stored (deviceID != ""), use direct write for listenStart
	// because connection is not stored yet. After connection is stored, ALWAYS use xiaozhi.WriteJSON/WriteMessage
	var err error
	if connReused && deviceID != "" {
		// Connection already stored, use helper with writeMu
		err = xiaozhi.WriteJSON(deviceID, listenStart)
	} else {
		// New connection, not stored yet - use direct write (no concurrent writes yet)
		err = conn.WriteJSON(listenStart)
	}
	if err != nil {
		logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send listen start: %v", err))
		// If reusing connection and it fails, connection is invalid - remove it and create new one
		if connReused && deviceID != "" {
			logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Reused connection is invalid, removing from manager and creating new connection", deviceID))
			xiaozhi.CloseConnection(deviceID) // Close and remove invalid connection
			// Retry with new connection
			connReused = false
			// Create new connection
			logger.Println(fmt.Sprintf("Xiaozhi STT: Creating new connection after reused connection failed"))
			var err2 error
			conn, _, err2 = websocket.DefaultDialer.DialContext(ctx, baseURL, headers)
			if err2 != nil {
				logger.Println("Xiaozhi STT: Failed to create new connection:", err2)
				return "", fmt.Errorf("failed to create new connection after reused connection failed: %w", err2)
			}
			logger.Println("Xiaozhi STT: ✅ New WebSocket connection created after reused connection failed")
			// Send hello event for new connection
			helloEvent := map[string]interface{}{
				"type":      "hello",
				"version":   1,
				"transport": "websocket",
				"features": map[string]interface{}{
					"mcp": true,
					"aec": true,
				},
				"language": "vi",
				"audio_params": map[string]interface{}{
					"format":         "opus",
					"sample_rate":    16000,
					"channels":       1,
					"frame_duration": 60,
				},
			}
			if err2 := conn.WriteJSON(helloEvent); err2 != nil {
				logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send hello: %v", err2))
				return "", fmt.Errorf("failed to send hello: %w", err2)
			}
			var helloResp map[string]interface{}
			if err2 := conn.ReadJSON(&helloResp); err2 != nil {
				logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to read hello response: %v", err2))
				return "", fmt.Errorf("failed to read hello response: %w", err2)
			}
			if sid, ok := helloResp["session_id"].(string); ok && sid != "" {
				sessionID = sid
				logger.Println(fmt.Sprintf("Xiaozhi STT: Extracted session_id from hello response: %s", sessionID))
			}
			// Retry sending listen start
			if err2 := conn.WriteJSON(listenStart); err2 != nil {
				logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send listen start on new connection: %v", err2))
				return "", fmt.Errorf("failed to send listen start: %w", err2)
			}
		} else {
			return "", fmt.Errorf("failed to send listen start: %w", err)
		}
	}
	logger.Println("Xiaozhi STT: Listen start event sent successfully, ready to receive audio")

	// Verify connection is still valid after sending listen start
	if conn.RemoteAddr() == nil {
		logger.Println("Xiaozhi STT: ⚠️  Connection is invalid after sending listen start, cannot proceed")
		if deviceID != "" && !connReused {
			conn.Close()
		} else if deviceID != "" {
			xiaozhi.CloseConnection(deviceID)
		}
		return "", fmt.Errorf("connection is invalid after sending listen start")
	}

	// Step 4: Setup STT handler and channels for async communication
	done := make(chan bool)
	transcriptChan := make(chan string, 10) // Increased buffer to 10 to handle long speech transcripts
	errChan := make(chan error, 1)
	errorOccurred := make(chan struct{}, 1)
	connectionFailed := make(chan bool, 1)

	// Create STT handler instance
	sttHandler := &STTHandler{
		transcriptChan:     transcriptChan,
		errChan:            errChan,
		errorOccurred:      errorOccurred,
		active:             true,
		transcriptReceived: false,
	}

	// Store connection and start reader goroutine (if new connection)
	// If reusing connection, reader goroutine is already running
	if !connReused && deviceID != "" {
		// Store connection with retry logic (in case of race condition with LLM release)
		maxRetries := 3
		retryInterval := 200 * time.Millisecond
		var storeErr error
		for retry := 0; retry < maxRetries; retry++ {
			storeErr = xiaozhi.StoreConnection(deviceID, conn, sessionID)
			if storeErr == nil {
				// Successfully stored
				break
			}
			// Check if error is "in use" - this means LLM hasn't fully released yet
			if strings.Contains(storeErr.Error(), "in use by LLM") {
				if retry < maxRetries-1 {
					safeLog("Xiaozhi STT: ⚠️  Connection still in use (retry %d/%d), waiting %v before retry...", retry+1, maxRetries, retryInterval)
					// Wait a bit before retrying
					select {
					case <-ctx.Done():
						conn.Close()
						return "", fmt.Errorf("context canceled while retrying to store connection: %w", ctx.Err())
					case <-time.After(retryInterval):
						// Continue to retry
					}
				} else {
					// Last retry failed
					safeLog("Xiaozhi STT: ⚠️  Failed to store connection after %d retries: %v", maxRetries, storeErr)
					conn.Close()
					return "", fmt.Errorf("failed to store connection after %d retries: %w", maxRetries, storeErr)
				}
			} else {
				// Different error - don't retry
				safeLog("Xiaozhi STT: ⚠️  Failed to store connection (non-retryable error): %v", storeErr)
				conn.Close()
				return "", fmt.Errorf("failed to store connection: %w", storeErr)
			}
		}
		if storeErr != nil {
			// All retries failed
			safeLog("Xiaozhi STT: ⚠️  Failed to store connection after all retries: %v", storeErr)
			conn.Close()
			return "", fmt.Errorf("failed to store connection after all retries: %w", storeErr)
		}
		safeLog("Xiaozhi STT: Stored NEW connection for device %s (sessionID: %s) - reader goroutine started", deviceID, sessionID)
		// IMPORTANT: After connection is stored, ping goroutine is running
		// All subsequent writes MUST use xiaozhi.WriteMessage/WriteJSON to avoid concurrent write errors
		connReused = true // Mark as "reused" so all writes use writeMu
	}

	// Register STT handler with connection manager
	// Ensure handler is active when registering (in case it was deactivated from previous request)
	if deviceID != "" {
		sttHandler.SetActive(true) // Ensure handler is active when registering
		xiaozhi.SetSTTHandler(deviceID, sttHandler)
		logger.Println(fmt.Sprintf("Xiaozhi STT: STT handler registered for device %s (active: true)", deviceID))
	}

	// Step 5: Audio sending goroutine (no separate reader goroutine - using connection manager's reader)
	// Similar to Vosk STT - accumulate audio and only send after user finishes speaking
	// Note: Python client gửi audio streaming liên tục, nhưng Go tích lũy và gửi sau end-of-speech
	// để phù hợp với Vector robot behavior (giống Vosk STT)
	go func() {
		defer func() {
			// Close channels when done
			defer func() {
				if r := recover(); r != nil {
					logger.Println(fmt.Sprintf("Xiaozhi STT: Recovered from panic while closing channels: %v", r))
				}
			}()
			close(transcriptChan)
			close(errChan)
		}()
		defer func() {
			// Send listen stop when done
			listenStop := map[string]interface{}{
				"type":  "listen",
				"state": "stop",
				"mode":  "auto",
			}
			// go-xiaozhi-main KHÔNG gửi session_id trong listen stop message
			// IMPORTANT: After connection is stored, ALWAYS use helper function with writeMu
			// to avoid concurrent write errors (ping goroutine is running)
			if deviceID != "" {
				// Connection is stored, use helper with writeMu
				xiaozhi.WriteJSON(deviceID, listenStop)
			} else {
				// No deviceID, connection not stored - use direct write (shouldn't happen in normal flow)
				conn.WriteJSON(listenStop)
			}
		}()

		// Initialize VAD detection
		sreq.DetectEndOfSpeech()

		chunkCount := 0         // Đếm số chunks đã gửi để log
		listenStopSent := false // Flag để đảm bảo chỉ gửi listen stop event một lần

		// KHÔNG gửi FirstReq (OpusHead/OpusTags) vì:
		// 1. go-xiaozhi-main KHÔNG gửi OpusHead/OpusTags - chỉ gửi OPUS audio frames
		// 2. Server đã biết format từ hello event (audio_params)
		// 3. FirstReq (3840 bytes) có thể chứa OpusHead/OpusTags mà server không mong đợi
		// if len(sreq.FirstReq) > 0 {
		// 	logger.Println(fmt.Sprintf("Xiaozhi STT: Skipping FirstReq (%d bytes) - server doesn't expect OpusHead/OpusTags", len(sreq.FirstReq)))
		// }

		// Tạo OPUS encoder để re-encode PCM → OPUS frames (16kHz, mono, VoIP)
		// Vector robot gửi OGG packets, nhưng server mong đợi raw OPUS frames
		opusEncoder, err := opus.NewEncoder(16000, 1, opus.AppVoIP)
		if err != nil {
			logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to create OPUS encoder: %v", err))
			func() {
				defer func() {
					if r := recover(); r != nil {
						logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send error to errChan (recovered from panic): %v", r))
					}
				}()
				select {
				case errChan <- fmt.Errorf("failed to create OPUS encoder: %w", err):
				default:
				}
			}()
			return
		}
		logger.Println("Xiaozhi STT: OPUS encoder created (16kHz, mono, VoIP) for OGG → OPUS conversion")

		// Frame size: 60ms @ 16kHz = 960 samples
		frameSize := 960
		pcmBuffer := []int16{} // Buffer để tích lũy PCM samples

		// Thêm delay nhỏ sau listen start để server sẵn sàng nhận audio (giống go-xiaozhi-main có delay 50ms)
		time.Sleep(50 * time.Millisecond)
		logger.Println("Xiaozhi STT: Ready to send audio chunks (after 50ms delay)")

		for {
			select {
			case <-done:
				return
			case <-errorOccurred:
				// Server đã trả về error, dừng gửi audio chunks
				logger.Println("Xiaozhi STT: Error occurred, stopping audio chunk sending")
				return
			default:
				chunk, err := sreq.GetNextStreamChunkOpus()
				// Update last audio chunk time when we receive audio from robot
				// This indicates robot is listening and sending audio
				if err == nil && len(chunk) > 0 && deviceID != "" {
					xiaozhi.UpdateLastAudioChunkTime(deviceID)
				}
				if err != nil {
					if err == io.EOF {
						logger.Println(fmt.Sprintf("Xiaozhi STT: End of audio stream (EOF) detected after %d chunks", chunkCount))
						// Gửi listen stop event khi hết audio (theo go-xiaozhi-main - KHÔNG có session_id)
						listenStop := map[string]interface{}{
							"type":  "listen",
							"state": "stop",
							"mode":  "auto",
						}
						// go-xiaozhi-main KHÔNG gửi session_id trong listen stop message
						listenStopJSON, _ := json.Marshal(listenStop)
						logger.Println(fmt.Sprintf("Xiaozhi STT: Sending listen stop event after EOF: %s", string(listenStopJSON)))
						// IMPORTANT: After connection is stored, ALWAYS use helper function with writeMu
						// to avoid concurrent write errors (ping goroutine is running)
						if deviceID != "" {
							// Connection is stored, use helper with writeMu
							xiaozhi.WriteJSON(deviceID, listenStop)
						} else {
							// No deviceID, connection not stored - use direct write (shouldn't happen in normal flow)
							conn.WriteJSON(listenStop)
						}
						// Use select with default to avoid blocking if no receiver
						select {
						case done <- true:
							// Signal sent successfully
						default:
							// No receiver, skip (channel might be closed or no receiver)
							logger.Println("Xiaozhi STT: done channel has no receiver, skipping signal")
						}
						return
					}
					// Check if error is "context canceled" or "DeadlineExceeded" - this is not a real error, just user cancel or timeout
					// Don't send to errChan, just return empty transcript (connection will be kept for reuse)
					errStr := err.Error()
					if strings.Contains(errStr, "context canceled") || strings.Contains(errStr, "DeadlineExceeded") || strings.Contains(errStr, "deadline exceeded") || strings.Contains(errStr, "Canceled") {
						logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Context canceled while getting audio chunk (user cancel or timeout) - returning empty transcript, keeping connection"))
						// Signal done to stop audio sending loop
						select {
						case done <- true:
						default:
						}
						// Send empty transcript to transcriptChan (not error)
						select {
						case transcriptChan <- "":
							logger.Println("Xiaozhi STT: Empty transcript sent to channel (context canceled)")
						default:
						}
						return
					}
					// Try to send error, but don't panic if channel is closed
					logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to get audio chunk: %v (type: %T)", err, err))
					func() {
						defer func() {
							if r := recover(); r != nil {
								logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send error to errChan (recovered from panic): %v, Original error: %v", r, err))
							}
						}()
						select {
						case errChan <- err:
							logger.Println("Xiaozhi STT: Error sent to errChan successfully")
						default:
							// Channel might be closed or full, just log
							logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - errChan is full or closed, cannot send error: %v", err))
						}
					}()
					return
				}

				// Check for end-of-speech detection
				speechIsDone, doProcess := sreq.DetectEndOfSpeech()

				// Gửi audio chunk ngay lập tức nếu doProcess (giống botkct.py - streaming liên tục)
				// botkct.py gửi mỗi OPUS frame ngay khi encode xong, không tích lũy
				// LƯU Ý: Vector robot gửi OGG packets (có thể chứa nhiều OPUS frames)
				// Server mong đợi raw OPUS frames, không phải OGG packets
				// Giải pháp: Decode OGG → PCM → Re-encode thành OPUS frames
				if doProcess {
					// Kiểm tra error trước khi gửi
					select {
					case <-errorOccurred:
						logger.Println("Xiaozhi STT: Error occurred before sending audio chunk, stopping")
						return
					default:
					}

					chunkCount++

					// Kiểm tra xem có phải OGG format không (OGG bắt đầu với "OggS")
					isOGG := len(chunk) >= 4 && chunk[0] == 0x4f && chunk[1] == 0x67 && chunk[2] == 0x67 && chunk[3] == 0x53

					if isOGG {
						// Decode OGG → PCM
						// LƯU Ý: OpusStream có thể bị corrupt nếu OGG packets bị fragment hoặc incomplete
						// Khi reuse connection, cần đảm bảo OpusStream state được reset đúng cách
						decodedPCM := sreq.OpusDecode(chunk)
						if len(decodedPCM) == 0 {
							// Skip empty chunks (có thể do lỗi decode hoặc corrupt stream)
							// Chunk #1 thường là OGG header/metadata, không chứa audio - đây là bình thường
							// Chỉ log warning nếu không phải chunk đầu tiên hoặc nếu có nhiều empty chunks liên tiếp
							if chunkCount == 1 {
								// Chunk #1 thường là OGG header - không cần log warning
								logger.Println(fmt.Sprintf("Xiaozhi STT: ℹ️  Skipping chunk #1 (OGG header/metadata, %d bytes) - this is normal", len(chunk)))
							} else if chunkCount%50 == 0 {
								// Log mỗi 50 chunks để debug nếu có nhiều empty chunks
								logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Skipping empty decoded chunk (chunk #%d, %d bytes) - may be corrupt OGG packet", chunkCount, len(chunk)))
							}
							continue
						}

						// Validate decoded PCM data
						if len(decodedPCM)%2 != 0 {
							// Invalid PCM data (must be even number of bytes for int16 samples)
							logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Invalid PCM data length (%d bytes, not even) - skipping chunk #%d", len(decodedPCM), chunkCount))
							continue
						}

						// Convert PCM bytes → int16 samples
						samples := make([]int16, len(decodedPCM)/2)
						for i := 0; i < len(decodedPCM)/2; i++ {
							samples[i] = int16(binary.LittleEndian.Uint16(decodedPCM[i*2:]))
						}

						// Thêm samples vào buffer
						pcmBuffer = append(pcmBuffer, samples...)

						// Encode PCM → OPUS frames (60ms = 960 samples @ 16kHz)
						// Gửi từng OPUS frame riêng biệt (giống botkct.py)
						for len(pcmBuffer) >= frameSize {
							frameSamples := pcmBuffer[:frameSize]
							pcmBuffer = pcmBuffer[frameSize:]

							// Encode frame thành OPUS
							opusFrame := make([]byte, 1275) // Max OPUS frame size
							n, err := opusEncoder.Encode(frameSamples, opusFrame)
							if err != nil {
								logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to encode OPUS frame: %v", err))
								continue
							}

							if n > 0 {
								// Check connection validity before sending
								if conn.RemoteAddr() == nil {
									logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Connection is invalid (RemoteAddr is nil), stopping audio sending"))
									select {
									case connectionFailed <- true:
									default:
									}
									select {
									case errorOccurred <- struct{}{}:
									default:
									}
									return
								}
								// IMPORTANT: After connection is stored, ALWAYS use helper function with writeMu
								// to avoid concurrent write errors (ping goroutine is running)
								var err error
								if deviceID != "" {
									// Connection is stored, use helper with writeMu
									err = xiaozhi.WriteMessage(deviceID, websocket.BinaryMessage, opusFrame[:n])
								} else {
									// No deviceID, connection not stored - use direct write (shouldn't happen in normal flow)
									err = conn.WriteMessage(websocket.BinaryMessage, opusFrame[:n])
								}
								if err != nil {
									// Check if connection was closed gracefully (don't log as error)
									if strings.Contains(err.Error(), "close sent") || strings.Contains(err.Error(), "use of closed network connection") {
										logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Connection closed while sending OPUS frame, stopping audio sending"))
									} else {
										logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send OPUS frame (%d bytes): %v", n, err))
									}
									// Mark connection as failed
									select {
									case connectionFailed <- true:
									default:
									}
									select {
									case errorOccurred <- struct{}{}:
									default:
									}
									return
								}

								// Log mỗi 10 frames để tránh spam logs
								if chunkCount%10 == 0 || chunkCount == 1 {
									logger.Println(fmt.Sprintf("Xiaozhi STT: Sent OPUS frame %d (from OGG chunk %d): %d bytes", chunkCount, chunkCount, n))
								}

								// Thêm delay nhỏ giữa các frames (giống botkct.py có sleep 0.01s)
								time.Sleep(10 * time.Millisecond)
							}
						}
					} else {
						// Không phải OGG format, gửi trực tiếp (có thể đã là raw OPUS)
						if chunkCount == 1 {
							logger.Println(fmt.Sprintf("Xiaozhi STT: First audio chunk: %d bytes (not OGG format, sending directly)", len(chunk)))
						}

						// Check connection validity before sending
						if conn.RemoteAddr() == nil {
							logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Connection is invalid (RemoteAddr is nil), stopping audio sending"))
							select {
							case connectionFailed <- true:
							default:
							}
							select {
							case errorOccurred <- struct{}{}:
							default:
							}
							return
						}
						// IMPORTANT: After connection is stored, ALWAYS use helper function with writeMu
						// to avoid concurrent write errors (ping goroutine is running)
						var err error
						if deviceID != "" {
							// Connection is stored, use helper with writeMu
							err = xiaozhi.WriteMessage(deviceID, websocket.BinaryMessage, chunk)
						} else {
							// No deviceID, connection not stored - use direct write (shouldn't happen in normal flow)
							err = conn.WriteMessage(websocket.BinaryMessage, chunk)
						}
						if err != nil {
							// Check if connection was closed gracefully (don't log as error)
							if strings.Contains(err.Error(), "close sent") || strings.Contains(err.Error(), "use of closed network connection") {
								logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Connection closed while sending audio chunk, stopping audio sending"))
							} else {
								logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send audio chunk (%d bytes): %v", len(chunk), err))
							}
							// Mark connection as failed
							select {
							case connectionFailed <- true:
							default:
							}
							// Signal error occurred
							select {
							case errorOccurred <- struct{}{}:
							default:
							}
							// Try to send error, but don't panic if channel is closed
							func() {
								defer func() {
									if r := recover(); r != nil {
										logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send error to errChan (recovered from panic): %v, Original error: %v", r, err))
									}
								}()
								select {
								case errChan <- fmt.Errorf("failed to send audio: %w", err):
									logger.Println("Xiaozhi STT: Error sent to errChan successfully")
								default:
									logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - errChan is full or closed, cannot send error: %v", err))
								}
							}()
							return
						}

						// Log mỗi 10 chunks để tránh spam logs
						if chunkCount%10 == 0 || chunkCount == 1 {
							logger.Println(fmt.Sprintf("Xiaozhi STT: Sent audio chunk %d (streaming continuously like botkct.py): %d bytes", chunkCount, len(chunk)))
						}
						// Thêm delay nhỏ giữa các chunks (giống botkct.py có sleep 0.01s trong audio_streaming_loop)
						time.Sleep(10 * time.Millisecond)
					}
				}

				// Nếu speech is done, gửi listen stop và dừng (chỉ gửi một lần)
				if speechIsDone && !listenStopSent {
					// Kiểm tra xem có error từ server không trước khi gửi audio
					select {
					case <-errorOccurred:
						logger.Println("Xiaozhi STT: Error occurred before sending audio, aborting")
						return
					default:
					}

					logger.Println(fmt.Sprintf("Xiaozhi STT: End of speech detected after %d chunks. Audio was already streamed continuously (like botkct.py). Sending listen stop event...", chunkCount))

					// Send listen stop event
					// go-xiaozhi-main: message = {"type": "listen", "mode": "manual", "state": "stop"} (KHÔNG có session_id)
					// Áp dụng y chang go-xiaozhi-main: KHÔNG gửi session_id trong listen stop message
					listenStop := map[string]interface{}{
						"type":  "listen",
						"state": "stop",
						"mode":  "auto", // Giữ mode giống listen start
					}
					// go-xiaozhi-main KHÔNG gửi session_id trong listen stop message
					// if sessionID != "" {
					// 	listenStop["session_id"] = sessionID
					// }
					// IMPORTANT: After connection is stored, ALWAYS use helper function with writeMu
					// to avoid concurrent write errors (ping goroutine is running)
					var err error
					if deviceID != "" {
						// Connection is stored, use helper with writeMu
						err = xiaozhi.WriteJSON(deviceID, listenStop)
					} else {
						// No deviceID, connection not stored - use direct write (shouldn't happen in normal flow)
						err = conn.WriteJSON(listenStop)
					}
					if err != nil {
						logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send listen stop: %v", err))
						// Không return error ở đây, vì audio đã được gửi
					} else {
						logger.Println("Xiaozhi STT: Listen stop event sent successfully")
						listenStopSent = true // Đánh dấu đã gửi để tránh gửi lại
					}

					// Wait longer for server to process long speech (increased from 500ms to 2s)
					// This gives server more time to process long audio and send transcript
					time.Sleep(2 * time.Second)
					logger.Println(fmt.Sprintf("Xiaozhi STT: End of speech detected, waiting for transcript from server (after listen stop)"))

					// KHÔNG đóng connection ở đây - LLM reader sẽ tiếp tục đọc từ connection này
					// Chỉ dừng gửi audio chunks, nhưng tiếp tục đọc messages để LLM reader có thể xử lý
					logger.Println(fmt.Sprintf("Xiaozhi STT: End of speech detected, stopping audio chunk sending. Connection will be managed by LLM reader."))

					// Chỉ dừng gửi audio chunks, không return - STT reader sẽ tiếp tục đọc messages
					// LLM reader sẽ đọc và xử lý LLM/TTS events từ connection này
					// Use select with default to avoid blocking if no receiver
					select {
					case done <- true:
						// Signal sent successfully
					default:
						// No receiver, skip (channel might be closed or no receiver)
						logger.Println("Xiaozhi STT: done channel has no receiver, skipping signal")
					}
					// KHÔNG return ở đây - để STT reader tiếp tục đọc messages cho LLM reader
					// Connection sẽ được đóng bởi LLM reader khi xong
				}
			}
		}
	}()

	// Step 7: Wait for transcript or error
	// Increased timeout to 120s to handle long speech processing
	safeLog("Xiaozhi STT: Waiting for transcript or error (timeout: 120s)")
	select {
	case transcript := <-transcriptChan:
		safeLog("Xiaozhi STT: SUCCESS - Received transcript for device %s: %s", sreq.Device, transcript)

		// Check if connection failed before storing
		failed := false
		select {
		case failed = <-connectionFailed:
		default:
		}

		// Connection already stored in manager (if new connection) or already exists (if reused)
		// Handle empty transcript case: keep handler active if transcript is empty
		if deviceID != "" {
			// Check if connection is actually closed or failed
			if failed || conn.RemoteAddr() == nil {
				safeLog("Xiaozhi STT: ⚠️  Connection failed or invalid, closing")
				// Close and remove invalid connection
				if connReused {
					xiaozhi.CloseConnection(deviceID)
				} else {
					conn.Close()
				}
			} else if transcript == "" {
				// Empty transcript - keep STT handler active to handle server messages
				// This prevents "No active handler" errors when server sends messages
				safeLog("Xiaozhi STT: Empty transcript received, keeping STT handler active for device %s (sessionID: %s) to handle server messages", deviceID, sessionID)
				// Don't deactivate handler - it will be deactivated when next STT request comes or connection closes
			} else {
				// Non-empty transcript - deactivate STT handler, LLM will take over
				sttHandler.SetActive(false)
				safeLog("Xiaozhi STT: STT handler deactivated for device %s (sessionID: %s) - connection kept for LLM", deviceID, sessionID)
			}
		} else {
			// Nếu không có deviceID, đóng connection ngay
			safeLog("Xiaozhi STT: No deviceID, closing connection immediately")
			conn.Close()
		}
		return transcript, nil
	case err := <-errChan:
		// Check if error is "DeadlineExceeded" or "context canceled" - treat as empty transcript, don't close connection
		errStr := ""
		if err != nil {
			errStr = err.Error()
		}
		if strings.Contains(errStr, "DeadlineExceeded") || strings.Contains(errStr, "deadline exceeded") || strings.Contains(errStr, "context canceled") || strings.Contains(errStr, "Canceled") {
			safeLog("Xiaozhi STT: ⚠️  DeadlineExceeded/context canceled for device %s: %v - treating as empty transcript, keeping connection", sreq.Device, err)
			// Don't close connection - let it be reused for next request
			return "", nil // Return empty transcript, not error
		}
		safeLog("Xiaozhi STT: ERROR - Received error from errChan for device %s: %v (type: %T)", sreq.Device, err, err)
		// Đóng connection nếu có lỗi thực sự
		if deviceID != "" {
			xiaozhi.CloseConnection(deviceID) // Đóng connection khi có lỗi
		} else {
			conn.Close()
		}
		return "", err
	case <-ctx.Done():
		// Context canceled - this is not a real error, just user cancel or timeout
		// Don't close connection, just return empty transcript (connection will be kept for reuse)
		safeLog("Xiaozhi STT: ⚠️  Context canceled for device %s: %v - returning empty transcript, keeping connection", sreq.Device, ctx.Err())
		// Don't close connection - let it be reused for next request
		return "", nil // Return empty transcript, not error
	case <-time.After(120 * time.Second):
		safeLog("Xiaozhi STT: ERROR - Timeout waiting for transcript for device %s (120s)", sreq.Device)
		// Đóng connection nếu timeout
		if deviceID != "" {
			xiaozhi.CloseConnection(deviceID) // Đóng connection khi timeout
		} else {
			conn.Close()
		}
		return "", fmt.Errorf("timeout waiting for transcript")
	}
}
