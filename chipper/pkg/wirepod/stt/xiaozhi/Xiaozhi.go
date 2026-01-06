package wirepod_vosk

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kercre123/wire-pod/chipper/pkg/logger"
	"github.com/kercre123/wire-pod/chipper/pkg/vars"
	sr "github.com/kercre123/wire-pod/chipper/pkg/wirepod/speechrequest"
	"github.com/kercre123/wire-pod/chipper/pkg/xiaozhi"
	opuslib "gopkg.in/hraban/opus.v2"
)

var Name string = "xiaozhi"

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
	logger.Println("(Bot " + sreq.Device + ", Xiaozhi) Processing...")

	// Get xiaozhi config
	baseURL, _, _ := xiaozhi.GetKnowledgeGraphConfig()
	if baseURL == "" {
		baseURL = "wss://api.tenclass.net/xiaozhi/v1/"
	}

	// Connect to xiaozhi WebSocket (using xiaozhi protocol)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	// Log tất cả headers được gửi để debug
	logger.Println(fmt.Sprintf("Xiaozhi STT: Connecting to %s with headers:", baseURL))
	for key, values := range headers {
		for _, value := range values {
			logger.Println(fmt.Sprintf("  %s: %s", key, value))
		}
	}

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, baseURL, headers)
	if err != nil {
		logger.Println("Xiaozhi STT: Failed to connect:", err)
		return "", fmt.Errorf("failed to connect to xiaozhi: %w", err)
	}
	// KHÔNG đóng connection ngay - LLM sẽ dùng lại connection này (giống botkct.py)
	// Connection sẽ được đóng sau khi LLM xong hoặc sau timeout
	// defer conn.Close() // REMOVED - để LLM có thể dùng lại connection

	// Step 1: Send hello event (following xiaozhi protocol from Python client)
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
	var sessionID string
	if sid, ok := helloResp["session_id"].(string); ok {
		sessionID = sid
		logger.Println(fmt.Sprintf("Xiaozhi STT: ✅ Extracted Session ID: %s", sessionID))
	} else {
		logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  WARNING - No session_id in hello response"))
	}

	// Validate hello response (giống botkct.py)
	if respType, ok := helloResp["type"].(string); !ok || respType != "hello" {
		logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  WARNING - Hello response type is not 'hello': %v", respType))
	}
	if respTransport, ok := helloResp["transport"].(string); !ok || respTransport != "websocket" {
		logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  WARNING - Hello response transport is not 'websocket': %v", respTransport))
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
	// Log chi tiết listen start event (giống botkct.py để debug)
	listenStartJSON, _ := json.Marshal(listenStart)
	logger.Println(fmt.Sprintf("Xiaozhi STT: Sending listen start event: %s", string(listenStartJSON)))
	if err := conn.WriteJSON(listenStart); err != nil {
		logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send listen start: %v", err))
		return "", fmt.Errorf("failed to send listen start: %w", err)
	}
	logger.Println("Xiaozhi STT: Listen start event sent successfully, ready to receive audio")

	// Step 4: Setup channels for async communication
	done := make(chan bool)
	transcriptChan := make(chan string, 1)
	errChan := make(chan error, 1)

	// Channel để signal khi có error từ server (để dừng gửi audio chunks)
	errorOccurred := make(chan struct{}, 1)

	// Step 5: Read messages from WebSocket (following xiaozhi protocol)
	go func() {
		defer func() {
			// Signal error occurred để dừng gửi audio chunks
			select {
			case errorOccurred <- struct{}{}:
			default:
			}
			// Only close channels if they haven't been closed yet
			// Use recover to prevent panic if channel is already closed
			defer func() {
				if r := recover(); r != nil {
					logger.Println(fmt.Sprintf("Xiaozhi STT: Recovered from panic while closing channels: %v", r))
				}
			}()
			close(transcriptChan)
			close(errChan)
		}()

		// Flag để biết đã nhận transcript chưa
		transcriptReceived := false

		for {
			// Nếu đã nhận transcript, dừng đọc - LLM reader sẽ tiếp tục đọc
			if transcriptReceived {
				// Kiểm tra xem connection còn trong manager không (LLM reader đã lấy chưa)
				if deviceID != "" {
					_, _, exists := xiaozhi.GetConnection(deviceID)
					if !exists {
						// LLM reader đã lấy connection, STT reader có thể dừng
						logger.Println(fmt.Sprintf("Xiaozhi STT: Connection taken by LLM reader, STT reader stopping."))
						return
					}
				}
				// Nếu LLM reader chưa lấy connection, tiếp tục đọc nhưng chỉ log (không xử lý)
				// Đợi một chút để LLM reader có cơ hội lấy connection
				time.Sleep(100 * time.Millisecond)
				continue
			}

			messageType, message, err := conn.ReadMessage()
			if err != nil {
				logger.Println(fmt.Sprintf("Xiaozhi STT: WebSocket ReadMessage error: %v (type: %T)", err, err))
				// Signal error occurred để dừng gửi audio chunks
				select {
				case errorOccurred <- struct{}{}:
				default:
				}
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Unexpected WebSocket close: %v", err))
					// Try to send error, but don't panic if channel is closed
					func() {
						defer func() {
							if r := recover(); r != nil {
								logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send error to errChan (recovered from panic): %v, Original error: %v", r, err))
							}
						}()
						select {
						case errChan <- fmt.Errorf("websocket error: %w", err):
							logger.Println("Xiaozhi STT: WebSocket error sent to errChan successfully")
						default:
							// Channel might be closed or full, just log
							logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - errChan is full or closed, cannot send WebSocket error: %v", err))
						}
					}()
				} else {
					logger.Println(fmt.Sprintf("Xiaozhi STT: WebSocket closed normally or expected error: %v", err))
				}
				return
			}

			if messageType == websocket.TextMessage {
				var event map[string]interface{}
				if err := json.Unmarshal(message, &event); err != nil {
					logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to unmarshal message: %v", err))
					msgLen := len(message)
					if msgLen > 500 {
						logger.Println(fmt.Sprintf("Xiaozhi STT: Raw message (first 500 chars): %s", string(message[:500])))
					} else {
						logger.Println(fmt.Sprintf("Xiaozhi STT: Raw message: %s", string(message)))
					}
					continue
				}

				// Log chi tiết tất cả events từ server (giống botkct.py để debug)
				eventJSON, _ := json.MarshalIndent(event, "", "  ")
				logger.Println(fmt.Sprintf("Xiaozhi STT: ========== EVENT RECEIVED FROM SERVER =========="))
				logger.Println(fmt.Sprintf("Xiaozhi STT: Event JSON:\n%s", string(eventJSON)))
				logger.Println(fmt.Sprintf("Xiaozhi STT: Event fields:"))
				for key, value := range event {
					logger.Println(fmt.Sprintf("  %s: %v (type: %T)", key, value, value))
				}
				logger.Println(fmt.Sprintf("Xiaozhi STT: ================================================"))

				// Check event type (following xiaozhi protocol from go-xiaozhi-main)
				if eventType, ok := event["type"].(string); ok {
					logger.Println(fmt.Sprintf("Xiaozhi STT: Processing event type: %s", eventType))
					switch eventType {
					case "stt":
						// STT event: {'type': 'stt', 'text': 'are youOK。', 'session_id': '9842a257'}
						logger.Println(fmt.Sprintf("Xiaozhi STT: ✅ STT EVENT - Full details:"))
						if text, ok := event["text"].(string); ok && text != "" {
							logger.Println(fmt.Sprintf("Xiaozhi STT: ✅ STT transcript text: '%s'", text))
							if sid, ok := event["session_id"].(string); ok {
								logger.Println(fmt.Sprintf("Xiaozhi STT: STT session_id: %s", sid))
							}
							select {
							case transcriptChan <- text:
								logger.Println(fmt.Sprintf("Xiaozhi STT: ✅ Transcript sent to channel successfully: '%s'", text))
								// Đánh dấu đã nhận transcript - STT reader sẽ dừng đọc, LLM reader sẽ tiếp tục
								transcriptReceived = true
								logger.Println(fmt.Sprintf("Xiaozhi STT: Transcript received, STT reader will stop reading. LLM reader will continue reading from this connection."))
							default:
								logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  WARNING: transcriptChan is full or closed, dropping transcript: '%s'", text))
							}
						} else {
							logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  WARNING - STT event received but text is empty or not a string"))
							logger.Println(fmt.Sprintf("Xiaozhi STT: STT event content: %+v", event))
						}
					case "listen":
						// Listen state change
						logger.Println(fmt.Sprintf("Xiaozhi STT: 📡 LISTEN EVENT - Full details:"))
						if state, ok := event["state"].(string); ok {
							logger.Println(fmt.Sprintf("Xiaozhi STT: Listen state: %s", state))
							if state == "stop" {
								logger.Println(fmt.Sprintf("Xiaozhi STT: Server requested listen stop, closing connection"))
								done <- true
								return
							}
						} else {
							logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  WARNING - Listen event without state field"))
						}
						if sid, ok := event["session_id"].(string); ok {
							logger.Println(fmt.Sprintf("Xiaozhi STT: Listen session_id: %s", sid))
						}
					case "mcp":
						// MCP event (Model Context Protocol)
						logger.Println(fmt.Sprintf("Xiaozhi STT: 🔧 MCP EVENT - Full details:"))
						if payload, ok := event["payload"].(map[string]interface{}); ok {
							logger.Println(fmt.Sprintf("Xiaozhi STT: MCP payload: %+v", payload))
						}
						if sid, ok := event["session_id"].(string); ok {
							logger.Println(fmt.Sprintf("Xiaozhi STT: MCP session_id: %s", sid))
						}
					case "tts":
						// TTS event
						logger.Println(fmt.Sprintf("Xiaozhi STT: 🔊 TTS EVENT - Full details:"))
						if state, ok := event["state"].(string); ok {
							logger.Println(fmt.Sprintf("Xiaozhi STT: TTS state: %s", state))
						}
						if text, ok := event["text"].(string); ok {
							logger.Println(fmt.Sprintf("Xiaozhi STT: TTS text: %s", text))
						}
						if sid, ok := event["session_id"].(string); ok {
							logger.Println(fmt.Sprintf("Xiaozhi STT: TTS session_id: %s", sid))
						}
					case "llm":
						// LLM event
						logger.Println(fmt.Sprintf("Xiaozhi STT: 🤖 LLM EVENT - Full details:"))
						if text, ok := event["text"].(string); ok {
							logger.Println(fmt.Sprintf("Xiaozhi STT: LLM text: %s", text))
						}
						if emotion, ok := event["emotion"].(string); ok {
							logger.Println(fmt.Sprintf("Xiaozhi STT: LLM emotion: %s", emotion))
						}
						if sid, ok := event["session_id"].(string); ok {
							logger.Println(fmt.Sprintf("Xiaozhi STT: LLM session_id: %s", sid))
						}
					case "error":
						// Error event - signal để dừng gửi audio chunks
						select {
						case errorOccurred <- struct{}{}:
							logger.Println("Xiaozhi STT: Error signal sent to stop audio sending")
						default:
						}

						// Log chi tiết error event từ server
						errorJSON, _ := json.MarshalIndent(event, "", "  ")
						logger.Println(fmt.Sprintf("Xiaozhi STT: ========== ERROR EVENT FROM SERVER =========="))
						logger.Println(fmt.Sprintf("Xiaozhi STT: Error event JSON:\n%s", string(errorJSON)))
						logger.Println(fmt.Sprintf("Xiaozhi STT: Error event fields:"))
						for key, value := range event {
							logger.Println(fmt.Sprintf("  %s: %v (type: %T)", key, value, value))
						}

						// Extract all possible error fields
						var errorMsg string
						if errMsg, ok := event["error"].(string); ok {
							errorMsg = errMsg
							logger.Println(fmt.Sprintf("Xiaozhi STT: Error message (from 'error' field): '%s'", errorMsg))
						} else if message, ok := event["message"].(string); ok {
							errorMsg = message
							logger.Println(fmt.Sprintf("Xiaozhi STT: Error message (from 'message' field): '%s'", errorMsg))
						} else {
							errorMsg = fmt.Sprintf("Unknown error format: %+v", event)
							logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  WARNING - No 'error' or 'message' field in error event"))
						}

						// Extract session_id if available
						errorSessionID := "unknown"
						if sid, ok := event["session_id"].(string); ok {
							errorSessionID = sid
							logger.Println(fmt.Sprintf("Xiaozhi STT: Error session_id: %s", errorSessionID))
						} else {
							logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  WARNING - No session_id in error event"))
						}

						logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR ANALYSIS:"))
						logger.Println(fmt.Sprintf("  Device-Id: %s", deviceID))
						logger.Println(fmt.Sprintf("  Client-Id: %s", clientID))
						logger.Println(fmt.Sprintf("  Session ID: %s", errorSessionID))
						logger.Println(fmt.Sprintf("  BaseURL: %s", baseURL))
						logger.Println(fmt.Sprintf("  Error message: %s", errorMsg))
						logger.Println(fmt.Sprintf("Xiaozhi STT: =============================================="))

						// Try to send error, but don't panic if channel is closed
						func() {
							defer func() {
								if r := recover(); r != nil {
									logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send error to errChan (recovered from panic): %v, Original error: %s", r, errorMsg))
								}
							}()
							select {
							case errChan <- fmt.Errorf("xiaozhi error (session: %s): %s", sessionID, errorMsg):
								logger.Println(fmt.Sprintf("Xiaozhi STT: Error sent to errChan successfully"))
							default:
								logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - errChan is full or closed, cannot send error: %s", errorMsg))
							}
						}()
						return
					default:
						logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  WARNING - Unknown event type: %v", eventType))
						logger.Println(fmt.Sprintf("Xiaozhi STT: Full event: %+v", event))
					}
				} else {
					logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  WARNING - Received message without 'type' field: %+v", event))
				}
				// Note: Binary messages (audio) are handled in the audio streaming goroutine
			} // Closing if messageType == websocket.TextMessage (line 241)
		} // Closing for loop
	}()

	// Step 6: Collect audio chunks and wait for end-of-speech before sending
	// Similar to Vosk STT - accumulate audio and only send after user finishes speaking
	// Note: Python client gửi audio streaming liên tục, nhưng Go tích lũy và gửi sau end-of-speech
	// để phù hợp với Vector robot behavior (giống Vosk STT)
	go func() {
		defer func() {
			// Send listen stop when done (backup - theo go-xiaozhi-main KHÔNG có session_id)
			listenStop := map[string]interface{}{
				"type":  "listen",
				"state": "stop",
				"mode":  "auto",
			}
			// go-xiaozhi-main KHÔNG gửi session_id trong listen stop message
			conn.WriteJSON(listenStop)
		}()

		// Initialize VAD detection
		sreq.DetectEndOfSpeech()

		chunkCount := 0 // Đếm số chunks đã gửi để log

		// KHÔNG gửi FirstReq (OpusHead/OpusTags) vì:
		// 1. go-xiaozhi-main KHÔNG gửi OpusHead/OpusTags - chỉ gửi OPUS audio frames
		// 2. Server đã biết format từ hello event (audio_params)
		// 3. FirstReq (3840 bytes) có thể chứa OpusHead/OpusTags mà server không mong đợi
		// if len(sreq.FirstReq) > 0 {
		// 	logger.Println(fmt.Sprintf("Xiaozhi STT: Skipping FirstReq (%d bytes) - server doesn't expect OpusHead/OpusTags", len(sreq.FirstReq)))
		// }

		// Tạo OPUS encoder để re-encode PCM → OPUS frames (16kHz, mono, VoIP)
		// Vector robot gửi OGG packets, nhưng server mong đợi raw OPUS frames
		opusEncoder, err := opuslib.NewEncoder(16000, 1, opuslib.AppVoIP)
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
						conn.WriteJSON(listenStop)
						done <- true
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
						decodedPCM := sreq.OpusDecode(chunk)
						if len(decodedPCM) == 0 {
							// Skip empty chunks
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
								// Gửi OPUS frame
								if err := conn.WriteMessage(websocket.BinaryMessage, opusFrame[:n]); err != nil {
									logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send OPUS frame (%d bytes): %v", n, err))
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

						// Gửi audio chunk trực tiếp (có thể đã là raw OPUS)
						if err := conn.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
							logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send audio chunk (%d bytes): %v", len(chunk), err))
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

				// Nếu speech is done, gửi listen stop và dừng
				if speechIsDone {
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
					if err := conn.WriteJSON(listenStop); err != nil {
						logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send listen stop: %v", err))
						// Không return error ở đây, vì audio đã được gửi
					} else {
						logger.Println("Xiaozhi STT: Listen stop event sent successfully")
					}

					// Wait a bit for server to process
					time.Sleep(500 * time.Millisecond)

					// KHÔNG đóng connection ở đây - LLM reader sẽ tiếp tục đọc từ connection này
					// Chỉ dừng gửi audio chunks, nhưng tiếp tục đọc messages để LLM reader có thể xử lý
					logger.Println(fmt.Sprintf("Xiaozhi STT: End of speech detected, stopping audio chunk sending. Connection will be managed by LLM reader."))

					// Chỉ dừng gửi audio chunks, không return - STT reader sẽ tiếp tục đọc messages
					// LLM reader sẽ đọc và xử lý LLM/TTS events từ connection này
					done <- true
					// KHÔNG return ở đây - để STT reader tiếp tục đọc messages cho LLM reader
					// Connection sẽ được đóng bởi LLM reader khi xong
				}
			}
		}
	}()

	// Step 7: Wait for transcript or error
	logger.Println("Xiaozhi STT: Waiting for transcript or error (timeout: 30s)")
	select {
	case transcript := <-transcriptChan:
		logger.Println(fmt.Sprintf("Xiaozhi STT: SUCCESS - Received transcript for device %s: %s", sreq.Device, transcript))
		// Lưu connection vào manager để LLM có thể dùng lại (giống botkct.py - dùng cùng connection cho STT và text message)
		if deviceID != "" {
			xiaozhi.StoreConnection(deviceID, conn, sessionID)
			logger.Println(fmt.Sprintf("Xiaozhi STT: Stored connection for device %s (sessionID: %s) - LLM will reuse this connection", deviceID, sessionID))
		} else {
			// Nếu không có deviceID, đóng connection ngay
			logger.Println("Xiaozhi STT: No deviceID, closing connection immediately")
			conn.Close()
		}
		return transcript, nil
	case err := <-errChan:
		logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Received error from errChan for device %s: %v (type: %T)", sreq.Device, err, err))
		// Đóng connection nếu có lỗi
		if deviceID != "" {
			xiaozhi.CloseConnection(deviceID) // Đóng connection khi có lỗi
		} else {
			conn.Close()
		}
		return "", err
	case <-ctx.Done():
		logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Context canceled for device %s: %v", sreq.Device, ctx.Err()))
		// Đóng connection nếu context canceled
		if deviceID != "" {
			xiaozhi.CloseConnection(deviceID) // Đóng connection khi context canceled
		} else {
			conn.Close()
		}
		return "", fmt.Errorf("context canceled: %w", ctx.Err())
	case <-time.After(30 * time.Second):
		logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Timeout waiting for transcript for device %s (30s)", sreq.Device))
		// Đóng connection nếu timeout
		if deviceID != "" {
			xiaozhi.CloseConnection(deviceID) // Đóng connection khi timeout
		} else {
			conn.Close()
		}
		return "", fmt.Errorf("timeout waiting for transcript")
	}
}
