package wirepod_ttr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fforchino/vector-go-sdk/pkg/vector"
	"github.com/fforchino/vector-go-sdk/pkg/vectorpb"
	"github.com/gorilla/websocket"
	"github.com/kercre123/wire-pod/chipper/pkg/logger"
	"github.com/kercre123/wire-pod/chipper/pkg/vars"
	"github.com/kercre123/wire-pod/chipper/pkg/xiaozhi"
	"gopkg.in/hraban/opus.v2"
)

// AudioQueue manages audio playback serialization per robot
type AudioQueue struct {
	ESN                   string
	AudioDone             chan bool
	AudioCurrentlyPlaying bool
}

var AudioQueues []AudioQueue
var audioQueueMutex sync.Mutex

// LLMHandler implements MessageHandler interface for LLM
// This handler processes LLM/TTS-related messages from the single reader goroutine
type LLMHandler struct {
	audioChunks     chan []byte
	textResponse    chan string
	errChan         chan error
	ttsStopChan     chan bool
	active          bool
	audioChunkCount int
	ttsStopped      bool      // Flag to indicate TTS has stopped (but don't close channel yet)
	lastFrameTime   time.Time // Track when last audio frame was received (for timeout-based channel closing)
	closeOnce       sync.Once // Ensure audioChunks is closed only once
	mu              sync.RWMutex
}

// HandleMessage processes messages from the WebSocket connection
func (h *LLMHandler) HandleMessage(messageType int, message []byte) error {
	h.mu.RLock()
	active := h.active
	h.mu.RUnlock()

	if !active {
		return nil // Handler is not active, ignore message
	}

	if messageType == websocket.TextMessage {
		var event map[string]interface{}
		if err := json.Unmarshal(message, &event); err != nil {
			logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ERROR - Failed to unmarshal message: %v", err))
			return err
		}

		eventType, ok := event["type"].(string)
		if !ok {
			return nil
		}

		switch eventType {
		case "llm":
			if text, ok := event["text"].(string); ok && text != "" {
				logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ LLM text: '%s'", text))
				select {
				case h.textResponse <- text:
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ LLM text sent to channel: '%s'", text))
				default:
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  textResponse channel is full, dropping text"))
				}
			}
		case "tts":
			if state, ok := event["state"].(string); ok {
				logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 🔊 TTS state: %s", state))
				if state == "start" {
					// Reset counter when TTS starts
					h.mu.Lock()
					h.audioChunkCount = 0
					h.mu.Unlock()
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ TTS started, ready to receive Opus frames"))
				} else if state == "sentence_start" {
					// TTS sentence_start contains the full text response (priority over LLM event)
					if text, ok := event["text"].(string); ok && text != "" {
						logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ TTS sentence_start text: '%s'", text))
						select {
						case h.textResponse <- text:
							logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ TTS sentence_start text sent to channel: '%s'", text))
						default:
							logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  textResponse channel is full, dropping text"))
						}
					}
				} else if state == "stop" {
					// TTS stopped - mark as stopped but don't close channel yet
					// Server may still send audio frames after TTS stop event (due to network delay/buffering)
					// We'll close the channel after a timeout (no new frames for 500ms) to ensure all frames are received
					h.mu.Lock()
					h.ttsStopped = true
					lastFrameTime := h.lastFrameTime // Use last frame time before TTS stop
					if lastFrameTime.IsZero() {
						lastFrameTime = time.Now() // If no frames received yet, use current time
					}
					h.mu.Unlock()
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 🔊 TTS stopped, will close audio channel after timeout (no new frames for 500ms) to receive remaining frames"))
					// Signal TTS stop
					select {
					case h.ttsStopChan <- true:
						logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ TTS stop signal sent"))
					default:
					}
					// Close channel after timeout (no new frames for 500ms) to allow remaining frames to be received
					// This ensures all audio frames sent by server are processed
					go func() {
						ticker := time.NewTicker(100 * time.Millisecond) // Check every 100ms
						defer ticker.Stop()
						timeout := 500 * time.Millisecond // Close if no new frames for 500ms
						for {
							select {
							case <-ticker.C:
								h.mu.RLock()
								ttsStopped := h.ttsStopped
								currentLastFrame := h.lastFrameTime
								h.mu.RUnlock()
								// Use the later of: lastFrameTime (when TTS stopped) or currentLastFrame (if new frames arrived)
								checkTime := lastFrameTime
								if currentLastFrame.After(checkTime) {
									checkTime = currentLastFrame
								}
								if ttsStopped && time.Since(checkTime) >= timeout {
									// No new frames for 500ms, close channel
									h.closeOnce.Do(func() {
										logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 🔒 Closing audio channel after timeout (no new frames for 500ms)"))
										close(h.audioChunks)
									})
									return
								}
							case <-time.After(2 * time.Second):
								// Safety timeout - close after 2 seconds max
								h.mu.RLock()
								ttsStopped := h.ttsStopped
								h.mu.RUnlock()
								if ttsStopped {
									h.closeOnce.Do(func() {
										logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 🔒 Closing audio channel after safety timeout (2s)"))
										close(h.audioChunks)
									})
									return
								}
							}
						}
					}()
				}
			}
		case "error":
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
		}
	} else if messageType == websocket.BinaryMessage {
		// Audio data (Opus-encoded from server)
		// Server sends Opus frames directly, we should forward them immediately
		// Don't buffer - send Opus frames as they arrive for real-time playback
		h.mu.Lock()
		h.audioChunkCount++
		count := h.audioChunkCount
		audioChunks := h.audioChunks // Capture channel reference
		h.mu.Unlock()

		// Send Opus frame immediately (don't wait for large buffer)
		// Audio processing goroutine will decode Opus → PCM → Downsample → Send to robot
		// Use recover to handle panic if channel is closed
		func() {
			defer func() {
				if r := recover(); r != nil {
					// Channel is closed - this can happen if TTS stopped but ConnectionManager
					// still receives messages from the server
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  audioChunks channel is closed, dropping Opus frame (recovered from panic: %v)", r))
				}
			}()
			select {
			case audioChunks <- message:
				// Update last frame time when successfully sent
				h.mu.Lock()
				h.lastFrameTime = time.Now()
				h.mu.Unlock()
				if count == 1 || count%10 == 0 {
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ Opus frame sent (frame #%d, %d bytes)", count, len(message)))
				}
			default:
				logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  audioChunks channel is full, dropping Opus frame"))
			}
		}()
	}
	return nil
}

// IsActive returns whether the handler is currently active
func (h *LLMHandler) IsActive() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.active
}

