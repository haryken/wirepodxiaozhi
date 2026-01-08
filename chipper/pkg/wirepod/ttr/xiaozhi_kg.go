package wirepod_ttr

import (
	"context"
	"encoding/binary"
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
	textResponse    chan string
	errChan         chan error
	ttsStopChan     chan bool
	active          bool
	audioChunkCount int
	ttsStopped      bool      // Flag to indicate TTS has stopped
	lastFrameTime   time.Time // Track when last audio frame was received
	mu              sync.RWMutex
	// Audio processing (synchronous)
	vclient interface {
		Send(*vectorpb.ExternalAudioStreamRequest) error
	}
	opusDecoder       *opus.Decoder
	accumulatedBuffer []byte
	chunkCount        int
	audioQueueStarted bool
	lastSendTime      time.Time // Track when we last sent audio (for flush timer)
	flushTimer        *time.Ticker
	flushTimerStop    chan bool // Signal to stop flush timer
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
					// TTS stopped - send final buffer and AudioStreamComplete (synchronous processing)
					h.mu.Lock()
					h.ttsStopped = true
					vclient := h.vclient
					accumulatedBuffer := h.accumulatedBuffer
					chunkCount := h.chunkCount
					flushTimerStop := h.flushTimerStop
					h.mu.Unlock()
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 🔊 TTS stopped, sending final buffer and completion"))

					// IMPORTANT: Stop flush timer FIRST and wait for it to fully stop
					// This ensures all pending chunks are sent before AudioStreamComplete
					if flushTimerStop != nil {
						select {
						case flushTimerStop <- true:
							logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ Flush timer stop signal sent"))
						default:
						}
						// Wait for flush timer to fully stop (give it time to finish current flush cycle)
						time.Sleep(200 * time.Millisecond)
					}

					// Signal TTS stop
					select {
					case h.ttsStopChan <- true:
						logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ TTS stop signal sent"))
					default:
					}

					// Wait longer for any remaining frames (1 second) to ensure all audio is received
					time.Sleep(1 * time.Second)

					// Re-check buffer after wait (in case new frames arrived)
					h.mu.Lock()
					accumulatedBuffer = h.accumulatedBuffer
					h.mu.Unlock()

					// Send final buffer and AudioStreamComplete
					if vclient != nil {
						// Send final buffer if any
						if len(accumulatedBuffer) > 0 {
							err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
								AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamChunk{
									AudioStreamChunk: &vectorpb.ExternalAudioStreamChunk{
										AudioChunkSizeBytes: uint32(len(accumulatedBuffer)),
										AudioChunkSamples:   accumulatedBuffer,
									},
								},
							})
							if err != nil {
								logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  ERROR - Failed to send final audio chunk: %v", err))
							} else {
								logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ Sent final audio chunk (%d bytes)", len(accumulatedBuffer)))
								// IMPORTANT: Wait longer before AudioStreamComplete to ensure final chunk is processed
								time.Sleep(500 * time.Millisecond)
							}
						}

						// IMPORTANT: Double-check buffer is empty before sending AudioStreamComplete
						// This ensures all chunks have been sent
						h.mu.Lock()
						if len(h.accumulatedBuffer) > 0 {
							logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  WARNING - Buffer still has %d bytes, sending as final chunk before AudioStreamComplete", len(h.accumulatedBuffer)))
							finalChunk := h.accumulatedBuffer
							h.accumulatedBuffer = []byte{}
							h.mu.Unlock()
							// Send remaining buffer
							if err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
								AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamChunk{
									AudioStreamChunk: &vectorpb.ExternalAudioStreamChunk{
										AudioChunkSizeBytes: uint32(len(finalChunk)),
										AudioChunkSamples:   finalChunk,
									},
								},
							}); err == nil {
								logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ Sent remaining buffer chunk (%d bytes)", len(finalChunk)))
								time.Sleep(500 * time.Millisecond)
							}
						} else {
							h.mu.Unlock()
						}

						// Send AudioStreamComplete - NOW all chunks should be sent
						err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
							AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamComplete{
								AudioStreamComplete: &vectorpb.ExternalAudioStreamComplete{},
							},
						})
						if err != nil {
							logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  ERROR - Failed to send AudioStreamComplete: %v", err))
						} else {
							logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ AudioStreamComplete sent (total chunks sent: %d)", chunkCount))
							// Clear buffer
							h.mu.Lock()
							h.accumulatedBuffer = []byte{}
							h.mu.Unlock()
							// IMPORTANT: Wait longer after AudioStreamComplete to ensure robot starts playing audio
							// Robot needs time to process all chunks and start playback
							time.Sleep(1 * time.Second)
						}
					}
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
		case "goodbye":
			// Server sends goodbye event - similar to ESP32, we should only signal TTS stop, not close connection
			// Connection should remain open for reuse (like ESP32 does)
			sessionID := ""
			if sid, ok := event["session_id"].(string); ok {
				sessionID = sid
			}
			logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 👋 Received goodbye event (session_id: %s) - signaling TTS stop but keeping connection for reuse", sessionID))
			// Signal TTS stop if not already signaled (this will trigger final buffer send)
			select {
			case h.ttsStopChan <- true:
			default:
			}
			// Don't close connection - let it be reused for next request (like ESP32)
		}
	} else if messageType == websocket.BinaryMessage {
		// Audio data (Opus-encoded from server)
		// Process audio synchronously: Decode Opus → PCM → Resample → Send to robot
		h.mu.Lock()
		h.audioChunkCount++
		count := h.audioChunkCount
		h.lastFrameTime = time.Now()
		vclient := h.vclient
		opusDecoder := h.opusDecoder
		accumulatedBuffer := h.accumulatedBuffer
		h.mu.Unlock()

		// Skip if vclient or opusDecoder is nil
		if vclient == nil || opusDecoder == nil {
			if count == 1 || count%50 == 0 {
				logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Skipping audio frame (vclient or opusDecoder is nil, frame #%d)", count))
			}
			return nil
		}

		// Decode OPUS → PCM
		pcmBuffer := make([]int16, 1440) // 60ms @ 24kHz max
		n, err := opusDecoder.Decode(message, pcmBuffer)
		if err != nil {
			if count == 1 || count%50 == 0 {
				logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Opus decode error (skipping frame #%d): %v", count, err))
			}
			return nil
		}
		if n == 0 {
			return nil
		}

		// Convert int16 → PCM bytes (little-endian)
		framePCMBytes := make([]byte, n*2)
		for i := 0; i < n; i++ {
			binary.LittleEndian.PutUint16(framePCMBytes[i*2:], uint16(pcmBuffer[i]))
		}

		// Resample 24kHz → 8kHz
		downsampledChunks := resample24kTo8kSimple(framePCMBytes)
		if len(downsampledChunks) == 0 {
			return nil
		}

		// Accumulate into buffer
		for _, c := range downsampledChunks {
			accumulatedBuffer = append(accumulatedBuffer, c...)
		}

		// Send audio chunks when buffer >= 256 bytes
		h.mu.Lock()
		for len(accumulatedBuffer) >= 256 {
			chunkSize := 1024
			if len(accumulatedBuffer) < 1024 {
				chunkSize = len(accumulatedBuffer)
			}
			chunkToSend := accumulatedBuffer[:chunkSize]
			accumulatedBuffer = accumulatedBuffer[chunkSize:]

			// Send to robot
			err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
				AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamChunk{
					AudioStreamChunk: &vectorpb.ExternalAudioStreamChunk{
						AudioChunkSizeBytes: uint32(len(chunkToSend)),
						AudioChunkSamples:   chunkToSend,
					},
				},
			})
			if err != nil {
				logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  ERROR - Failed to send audio chunk: %v", err))
				if strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "closed") {
					// vclient closed, stop processing
					h.vclient = nil
					h.mu.Unlock()
					return nil
				}
				break
			}
			h.chunkCount++
			if h.chunkCount == 1 || h.chunkCount%50 == 0 {
				logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ Sent audio chunk #%d (%d bytes)", h.chunkCount, len(chunkToSend)))
			}
			// Small delay between chunks (like play_sound)
			time.Sleep(time.Millisecond * 60)
		}
		h.accumulatedBuffer = accumulatedBuffer
		h.mu.Unlock()

		if count == 1 || count%10 == 0 {
			logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ Opus frame processed (frame #%d, %d bytes)", count, len(message)))
		}
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
	defer func() {
		if r := recover(); r != nil {
			// Use fmt.Fprintf to stderr to avoid logger panic
			fmt.Fprintf(os.Stderr, "[Xiaozhi Audio Queue] Device: %s | PANIC in StopAudio_Queue (recovered): %v\n", esn, r)
		}
	}()
	audioQueueMutex.Lock()
	defer audioQueueMutex.Unlock()

	for i, q := range AudioQueues {
		if q.ESN == esn {
			AudioQueues[i].AudioCurrentlyPlaying = false
			select {
			case AudioQueues[i].AudioDone <- true:
			default:
			}
			// Use safe logging to prevent panic
			func() {
				defer func() {
					if r := recover(); r != nil {
						fmt.Fprintf(os.Stderr, "[Xiaozhi Audio Queue] Device: %s | Audio playback finished (logger panic recovered: %v)\n", esn, r)
					}
				}()
				logger.Println(fmt.Sprintf("[Xiaozhi Audio Queue] Device: %s | Audio playback finished", esn))
			}()
			return
		}
	}
}