// SetActive sets the handler as active or inactive
func (h *LLMHandler) SetActive(active bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.active = active
	if !active {
		// Reset counter when deactivating
		h.audioChunkCount = 0
	}
}

// WaitForAudio_Queue waits for current audio playback to complete before starting new one
func WaitForAudio_Queue(esn string) {
	audioQueueMutex.Lock()
	defer audioQueueMutex.Unlock()

	for i, q := range AudioQueues {
		if q.ESN == esn {
			if q.AudioCurrentlyPlaying {
				audioQueueMutex.Unlock()
				logger.Println(fmt.Sprintf("[Xiaozhi Audio Queue] Device: %s | Waiting for current audio to finish...", esn))
				for range AudioQueues[i].AudioDone {
					break
				}
				audioQueueMutex.Lock()
				logger.Println(fmt.Sprintf("[Xiaozhi Audio Queue] Device: %s | Previous audio finished, starting new audio", esn))
			}
			return
		}
	}
}

// StartAudio_Queue marks audio playback as started
func StartAudio_Queue(esn string) {
	audioQueueMutex.Lock()
	defer audioQueueMutex.Unlock()

	// Check if queue exists for this ESN
	for i, q := range AudioQueues {
		if q.ESN == esn {
			if q.AudioCurrentlyPlaying {
				audioQueueMutex.Unlock()
				logger.Println(fmt.Sprintf("[Xiaozhi Audio Queue] Device: %s | Waiting for previous audio to finish...", esn))
				for range AudioQueues[i].AudioDone {
					break
				}
				audioQueueMutex.Lock()
			}
			AudioQueues[i].AudioCurrentlyPlaying = true
			logger.Println(fmt.Sprintf("[Xiaozhi Audio Queue] Device: %s | Audio playback started", esn))
			return
		}
	}

	// Create new queue if doesn't exist
	var aq AudioQueue
	aq.AudioCurrentlyPlaying = true
	aq.AudioDone = make(chan bool, 1)
	aq.ESN = esn
	AudioQueues = append(AudioQueues, aq)
	logger.Println(fmt.Sprintf("[Xiaozhi Audio Queue] Device: %s | New audio queue created, audio playback started", esn))
}

// StopAudio_Queue marks audio playback as finished
func StopAudio_Queue(esn string) {
	audioQueueMutex.Lock()
	defer audioQueueMutex.Unlock()

	for i, q := range AudioQueues {
		if q.ESN == esn {
			AudioQueues[i].AudioCurrentlyPlaying = false
			select {
			case AudioQueues[i].AudioDone <- true:
			default:
			}
			logger.Println(fmt.Sprintf("[Xiaozhi Audio Queue] Device: %s | Audio playback finished", esn))
			return
		}
	}
}