// StreamingXiaozhiKG handles knowledge graph requests using xiaozhi WebSocket
// This provides real-time voice conversation with TTS audio playback on robot
// isConversationMode: if true, LLM will use {{newVoiceRequest||now}} to continue conversation
func StreamingXiaozhiKG(esn string, transcribedText string, isKG bool, isConversationMode bool) (string, error) {
	// Ensure esn is not empty to prevent panics in logger calls
	if esn == "" {
		esn = "unknown"
	}

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
			if err := xiaozhi.StoreConnection(deviceID, conn, sessionID); err != nil {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Failed to store connection: %v", esn, err))
				conn.Close()
				return "", fmt.Errorf("failed to store connection: %w", err)
			}
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

	textResponse := make(chan string, 5) // Increased buffer to handle both LLM and TTS sentence_start events
	errChan := make(chan error, 1)
	ttsStopChan := make(chan bool, 1) // Signal when TTS stops (for connection release timing)

	// Create LLM handler instance (vclient and opusDecoder will be set after audio client is created)
	llmHandler := &LLMHandler{
		textResponse:      textResponse,
		errChan:           errChan,
		ttsStopChan:       ttsStopChan,
		active:            true,
		audioChunkCount:   0,
		accumulatedBuffer: []byte{},
		chunkCount:        0,
		audioQueueStarted: false,
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
	var audioPrepareSent bool // Track if AudioStreamPrepare was sent successfully
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Checking robot connection status: robot == nil? %v", esn, robot == nil))
	if robot != nil {
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Creating audio playback client for robot...", esn))
		// Use audioCtx instead of ctx to prevent stream from closing when LLM request completes
		audioClient, err := robot.Conn.ExternalAudioStreamPlayback(audioCtx)
		if err != nil {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  WARNING - Failed to create audio playback client: %v. Continuing without audio playback.", esn, err))
			vclient = nil
			audioPrepareSent = false
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
				audioPrepareSent = false
			} else {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ AudioStreamPrepare sent successfully (8kHz, volume 100)", esn))
				audioPrepareSent = true
				// Add delay after AudioStreamPrepare to ensure robot is ready (like kgsim_cmds.go)
				// Increased delay to 100ms to ensure robot has processed the prepare message
				time.Sleep(time.Millisecond * 100)
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Delay after AudioStreamPrepare completed, robot should be ready", esn))
			}
		}
	} else {
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Robot %s not available (robot == nil), skipping audio playback setup. TTS/STT will still work.", esn, esn))
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Robot connection was not created. Possible reasons:", esn))
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s |   1. Robot not found in vars.BotInfo.Robots (count: %d)", esn, len(vars.BotInfo.Robots)))
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s |   2. Robot not found in vars.RecurringInfo (count: %d)", esn, len(vars.RecurringInfo)))
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s |   3. Missing IP address or GUID", esn))
		vclient = nil
		audioPrepareSent = false
	}

	// Setup audio processing in LLM handler (synchronous processing)
	if vclient != nil && audioPrepareSent {
		// Create Opus decoder for 24kHz, mono
		opusDecoder, err := opus.NewDecoder(24000, 1)
		if err != nil {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  WARNING - Failed to create Opus decoder: %v. Disabling audio playback.", esn, err))
			vclient = nil
		} else {
			// Set vclient and opusDecoder in LLM handler for synchronous processing
			llmHandler.mu.Lock()
			llmHandler.vclient = vclient
			llmHandler.opusDecoder = opusDecoder
			llmHandler.lastSendTime = time.Now()
			llmHandler.flushTimer = time.NewTicker(50 * time.Millisecond) // Flush buffer every 50ms
			llmHandler.flushTimerStop = make(chan bool, 1)
			llmHandler.mu.Unlock()
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Audio processing setup complete (synchronous mode)", esn))
			// Start audio queue
			WaitForAudio_Queue(esn)
			StartAudio_Queue(esn)
			llmHandler.mu.Lock()
			llmHandler.audioQueueStarted = true
			llmHandler.mu.Unlock()

			// Start flush timer goroutine to send small buffers periodically
			go func() {
				defer func() {
					llmHandler.mu.Lock()
					if llmHandler.flushTimer != nil {
						llmHandler.flushTimer.Stop()
					}
					llmHandler.mu.Unlock()
				}()
				// REMOVED: Timeout logic - only send AudioStreamComplete when server sends TTS stop event
				// This ensures we don't interrupt audio playback prematurely
				// Server will send TTS stop event when audio is complete
				for {
					select {
					case <-llmHandler.flushTimer.C:
						llmHandler.mu.Lock()
						vclient := llmHandler.vclient
						accumulatedBuffer := llmHandler.accumulatedBuffer
						lastSendTime := llmHandler.lastSendTime
						llmHandler.mu.Unlock()

						// Flush buffer if it has data and hasn't been sent for >50ms
						if len(accumulatedBuffer) > 0 && vclient != nil && time.Since(lastSendTime) > 50*time.Millisecond {
							chunkToSend := accumulatedBuffer
							err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
								AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamChunk{
									AudioStreamChunk: &vectorpb.ExternalAudioStreamChunk{
										AudioChunkSizeBytes: uint32(len(chunkToSend)),
										AudioChunkSamples:   chunkToSend,
									},
								},
							})
							if err != nil {
								if strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "closed") {
									llmHandler.mu.Lock()
									llmHandler.vclient = nil
									llmHandler.mu.Unlock()
									return
								}
							} else {
								llmHandler.mu.Lock()
								llmHandler.chunkCount++
								llmHandler.accumulatedBuffer = []byte{}
								llmHandler.lastSendTime = time.Now()
								llmHandler.mu.Unlock()
								time.Sleep(time.Millisecond * 60)
							}
						}
					case <-llmHandler.flushTimerStop:
						return
					}
				}
			}()
		}
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

	// Calculate shouldContinueConversation BEFORE starting audio processing goroutine
	// This allows us to trigger DoNewRequest immediately after AudioStreamComplete
	// NOTE: We'll update this when we receive the text response (to check for {{newVoiceRequest||now}})
	shouldContinueConversation := false
	if vars.APIConfig.Knowledge.SaveChat {
		shouldContinueConversation = true
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ SaveChat enabled - continuous conversation will be activated after audio", esn))
	} else if isConversationMode {
		shouldContinueConversation = true
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Conversation mode explicitly requested - continuous conversation will be activated after audio", esn))
	}

	// Audio processing is now synchronous - handled directly in LLM handler when receiving Opus frames
	// Setup DoNewRequest trigger after TTS stops (in a separate goroutine to avoid blocking)
	go func() {
		defer audioCancelSafe()    // Cancel audio context when done
		defer StopAudio_Queue(esn) // Mark audio as finished when done

		// Wait for TTS stop event
		select {
		case <-ttsStopChan:
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ TTS stop event received, preparing for continuous conversation...", esn))
		case <-time.After(30 * time.Second):
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⏱️  Timeout waiting for TTS stop (30s), proceeding anyway", esn))
		}

		// IMPORTANT: Wait for LLM handler to finish sending AudioStreamComplete AND robot to start playing audio
		// LLM handler needs time to:
		// 1. Sleep 1 second after TTS stop (wait for remaining frames)
		// 2. Send final buffer (if any)
		// 3. Send AudioStreamComplete
		// 4. Wait 500ms after AudioStreamComplete (for robot to start playing)
		// Total time needed: ~2-3 seconds
		// Then wait additional time for robot to finish playing audio (estimate based on chunks sent)
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⏳ Waiting for LLM handler to finish sending AudioStreamComplete and robot to start playing...", esn))
		time.Sleep(3 * time.Second) // Wait for LLM handler to complete audio sending and robot to start

		// Estimate audio duration: each chunk is ~60ms, wait for estimated playback time
		// Get chunk count to estimate duration
		llmHandler.mu.RLock()
		estimatedChunks := llmHandler.chunkCount
		llmHandler.mu.RUnlock()
		if estimatedChunks > 0 {
			// Estimate: each chunk ~60ms, add 2 seconds buffer for safety
			estimatedDuration := time.Duration(estimatedChunks) * 60 * time.Millisecond
			estimatedDuration += 2 * time.Second // Safety buffer
			if estimatedDuration > 3*time.Second {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⏳ Estimated audio duration: %v, waiting for robot to finish playing...", esn, estimatedDuration))
				time.Sleep(estimatedDuration - 3*time.Second) // Already waited 3 seconds above
			}
		}

		// IMPORTANT: Release connection after LLM handler finishes (before DoNewRequest)
		// This allows STT handler to use the connection as soon as robot starts speaking
		// Flow: TTS stop → Wait for AudioStreamComplete → Release connection → Activate STT → DoNewRequest → Robot speaks → STT uses connection
		if shouldContinueConversation && connFromSTT && deviceID != "" {
			// Deactivate LLM handler - connection manager's reader will route messages to STT handler
			// (giống botkct.py - không cần "release connection", chỉ cần deactivate handler)
			llmHandler.SetActive(false)
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM handler deactivated (connection manager will route to STT handler)", esn))

			// Note: ReleaseConnection is now no-op (giống botkct.py - connection luôn available)
			// Connection will be reused automatically when STT handler is active
			xiaozhi.ReleaseConnection(deviceID) // No-op, kept for clarity
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Connection ready for STT handler (connection always available, routing handles it)", esn))
		}

		// Trigger DoNewRequest if continuous conversation is enabled
		if shouldContinueConversation && robot != nil {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | 🎤 Starting continuous listening trigger...", esn))

			// Activate STT handler BEFORE calling DoNewRequest
			if vars.APIConfig.Knowledge.Provider == "xiaozhi" && deviceID != "" {
				if activated := xiaozhi.ActivateSTTHandler(deviceID); activated {
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ STT handler activated", esn))
				}
				// Send listen start message to xiaozhi server
				listenStart := map[string]interface{}{
					"type":  "listen",
					"state": "start",
					"mode":  "auto",
				}
				if err := xiaozhi.WriteJSON(deviceID, listenStart); err == nil {
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Listen start message sent to xiaozhi server", esn))
				}
			}

			// Call DoNewRequest 2 times with shorter delay between attempts
			maxAttempts := 2
			attemptInterval := 500 * time.Millisecond
			timeout := 10 * time.Second
			timeoutChan := time.After(timeout)

			for attempt := 1; attempt <= maxAttempts; attempt++ {
				// Check timeout
				select {
				case <-timeoutChan:
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⏱️  Timeout reached (%v), stopping DoNewRequest attempts", esn, timeout))
					return
				default:
				}

				// Check if robot is already listening
				if vars.APIConfig.Knowledge.Provider == "xiaozhi" && deviceID != "" {
					if xiaozhi.IsRobotListening(deviceID) {
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Robot is already listening, skipping DoNewRequest (attempt %d/%d)", esn, attempt, maxAttempts))
						return
					}
				}

				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | 📞 Attempt %d/%d: Calling DoNewRequest()...", esn, attempt, maxAttempts))
				DoNewRequest(robot)
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ DoNewRequest() called (attempt %d/%d)", esn, attempt, maxAttempts))

				// Wait and check if robot is listening
				time.Sleep(500 * time.Millisecond)
				if vars.APIConfig.Knowledge.Provider == "xiaozhi" && deviceID != "" {
					if xiaozhi.IsRobotListening(deviceID) {
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Robot is now listening, stopping attempts", esn))
						return
					}
					if xiaozhi.IsSTTHandlerActive(deviceID) {
						time.Sleep(1 * time.Second)
						if xiaozhi.IsRobotListening(deviceID) {
							logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Robot is now listening after additional wait, stopping attempts", esn))
							return
						}
					}
				}

				// If not last attempt, wait before next attempt
				if attempt < maxAttempts {
					select {
					case <-timeoutChan:
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⏱️  Timeout reached, stopping attempts", esn))
						return
					case <-time.After(attemptInterval):
						// Continue to next attempt
					}
				}
			}
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Completed all %d DoNewRequest attempts", esn, maxAttempts))
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

		// Wait for TTS stop event before releasing connection
		// Audio processing is now synchronous (handled directly in LLM handler), so we just wait for TTS stop
		// Keep WebSocket reader goroutine running continuously (like xiaozhi-esp32-main)
		if connFromSTT {
			// If continuous conversation is enabled, connection will be released in DoNewRequest goroutine
			// Otherwise, release it here after TTS stops
			if !shouldContinueConversation {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⏳ Waiting for TTS stop event before releasing connection...", esn))
				// Wait for TTS stop event (audio processing is synchronous, so it's already done when TTS stops)
				select {
				case <-ttsStopChan:
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ TTS stop event received, audio processing completed (synchronous)", esn))
					// Wait a bit for WebSocket reader goroutine to process remaining messages
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⏳ Waiting a bit more for server to send any remaining messages...", esn))
					time.Sleep(2 * time.Second) // Reduced wait time
					// Deactivate LLM handler - connection manager's reader will route messages to STT handler
					if deviceID != "" {
						llmHandler.SetActive(false)
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM handler deactivated (connection manager will route to STT handler)", esn))
					}
					// Release connection (don't close it) so STT can reuse for next request
					if deviceID != "" {
						xiaozhi.ReleaseConnection(deviceID)
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Connection released (STT handler will handle next request)", esn))
					}
				case <-time.After(120 * time.Second):
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Timeout waiting for TTS stop (120s), releasing connection anyway", esn))
					// Deactivate LLM handler - connection manager's reader will route messages to STT handler
					if deviceID != "" {
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
				// Continuous conversation enabled - connection is released IMMEDIATELY after TTS stop in goroutine
				// Don't wait here because ttsStopChan is already consumed by DoNewRequest goroutine
				// The goroutine handles: TTS stop → Release connection → Activate STT → DoNewRequest
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Continuous conversation enabled - connection release handled by DoNewRequest goroutine (released immediately after TTS stop)", esn))
			}
		} else {
			// For new connections, wait for TTS stop event
			select {
			case <-ttsStopChan:
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ TTS stop event received (synchronous audio processing)", esn))
			case <-time.After(10 * time.Second):
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Timeout waiting for TTS stop (10s), proceeding anyway", esn))
			}
		}

		// NOTE: Continuous conversation flow (in separate goroutine):
		// 1. TTS stop event received
		// 2. Release connection IMMEDIATELY (so STT can use it)
		// 3. Deactivate LLM handler (connection manager routes to STT handler)
		// 4. Activate STT handler
		// 5. Send listen start message to server
		// 6. Call DoNewRequest to open robot mic
		// 7. Robot speaks → STT handler uses released connection to send audio
		// This allows continuous conversation without needing "hey vector" each time

		// Don't cancel audioCtx here - let audio processing goroutine cancel it when done
		// This prevents vclient stream from closing while audio is still being sent
		return text, nil
	case <-time.After(30 * time.Second):
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ❌ TIMEOUT - No LLM response received after 30 seconds", esn))
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Debug info: textResponse channel length=%d, errChan length=%d", esn, len(textResponse), len(errChan)))

		// IMPORTANT: Deactivate LLM handler and release connection on timeout
		// This allows STT to reuse the connection for next request
		if deviceID != "" {
			// Deactivate LLM handler
			if connFromSTT {
				llmHandler.SetActive(false)
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM handler deactivated (timeout case)", esn))
			}
			// Release connection so STT can reuse it
			xiaozhi.ReleaseConnection(deviceID)
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Connection released (timeout case - STT can reuse)", esn))
		}

		// Stop audio queue if started
		llmHandler.mu.Lock()
		audioQueueStarted := llmHandler.audioQueueStarted
		llmHandler.mu.Unlock()
		if audioQueueStarted && esn != "" {
			StopAudio_Queue(esn)
		}

		// Cancel audioCtx on timeout (error path)
		audioCancelSafe()
		return "", fmt.Errorf("timeout waiting for response")
	case err := <-errChan:
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ❌ Error received from errChan: %v", esn, err))

		// IMPORTANT: Deactivate LLM handler and release connection on error
		// This allows STT to reuse the connection for next request
		if deviceID != "" {
			// Deactivate LLM handler
			if connFromSTT {
				llmHandler.SetActive(false)
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM handler deactivated (error case)", esn))
			}
			// Release connection so STT can reuse it
			xiaozhi.ReleaseConnection(deviceID)
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Connection released (error case - STT can reuse)", esn))
		}

		// Stop audio queue if started
		llmHandler.mu.Lock()
		audioQueueStarted := llmHandler.audioQueueStarted
		llmHandler.mu.Unlock()
		if audioQueueStarted && esn != "" {
			StopAudio_Queue(esn)
		}

		// Cancel audioCtx on error (error path)
		audioCancelSafe()
		return "", err
	}
}