// StreamingXiaozhiKG handles knowledge graph requests using xiaozhi WebSocket
// This provides real-time voice conversation with TTS audio playback on robot
// isConversationMode: if true, LLM will use {{newVoiceRequest||now}} to continue conversation
func StreamingXiaozhiKG(esn string, transcribedText string, isKG bool, isConversationMode bool) (string, error) {
	// Create context for audio playback - don't cancel until audio is done
	// NOTE: audioCancel will be called by audio processing goroutine when it completes
	// We also cancel in error paths (timeout, error) as a safety net
	// Using sync.Once ensures it's only canceled once
	audioCtx, audioCancel := context.WithCancel(context.Background())
	var audioCancelOnce sync.Once
	audioCancelSafe := func() {
		audioCancelOnce.Do(audioCancel)
	}
	// NOTE: Do NOT cancel in defer here - let audio processing goroutine cancel it when done
	// This prevents vclient stream from closing while audio is still being sent
	// Only cancel in error paths (timeout, error) explicitly

	// Create separate context for LLM request (can be canceled when LLM response is received)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // Ensure context is canceled when function returns

	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ========== STARTING StreamingXiaozhiKG ==========", esn))
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ESN: %s, TranscribedText: '%s', isKG: %v, isConversationMode: %v", esn, esn, transcribedText, isKG, isConversationMode))

	// Get robot connection - try to create even if robot not in vars.BotInfo.Robots
	// This allows audio playback for any robot that makes a request
	var robot *vector.Vector
	var guid string
	var target string
	matched := false

	// Step 1: Try to find robot in vars.BotInfo.Robots
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Searching for robot in vars.BotInfo.Robots (count: %d)...", esn, len(vars.BotInfo.Robots)))
	for i, bot := range vars.BotInfo.Robots {
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s |   Checking Robot[%d]: ESN=%s, IP=%s, GUID=%s", esn, i, bot.Esn, bot.IPAddress, bot.GUID))
		if esn == bot.Esn {
			guid = bot.GUID
			if guid == "" {
				guid = vars.BotInfo.GlobalGUID
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s |   Robot GUID is empty, using GlobalGUID: %s", esn, guid))
			}
			target = bot.IPAddress + ":443"
			matched = true
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Found robot in vars.BotInfo.Robots (IP: %s, GUID: %s)", esn, bot.IPAddress, guid))
			break
		}
	}
	if !matched {
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Robot %s not found in vars.BotInfo.Robots", esn, esn))
	}

	var err error
	robot, err = vector.New(vector.WithSerialNo(esn), vector.WithToken(guid), vector.WithTarget(target))
	if err != nil {
		return "", err
	}

	_, err = robot.Conn.BatteryState(ctx, &vectorpb.BatteryStateRequest{})
	if err != nil {
		return "", err
	}

	// Lấy Device-Id từ config
	deviceID := xiaozhi.GetDeviceIDFromConfig()

	// Bước 1: Thử lấy connection từ STT (giống botkct.py - dùng cùng connection cho STT và text message)
	// Dùng CheckConnection để kiểm tra mà không mark "in use" (giống STT)
	var conn *websocket.Conn
	var sessionID string
	var connFromSTT bool

	if deviceID != "" {
		if storedConn, storedSessionID, exists := xiaozhi.CheckConnection(deviceID); exists {
			// Connection exists and can be reused - now mark it as "in use" for LLM
			conn = storedConn
			sessionID = storedSessionID
			connFromSTT = true
			// Mark connection as "in use" for LLM
			xiaozhi.GetConnection(deviceID) // This marks it as "in use"
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ REUSING connection from STT (sessionID: %s) - giống botkct.py", esn, sessionID))
		}
	}

	// Nếu không có connection từ STT, tạo connection mới
	if conn == nil {
		connFromSTT = false
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  No connection from STT, creating new connection", esn))

		// Get xiaozhi config
		baseURL, _, _ := xiaozhi.GetKnowledgeGraphConfig()
		if baseURL == "" {
			baseURL = "wss://api.tenclass.net/xiaozhi/v1/"
		}

		// Lấy Client-Id từ config
		clientID := xiaozhi.GetClientIDFromConfig()
		headers := http.Header{}
		// Protocol-Version header (giống botkct.py)
		headers.Add("Protocol-Version", "1")
		if deviceID != "" {
			headers.Add("Device-Id", deviceID)
			logger.Println("Xiaozhi KG: Using Device-Id from config:", deviceID)
		}
		if clientID != "" {
			headers.Add("Client-Id", clientID)
			logger.Println("Xiaozhi KG: Using Client-Id from config:", clientID)
		}
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | WebSocket connection headers: Protocol-Version=1, Device-Id=%s, Client-Id=%s", esn, deviceID, clientID))

		// Connect to xiaozhi WebSocket
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | 🔌 Connecting to WebSocket: %s", esn, baseURL))
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Headers: %v", esn, headers))
		var resp *http.Response
		var err error
		conn, resp, err = websocket.DefaultDialer.Dial(baseURL, headers)
		if err != nil {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ❌ WebSocket connection failed: %v (type: %T)", esn, err, err))
			if resp != nil {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | HTTP Response Status: %s", esn, resp.Status))
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | HTTP Response Headers: %v", esn, resp.Header))
			}
			return "", fmt.Errorf("failed to connect to xiaozhi: %w", err)
		}
		if resp != nil {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ WebSocket connected! HTTP Status: %s", esn, resp.Status))
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Response Headers: %v", esn, resp.Header))
		}

		// Step 1: Send hello event (giống botkct.py line 543-557)
		helloEvent := map[string]interface{}{
			"type":    "hello",
			"version": 1,
			"features": map[string]interface{}{
				"mcp": true,
				"aec": true,
			},
			"transport": "websocket",
			"language":  "vi",
			"audio_params": map[string]interface{}{
				"format":         "opus",
				"sample_rate":    16000, // botkct.py uses 16kHz
				"channels":       1,
				"frame_duration": 60, // botkct.py uses 60
			},
		}
		if err := conn.WriteJSON(helloEvent); err != nil {
			conn.Close()
			return "", fmt.Errorf("failed to send hello: %w", err)
		}

		// Read hello response
		var helloResp map[string]interface{}
		if err := conn.ReadJSON(&helloResp); err != nil {
			conn.Close()
			return "", fmt.Errorf("failed to read hello response: %w", err)
		}
		logger.Println("Xiaozhi KG: Connected and hello received")

		// Extract session_id from hello response
		if sid, ok := helloResp["session_id"].(string); ok && sid != "" {
			sessionID = sid
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Using session_id from hello: %s", esn, sessionID))
		}

		// Store new connection in manager and start reader (like STT does)
		if deviceID != "" {
			xiaozhi.StoreConnection(deviceID, conn, sessionID)
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Stored NEW connection for device %s (sessionID: %s) - reader goroutine started", esn, deviceID, sessionID))
			// Mark connection as "in use" for LLM
			xiaozhi.GetConnection(deviceID) // This marks it as "in use"
		}
	}

	// Cleanup: Giữ connection trong manager để reuse cho request tiếp theo (giống botkct.py)
	// Chỉ đóng connection nếu có lỗi hoặc connection không còn valid
	defer func() {
		// Connection is now always in manager (either from STT or newly created)
		// Don't close it here - let it be reused for next request
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Connection kept in manager for reuse (connFromSTT: %v)", esn, connFromSTT))
	}()

	// Step 2: Send text query (giống botkct.py line 789 - gửi trực tiếp sau khi nhận STT, KHÔNG gửi listen start)
	// Nếu dùng connection từ STT, sessionID đã có sẵn
	// Nếu tạo connection mới, sessionID đã được extract từ hello response ở trên
	// botkct.py (line 634-638) sử dụng format: {"session_id": "...", "type": "text", "text": "..."}
	// botkct.py KHÔNG gửi listen start trước text message, nó gửi text message trực tiếp trên cùng connection
	textMessage := map[string]interface{}{
		"type": "text",
		"text": transcribedText,
	}
	// Extract session_id from hello response if available (giống botkct.py)
	if sessionID != "" {
		textMessage["session_id"] = sessionID
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Using session_id from hello for text query: %s", esn, sessionID))
	}
	// KHÔNG thêm device_id hay client_id vào message body (theo botkct.py)

	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ========== SENDING TEXT QUERY ==========", esn))
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Sending text query: %s", esn, transcribedText))
	textMessageJSON, _ := json.Marshal(textMessage)
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Text message JSON: %s", esn, string(textMessageJSON)))
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Connection status: conn == nil? %v, connFromSTT? %v, sessionID: %s", esn, conn == nil, connFromSTT, sessionID))

	// Use WriteJSON helper if connection is in manager (to serialize writes)
	if deviceID != "" {
		if err := xiaozhi.WriteJSON(deviceID, textMessage); err != nil {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ❌ ERROR - Failed to send text query: %v", esn, err))
			return "", fmt.Errorf("failed to send text query: %w", err)
		}
	} else {
		if err := conn.WriteJSON(textMessage); err != nil {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ❌ ERROR - Failed to send text query: %v", esn, err))
			return "", fmt.Errorf("failed to send text query: %w", err)
		}
	}
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Text query sent successfully to server", esn))
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ========================================", esn))

	// Step 3: Note - xiaozhi expects audio input, not text
	// Since we already have transcribed text from STT, we need to:
	// Option 1: Use TTS to convert text back to audio (not ideal)
	// Option 2: Use xiaozhi's text query feature if available
	// Option 3: For now, we'll just wait for any audio response from xiaozhi
	// In a real implementation, you'd send the audio that was transcribed

	done := make(chan bool)
	audioChunks := make(chan []byte, 500) // Increased buffer to prevent dropping chunks
	textResponse := make(chan string, 5)  // Increased buffer to handle both LLM and TTS sentence_start events
	errChan := make(chan error, 1)
	ttsStopChan := make(chan bool, 1)         // Signal when TTS stops (for connection release timing)
	audioProcessingDone := make(chan bool, 1) // Signal when audio processing goroutine completes

	// Create LLM handler instance
	llmHandler := &LLMHandler{
		audioChunks:     audioChunks,
		textResponse:    textResponse,
		errChan:         errChan,
		ttsStopChan:     ttsStopChan,
		active:          true,
		audioChunkCount: 0,
	}

	// Register LLM handler with connection manager (always register, whether connection from STT or newly created)
	if deviceID != "" {
		xiaozhi.SetLLMHandler(deviceID, llmHandler)
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | LLM handler registered for device %s (connFromSTT: %v)", esn, deviceID, connFromSTT))
	}

	// Setup audio playback client (only if robot connection exists)
	// Use the same pattern as kgsim_cmds.go - let Go infer the type
	var vclient interface {
		Send(*vectorpb.ExternalAudioStreamRequest) error
	}
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Checking robot connection status: robot == nil? %v", esn, robot == nil))
	if robot != nil {
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Creating audio playback client for robot...", esn))
		// Use audioCtx instead of ctx to prevent stream from closing when LLM request completes
		audioClient, err := robot.Conn.ExternalAudioStreamPlayback(audioCtx)
		if err != nil {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  WARNING - Failed to create audio playback client: %v. Continuing without audio playback.", esn, err))
			vclient = nil
		} else {
			vclient = audioClient
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Audio playback client created successfully", esn))
			// Prepare audio stream - use 8kHz like Play Audio feature
			err = vclient.Send(&vectorpb.ExternalAudioStreamRequest{
				AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamPrepare{
					AudioStreamPrepare: &vectorpb.ExternalAudioStreamPrepare{
						AudioFrameRate: 8000, // Use 8kHz like Play Audio (works perfectly)
						AudioVolume:    100,
					},
				},
			})
			if err != nil {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  WARNING - Failed to send AudioStreamPrepare: %v. Disabling audio playback.", esn, err))
				vclient = nil // Disable audio playback if prepare fails
			} else {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ AudioStreamPrepare sent successfully (8kHz, volume 100)", esn))
			}
		}
	} else {
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Robot %s not available (robot == nil), skipping audio playback setup. TTS/STT will still work.", esn, esn))
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Robot connection was not created. Possible reasons:", esn))
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s |   1. Robot not found in vars.BotInfo.Robots (count: %d)", esn, len(vars.BotInfo.Robots)))
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s |   2. Robot not found in vars.RecurringInfo (count: %d)", esn, len(vars.RecurringInfo)))
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s |   3. Missing IP address or GUID", esn))
		vclient = nil
	}

	// No separate reader goroutine - using connection manager's reader
	// LLM handler will receive messages from connection manager's reader goroutine
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Using connection manager's reader goroutine (no separate reader)", esn))

	// Text query was already sent above (after hello response)
	// Now we wait for LLM response from the server
	// Messages will be handled by connection manager's reader goroutine and routed to LLM handler

	// Send text query (we need to convert to audio first)
	// For now, we'll send a simple text message
	// In production, you'd use TTS to convert text to Opus audio first
	logger.Println("Xiaozhi KG: Sending query: " + transcribedText)

	// Note: xiaozhi expects audio input, so we need to handle text differently
	// For now, we'll just wait for response

	// Play audio chunks to robot
	// Audio from xiaozhi is Opus-encoded at 24kHz
	// We need to: Decode Opus → PCM → Downsample 24k→16k → Send to robot
	go func() {
		// Capture audioCancelSafe to cancel audio context when done
		// This ensures audioCtx is canceled when audio processing completes
		defer audioCancelSafe()
		// Wait for previous audio to finish and start new audio queue
		WaitForAudio_Queue(esn)
		StartAudio_Queue(esn)
		defer StopAudio_Queue(esn) // Mark audio as finished when done

		// Wait a bit for vclient to be created (if robot connection is being established)
		// Check vclient status with timeout
		maxWaitTime := 5 * time.Second
		waitInterval := 100 * time.Millisecond
		waited := 0 * time.Millisecond
		for vclient == nil && waited < maxWaitTime {
			time.Sleep(waitInterval)
			waited += waitInterval
		}

		// Only process audio if robot connection exists
		if vclient == nil {
			logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  Skipping audio playback (robot not available after waiting %v)", esn, waited))
			return
		}
		logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ✅ vclient is ready, starting audio processing", esn))

		// Create Opus decoder for 24kHz, mono
		opusDecoder, err := opus.NewDecoder(24000, 1)
		if err != nil {
			logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ERROR - Failed to create Opus decoder: %v", esn, err))
			return
		}
		logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | Opus decoder created, ready for real-time streaming", esn))

		// Process OPUS chunks in real-time - same approach as /api-sdk/play_sound
		// Accumulate chunks to 1024 bytes before sending (like /api-sdk/play_sound)
		// But also send buffer if it's been waiting too long (>200ms) to avoid audio delay
		chunkCount := 0                                      // Track total chunks sent (moved outside loop to prevent reset)
		decodedFrameCount := 0                               // Track decoded frames for logging
		accumulatedBuffer := []byte{}                        // Accumulate chunks until we have 1024 bytes
		lastSendTime := time.Now()                           // Track when we last sent audio
		flushTimer := time.NewTicker(200 * time.Millisecond) // Flush buffer every 200ms if not empty
		defer flushTimer.Stop()
		for {
			select {
			case <-done:
				// TTS complete, send final buffer and completion
				if vclient != nil && len(accumulatedBuffer) > 0 {
					logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | 📤 Sending final buffer (%d bytes) before completion", esn, len(accumulatedBuffer)))
					err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
						AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamChunk{
							AudioStreamChunk: &vectorpb.ExternalAudioStreamChunk{
								AudioChunkSizeBytes: uint32(len(accumulatedBuffer)),
								AudioChunkSamples:   accumulatedBuffer,
							},
						},
					})
					if err != nil {
						logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  ERROR - Failed to send final audio chunk: %v", esn, err))
					} else {
						chunkCount++
						logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ✅ Sent final audio chunk %d to robot (%d bytes)", esn, chunkCount, len(accumulatedBuffer)))
						time.Sleep(time.Millisecond * 60)
					}
				}
				// Send completion
				if vclient != nil {
					err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
						AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamComplete{
							AudioStreamComplete: &vectorpb.ExternalAudioStreamComplete{},
						},
					})
					if err != nil {
						logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  ERROR - Failed to send AudioStreamComplete: %v", esn, err))
					} else {
						logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ✅ Audio stream complete sent to robot", esn))
					}
				}
				// Signal that audio processing is done
				select {
				case audioProcessingDone <- true:
				default:
				}
				return
			case chunk, ok := <-audioChunks:
				if !ok {
					logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | Audio channel closed, sending final buffer", esn))
					// Channel closed - send any remaining accumulated data
					if vclient != nil && len(accumulatedBuffer) > 0 {
						err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
							AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamChunk{
								AudioStreamChunk: &vectorpb.ExternalAudioStreamChunk{
									AudioChunkSizeBytes: uint32(len(accumulatedBuffer)),
									AudioChunkSamples:   accumulatedBuffer,
								},
							},
						})
						if err != nil {
							logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  ERROR - Failed to send final audio chunk: %v", esn, err))
						} else {
							chunkCount++
							logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ✅ Sent final audio chunk %d to robot (%d bytes)", esn, chunkCount, len(accumulatedBuffer)))
						}
						time.Sleep(time.Millisecond * 60)
					}
					// Send completion
					if vclient != nil {
						err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
							AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamComplete{
								AudioStreamComplete: &vectorpb.ExternalAudioStreamComplete{},
							},
						})
						if err != nil {
							logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  ERROR - Failed to send AudioStreamComplete: %v", esn, err))
						} else {
							logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ✅ Audio stream complete sent to robot", esn))
						}
					}
					logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | Audio channel closed, exiting", esn))
					// Signal that audio processing is done
					select {
					case audioProcessingDone <- true:
					default:
					}
					return
				}
				if len(chunk) == 0 {
					logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | Received empty audio chunk, skipping", esn))
					continue
				}

				logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ✅ Received audio chunk from handler: %d bytes", esn, len(chunk)))

				// If vclient is nil (closed), continue receiving frames to avoid blocking handler
				// but skip processing (decode, resample, send) since we can't send anymore
				if vclient == nil {
					logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  vclient is nil, skipping audio chunk processing (will continue receiving to avoid blocking handler)", esn))
					continue
				}

				// Decode OPUS → PCM
				pcmBuffer := make([]int16, 1440) // 60ms @ 24kHz max
				n, err := opusDecoder.Decode(chunk, pcmBuffer)
				if err != nil {
					// Log decode errors to debug
					logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  Opus decode error (skipping frame): %v, chunk size: %d bytes", esn, err, len(chunk)))
					continue
				}
				if n == 0 {
					logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  Opus decode returned 0 samples, chunk size: %d bytes", esn, len(chunk)))
					continue
				}

				// Log successful decode (first few frames and every 10th frame)
				decodedFrameCount++
				if decodedFrameCount == 1 || decodedFrameCount%10 == 0 {
					logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ✅ Opus decoded: %d samples (from %d bytes Opus frame, frame #%d)", esn, n, len(chunk), decodedFrameCount))
				}

				// Convert int16 → PCM bytes (little-endian)
				framePCMBytes := make([]byte, n*2)
				for i := 0; i < n; i++ {
					framePCMBytes[i*2] = byte(pcmBuffer[i])
					framePCMBytes[i*2+1] = byte(pcmBuffer[i] >> 8)
				}

				// Resample 24kHz → 8kHz (simple linear interpolation, like Play Audio)
				downsampledChunks := resample24kTo8kSimple(framePCMBytes)
				if len(downsampledChunks) == 0 {
					logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  Resample returned empty, skipping (PCM: %d bytes)", esn, len(framePCMBytes)))
					continue
				}

				// Accumulate all downsampled chunks into buffer
				downsampledSize := 0
				for _, c := range downsampledChunks {
					accumulatedBuffer = append(accumulatedBuffer, c...)
					downsampledSize += len(c)
				}

				// Log buffer status (first few times and when buffer is growing)
				if decodedFrameCount <= 5 || len(accumulatedBuffer) >= 1024 {
					logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | 📊 Buffer status: accumulated=%d bytes (from %d bytes downsampled), ready to send: %v", esn, len(accumulatedBuffer), downsampledSize, len(accumulatedBuffer) >= 1024))
				}

				// Send 1024-byte chunks (like /api-sdk/play_sound)
				// /api-sdk/play_sound only sends chunks >= 1024 bytes
				// Keep sending until buffer is less than 1024 bytes
				sentInThisIteration := 0
				for len(accumulatedBuffer) >= 1024 {
					lastSendTime = time.Now() // Update last send time
					if vclient == nil {
						logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  vclient became nil during sending, stopping audio processing", esn))
						// Signal done channel to stop processing gracefully
						select {
						case done <- true:
						default:
						}
						return
					}

					chunkToSend := accumulatedBuffer[:1024]
					accumulatedBuffer = accumulatedBuffer[1024:]

					err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
						AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamChunk{
							AudioStreamChunk: &vectorpb.ExternalAudioStreamChunk{
								AudioChunkSizeBytes: 1024, // Always 1024 bytes (like /api-sdk/play_sound)
								AudioChunkSamples:   chunkToSend,
							},
						},
					})
					if err != nil {
						logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  ERROR - Failed to send audio chunk: %v", esn, err))
						if strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "closed") {
							logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  vclient connection closed (EOF/closed), will continue receiving frames but not sending", esn))
							vclient = nil
							// Don't return here - continue receiving frames from channel to avoid blocking handler
							// Exit will happen when channel is closed (TTS stop)
							// Clear accumulated buffer since we can't send anymore
							accumulatedBuffer = []byte{}
							break // Break out of the inner for loop, continue with outer select
						}
					} else {
						chunkCount++
						sentInThisIteration++
						if chunkCount == 1 || chunkCount%10 == 0 || sentInThisIteration > 0 {
							logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ✅ Sent audio chunk %d to robot (1024 bytes), remaining buffer: %d bytes", esn, chunkCount, len(accumulatedBuffer)))
						}
					}

					// Use 60ms delay (same as /api-sdk/play_sound which works well)
					time.Sleep(time.Millisecond * 60)
				}
			case <-flushTimer.C:
				// Flush buffer if it's been waiting too long (>200ms) and has data
				// This ensures audio is sent even if buffer < 1024 bytes, preventing audio delay
				if len(accumulatedBuffer) > 0 && time.Since(lastSendTime) > 200*time.Millisecond {
					if vclient == nil {
						logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  vclient is nil during flush, skipping", esn))
						continue
					}
					// Send whatever we have (even if < 1024 bytes) to avoid delay
					chunkToSend := accumulatedBuffer
					accumulatedBuffer = []byte{} // Clear buffer
					err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
						AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamChunk{
							AudioStreamChunk: &vectorpb.ExternalAudioStreamChunk{
								AudioChunkSizeBytes: uint32(len(chunkToSend)),
								AudioChunkSamples:   chunkToSend,
							},
						},
					})
					if err != nil {
						logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  ERROR - Failed to send flushed audio chunk: %v", esn, err))
						if strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "closed") {
							logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  vclient connection closed during flush, will continue receiving frames but not sending", esn))
							vclient = nil
							// Don't return here - continue receiving frames from channel to avoid blocking handler
							// Exit will happen when channel is closed (TTS stop)
						}
					} else {
						chunkCount++
						logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ✅ Flushed audio chunk %d to robot (%d bytes) after 200ms timeout", esn, chunkCount, len(chunkToSend)))
						lastSendTime = time.Now()
						time.Sleep(time.Millisecond * 60)
					}
				}
			case err, ok := <-errChan:
				if !ok {
					// Signal that audio processing is done
					select {
					case audioProcessingDone <- true:
					default:
					}
					return
				}
				if err != nil {
					logger.Println("Xiaozhi KG error: " + err.Error())
				}
				// Signal that audio processing is done (error path)
				select {
				case audioProcessingDone <- true:
				default:
				}
				return
			}
		}
	}()

	// Wait for completion
	// Ensure esn is not empty to avoid nil pointer issues
	if esn == "" {
		esn = "unknown"
	}
	// Use recover to prevent panic from logger
	func() {
		defer func() {
			if r := recover(); r != nil {
				// Log to stderr directly to avoid logger issues
				fmt.Fprintf(os.Stderr, "[Xiaozhi KG] PANIC in logger (recovered): %v\n", r)
			}
		}()
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⏳ Waiting for LLM response (timeout: 30s)...", esn))
	}()
	select {
	case text := <-textResponse:
		// Ensure text is not nil/empty to avoid issues
		if text == "" {
			text = "(empty response)"
		}
		// Use recover to prevent panic from logger
		func() {
			defer func() {
				if r := recover(); r != nil {
					// Log to stderr directly to avoid logger issues
					fmt.Fprintf(os.Stderr, "[Xiaozhi KG] PANIC in logger (recovered): %v, esn: %s, text: %s\n", r, esn, text)
				}
			}()
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ========== LLM TEXT RESPONSE RECEIVED ==========", esn))
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM response text: '%s'", esn, text))
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Text length: %d bytes", esn, len(text)))
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Text will be returned to caller", esn))
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ==============================================", esn))
		}()

		// Wait for audio processing to complete before releasing connection
		// Keep WebSocket reader goroutine running continuously (like xiaozhi-esp32-main)
		// It will ignore messages when no LLM request is active
		// IMPORTANT: Wait for audio channel to close (audio processing done) instead of TTS stop event
		// This ensures all audio frames are processed even if TTS stop event is not received
		if connFromSTT {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⏳ Waiting for audio processing to complete before releasing connection (keeping reader goroutine running)...", esn))
			// Wait for either TTS stop event OR audio processing done (whichever comes first)
			// Use longer timeout (120s) for long responses
			select {
			case <-ttsStopChan:
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ TTS stop event received, waiting for audio processing to complete...", esn))
				// Wait for audio processing to complete (audio channel closed)
				select {
				case <-audioProcessingDone:
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Audio processing completed", esn))
				case <-time.After(10 * time.Second):
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Timeout waiting for audio processing (10s), proceeding anyway", esn))
				}
				// Wait a bit for WebSocket reader goroutine to process remaining messages
				time.Sleep(1 * time.Second)
				// Deactivate LLM handler - connection manager's reader will route messages to STT handler
				if deviceID != "" && connFromSTT {
					llmHandler.SetActive(false)
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM handler deactivated (connection manager will route to STT handler)", esn))
				}
				// Release connection (don't close it) so STT can reuse for next request
				// This keeps the session alive for continuous conversation
				if deviceID != "" {
					xiaozhi.ReleaseConnection(deviceID)
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Connection released (STT handler will handle next request)", esn))
				}
			case <-audioProcessingDone:
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Audio processing completed (audio channel closed)", esn))
				// Wait a bit for WebSocket reader goroutine to process remaining messages
				time.Sleep(1 * time.Second)
				// Deactivate LLM handler - connection manager's reader will route messages to STT handler
				if deviceID != "" && connFromSTT {
					llmHandler.SetActive(false)
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM handler deactivated (connection manager will route to STT handler)", esn))
				}
				// Release connection (don't close it) so STT can reuse for next request
				if deviceID != "" {
					xiaozhi.ReleaseConnection(deviceID)
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Connection released (STT handler will handle next request)", esn))
				}
			case <-time.After(120 * time.Second):
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Timeout waiting for audio processing (120s), releasing connection anyway", esn))
				// Deactivate LLM handler - connection manager's reader will route messages to STT handler
				if deviceID != "" && connFromSTT {
					llmHandler.SetActive(false)
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM handler deactivated (connection manager will route to STT handler)", esn))
				}
				// Release connection (don't close it) so STT can reuse for next request
				if deviceID != "" {
					xiaozhi.ReleaseConnection(deviceID)
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Connection released (STT handler will handle next request)", esn))
				}
			}
		} else {
			// For new connections, wait for audio processing to complete
			select {
			case <-audioProcessingDone:
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Audio processing completed", esn))
			case <-time.After(10 * time.Second):
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Timeout waiting for audio processing (10s), proceeding anyway", esn))
			}
		}

		// Check if conversation should continue (newVoiceRequest action)
		// If conversation mode is enabled (either via SaveChat or explicit request), trigger continuous listening
		shouldContinueConversation := (vars.APIConfig.Knowledge.SaveChat || isConversationMode) && strings.Contains(text, "{{newVoiceRequest||now}}")
		if shouldContinueConversation && robot != nil {
			logger.Println("Xiaozhi KG: Continuous conversation mode activated - robot will listen for next question")
			// Trigger robot to listen for next question without wake word
			// This happens after audio playback completes
			go func() {
				time.Sleep(1 * time.Second) // Wait a bit more for audio to finish
				DoNewRequest(robot)
			}()
		} else if shouldContinueConversation && robot == nil {
			logger.Println(fmt.Sprintf("Xiaozhi KG: Continuous conversation mode requested but robot %s not available", esn))
		}

		// Wait a bit for audio processing to complete before returning
		// This ensures audioCtx is not canceled too early, allowing audio chunks to be sent
		// Audio processing goroutine will cancel audioCtx when done
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⏳ Waiting for audio processing to complete before returning...", esn))
		time.Sleep(2 * time.Second) // Give audio processing time to finish sending remaining chunks

		// Don't cancel audioCtx here - let audio processing goroutine cancel it when done
		// This prevents vclient stream from closing while audio is still being sent
		return text, nil
	case <-time.After(30 * time.Second):
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ❌ TIMEOUT - No LLM response received after 30 seconds", esn))
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Debug info: textResponse channel length=%d, errChan length=%d", esn, len(textResponse), len(errChan)))
		// Cancel audioCtx on timeout (error path)
		audioCancelSafe()
		return "", fmt.Errorf("timeout waiting for response")
	case err := <-errChan:
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ❌ Error received from errChan: %v", esn, err))
		// Cancel audioCtx on error (error path)
		audioCancelSafe()
		return "", err
	}
}
