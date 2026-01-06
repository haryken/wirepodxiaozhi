package wirepod_ttr

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fforchino/vector-go-sdk/pkg/vector"
	"github.com/fforchino/vector-go-sdk/pkg/vectorpb"
	"github.com/gorilla/websocket"
	"github.com/kercre123/wire-pod/chipper/pkg/logger"
	"github.com/kercre123/wire-pod/chipper/pkg/vars"
	"github.com/kercre123/wire-pod/chipper/pkg/xiaozhi"
	opuslib "gopkg.in/hraban/opus.v2"
)

// AudioQueue manages audio playback serialization per robot
type AudioQueue struct {
	ESN                   string
	AudioDone             chan bool
	AudioCurrentlyPlaying bool
}

var AudioQueues []AudioQueue
var audioQueueMutex sync.Mutex

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
	ctx := context.Background()

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

	// Step 2: If not found, try to find IP from RecurringInfo
	if !matched {
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Robot not found in vars.BotInfo.Robots, checking RecurringInfo (count: %d)...", esn, len(vars.RecurringInfo)))
		for i, rinfo := range vars.RecurringInfo {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s |   RecurringInfo[%d]: ESN=%s, IP=%s, ID=%s", esn, i, rinfo.ESN, rinfo.IP, rinfo.ID))
			if rinfo.ESN == esn && rinfo.IP != "" {
				guid = vars.BotInfo.GlobalGUID // Use GlobalGUID if robot not in BotInfo
				target = rinfo.IP + ":443"
				matched = true
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Found robot in RecurringInfo (IP: %s, using GlobalGUID: %s)", esn, rinfo.IP, guid))
				break
			}
		}
		if !matched {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Robot %s not found in RecurringInfo either", esn, esn))
		}
	}

	// Step 3: Try to create robot connection if we have IP and GUID
	if matched && target != "" && guid != "" {
		var err error
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Attempting to create robot connection (ESN: %s, GUID: %s, Target: %s)", esn, esn, guid, target))
		robot, err = vector.New(vector.WithSerialNo(esn), vector.WithToken(guid), vector.WithTarget(target))
		if err != nil {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  WARNING - Failed to create robot connection: %v. Continuing without robot connection.", esn, err))
			robot = nil // Ensure robot is nil on error
		} else {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Robot connection created successfully", esn))
			// Test connection with battery state (non-blocking - don't fail if this errors)
			_, err = robot.Conn.BatteryState(ctx, &vectorpb.BatteryStateRequest{})
			if err != nil {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  WARNING - Failed to get battery state: %v. Robot connection may still work for audio playback.", esn, err))
				// Don't set robot to nil here - connection might still work for audio
			} else {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Robot connection verified (battery state OK)", esn))
			}
		}
	} else {
		if !matched {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Robot %s not found in vars.BotInfo.Robots or RecurringInfo. Available robots: %d", esn, esn, len(vars.BotInfo.Robots)))
			for i, bot := range vars.BotInfo.Robots {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s |   Robot[%d]: ESN=%s, IP=%s", esn, i, bot.Esn, bot.IPAddress))
			}
		} else {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Missing IP or GUID for robot %s (target: %s, guid: %s)", esn, esn, target, guid))
		}
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Continuing with xiaozhi TTS/STT without robot connection (audio playback will be skipped)", esn))
	}

	// Lấy Device-Id từ config
	deviceID := xiaozhi.GetDeviceIDFromConfig()

	// Bước 1: Thử lấy connection từ STT (giống botkct.py - dùng cùng connection cho STT và text message)
	var conn *websocket.Conn
	var sessionID string
	var connFromSTT bool

	if deviceID != "" {
		if storedConn, storedSessionID, exists := xiaozhi.GetConnection(deviceID); exists {
			conn = storedConn
			sessionID = storedSessionID
			connFromSTT = true
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
	}

	// Cleanup: Đóng connection sau khi xong (chỉ nếu không phải connection từ STT, hoặc đóng sau khi LLM xong)
	defer func() {
		if !connFromSTT {
			// Nếu là connection mới, đóng ngay
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | 🔌 Closing WebSocket connection (new connection)...", esn))
			if err := conn.Close(); err != nil {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Error closing WebSocket: %v", esn, err))
			} else {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ WebSocket closed successfully", esn))
			}
		} else {
			// Nếu là connection từ STT, xóa khỏi manager sau khi LLM xong
			if deviceID != "" {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | 🔌 Removing connection from manager (LLM finished)", esn))
				xiaozhi.RemoveConnection(deviceID)
			}
		}
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

	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Sending text query: %s", esn, transcribedText))
	textMessageJSON, _ := json.Marshal(textMessage)
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Text message JSON: %s", esn, string(textMessageJSON)))

	if err := conn.WriteJSON(textMessage); err != nil {
		return "", fmt.Errorf("failed to send text query: %w", err)
	}
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Text query sent successfully", esn))

	done := make(chan bool)
	audioChunks := make(chan []byte, 100)
	textResponse := make(chan string, 1)
	errChan := make(chan error, 1)

	// Setup audio playback client (only if robot connection exists)
	// Use the same pattern as kgsim_cmds.go - let Go infer the type
	var vclient interface {
		Send(*vectorpb.ExternalAudioStreamRequest) error
	}
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Checking robot connection status: robot == nil? %v", esn, robot == nil))
	if robot != nil {
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Creating audio playback client for robot...", esn))
		audioClient, err := robot.Conn.ExternalAudioStreamPlayback(ctx)
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
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ AudioStreamPrepare sent successfully (16kHz, volume 100)", esn))
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

	// Read messages from WebSocket
	go func() {
		defer func() {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | WebSocket reader goroutine exiting, closing channels", esn))
			close(audioChunks)
			close(textResponse)
			close(errChan)
		}()

		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ WebSocket reader goroutine started, waiting for messages...", esn))
		audioBuffer := []byte{}

		for {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⏳ Waiting for next message from server...", esn))
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				// Log chi tiết lỗi
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ❌ WebSocket ReadMessage ERROR", esn))
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Error: %v", esn, err))
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Error type: %T", esn, err))

				// Kiểm tra loại lỗi chi tiết
				if opErr, ok := err.(*net.OpError); ok {
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | OpError details:", esn))
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s |   Op: %s", esn, opErr.Op))
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s |   Net: %s", esn, opErr.Net))
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s |   Addr: %v", esn, opErr.Addr))
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s |   Err: %v", esn, opErr.Err))
					if opErr.Err != nil {
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s |   Err type: %T", esn, opErr.Err))
					}
				}

				// Kiểm tra close error
				if closeErr, ok := err.(*websocket.CloseError); ok {
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | CloseError details:", esn))
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s |   Code: %d", esn, closeErr.Code))
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s |   Text: %s", esn, closeErr.Text))
				}

				// Kiểm tra nếu là unexpected close
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ❌ UNEXPECTED WebSocket close error", esn))
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | This indicates the server closed the connection unexpectedly", esn))
					errChan <- fmt.Errorf("websocket unexpected close: %w", err)
				} else {
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ WebSocket closed normally or expected error", esn))
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | This is normal when connection is closed intentionally", esn))
				}
				return
			}
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Received message from server: type=%d, size=%d bytes", esn, messageType, len(message)))

			if messageType == websocket.TextMessage {
				var event map[string]interface{}
				if err := json.Unmarshal(message, &event); err != nil {
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ERROR - Failed to unmarshal message: %v", esn, err))
					continue
				}

				eventType, ok := event["type"].(string)
				if !ok {
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | WARNING - Message missing 'type' field", esn))
					continue
				}

				// Log all events for debugging
				eventJSON, _ := json.MarshalIndent(event, "", "  ")
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ========== EVENT RECEIVED ==========", esn))
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Event type: %s", esn, eventType))
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Event JSON:\n%s", esn, string(eventJSON)))
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ====================================", esn))

				switch eventType {
				case "stt":
					// STT transcription received
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | 🎤 STT EVENT received", esn))
					if text, ok := event["text"].(string); ok {
						logger.Println(fmt.Sprintf("[Xiaozhi STT] Device: %s | ✅ Transcribed text: %s", esn, text))
						// Log additional STT info if available
						if confidence, ok := event["confidence"].(float64); ok {
							logger.Println(fmt.Sprintf("[Xiaozhi STT] Device: %s | Confidence: %.2f", esn, confidence))
						}
						if duration, ok := event["duration"].(float64); ok {
							logger.Println(fmt.Sprintf("[Xiaozhi STT] Device: %s | Duration: %.2fs", esn, duration))
						}
						// Log all STT event fields
						for key, value := range event {
							if key != "type" && key != "text" && key != "confidence" && key != "duration" {
								logger.Println(fmt.Sprintf("[Xiaozhi STT] Device: %s | STT field %s: %v (type: %T)", esn, key, value, value))
							}
						}
					} else {
						logger.Println(fmt.Sprintf("[Xiaozhi STT] Device: %s | ⚠️  WARNING - STT event missing 'text' field", esn))
					}
				case "llm":
					// LLM response text
					logger.Println(fmt.Sprintf("[Xiaozhi LLM] Device: %s | ========== LLM EVENT RECEIVED ==========", esn))
					logger.Println(fmt.Sprintf("[Xiaozhi LLM] Device: %s | Full LLM event: %+v", esn, event))
					if text, ok := event["text"].(string); ok {
						logger.Println(fmt.Sprintf("[Xiaozhi LLM] Device: %s | ✅ LLM Response TEXT: '%s' (length: %d bytes)", esn, text, len(text)))
						// Log all LLM event fields
						for key, value := range event {
							if key != "type" && key != "text" {
								logger.Println(fmt.Sprintf("[Xiaozhi LLM] Device: %s | LLM field %s: %v (type: %T)", esn, key, value, value))
							}
						}
						// Store full LLM response text (may be sent in chunks)
						// We'll check for newVoiceRequest at the end when we have full response
						select {
						case textResponse <- text:
							logger.Println(fmt.Sprintf("[Xiaozhi LLM] Device: %s | ✅ LLM text sent to response channel: '%s'", esn, text))
						default:
							logger.Println(fmt.Sprintf("[Xiaozhi LLM] Device: %s | ⚠️  WARNING - textResponse channel full, dropping LLM text: '%s'", esn, text))
						}
						logger.Println(fmt.Sprintf("[Xiaozhi LLM] Device: %s | ========================================", esn))
					} else {
						logger.Println(fmt.Sprintf("[Xiaozhi LLM] Device: %s | ⚠️  WARNING - LLM event missing 'text' field", esn))
						keys := make([]string, 0, len(event))
						for k := range event {
							keys = append(keys, k)
						}
						logger.Println(fmt.Sprintf("[Xiaozhi LLM] Device: %s | LLM event keys: %v", esn, keys))
					}
				case "tts":
					// TTS state change
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | 🎤 TTS EVENT received", esn))
					if state, ok := event["state"].(string); ok {
						if state == "start" {
							audioBuffer = []byte{}
							logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ✅ TTS STARTED - Ready to receive audio", esn))
							// Log TTS text if available
							if text, ok := event["text"].(string); ok {
								logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | Text to speak: %s", esn, text))
							}
							// Log all TTS event fields
							for key, value := range event {
								if key != "type" && key != "state" && key != "text" {
									logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | TTS field %s: %v (type: %T)", esn, key, value, value))
								}
							}
						} else if state == "sentence_start" {
							// TTS sentence_start contains the full text response (giống botkct.py line 858-876)
							logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ========== TTS SENTENCE_START EVENT ==========", esn))
							if text, ok := event["text"].(string); ok && text != "" {
								logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ✅ TTS sentence_start TEXT: '%s' (length: %d bytes)", esn, text, len(text)))
								// Send text to response channel (this is the actual LLM response text)
								// Priority: TTS sentence_start text > LLM event text (vì TTS có text đầy đủ)
								select {
								case textResponse <- text:
									logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ✅ TTS sentence_start text sent to response channel: '%s'", esn, text))
								default:
									logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  WARNING - textResponse channel full, dropping TTS sentence_start text: '%s'", esn, text))
								}
								logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ==========================================", esn))
							} else {
								logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  WARNING - TTS sentence_start event missing 'text' field", esn))
							}
							// Log all TTS sentence_start event fields
							for key, value := range event {
								if key != "type" && key != "state" && key != "text" {
									logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | TTS sentence_start field %s: %v (type: %T)", esn, key, value, value))
								}
							}
						} else if state == "stop" {
							logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ✅ TTS STOPPED | Audio buffer size: %d bytes", esn, len(audioBuffer)))
							// Log all TTS stop event fields
							for key, value := range event {
								if key != "type" && key != "state" {
									logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | TTS stop field %s: %v (type: %T)", esn, key, value, value))
								}
							}
							// Send remaining audio
							if len(audioBuffer) > 0 {
								logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | Sending final audio buffer: %d bytes", esn, len(audioBuffer)))
								select {
								case audioChunks <- audioBuffer:
									logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ✅ Final audio chunk sent to channel: %d bytes", esn, len(audioBuffer)))
								default:
									logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  WARNING - Audio channel full, dropping final chunk", esn))
								}
							} else {
								logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  WARNING - TTS stopped but audio buffer is empty", esn))
							}
							done <- true
							logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ TTS complete, signaling done", esn))
							return
						} else {
							logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | Unknown TTS state: %s", esn, state))
						}
					} else {
						logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  WARNING - TTS event missing 'state' field", esn))
					}
				case "error":
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ❌ ERROR EVENT received from server", esn))
					if errMsg, ok := event["error"].(string); ok {
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ❌ Error message: %s", esn, errMsg))
						// Log all error event fields
						for key, value := range event {
							if key != "type" && key != "error" {
								logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Error field %s: %v (type: %T)", esn, key, value, value))
							}
						}
						errChan <- fmt.Errorf("xiaozhi error: %s", errMsg)
						return
					} else {
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  WARNING - Error event missing 'error' field", esn))
					}
				case "alert":
					// Alert from server (may contain error messages)
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  ALERT EVENT received from server", esn))
					// Log all alert event fields
					for key, value := range event {
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Alert field %s: %v (type: %T)", esn, key, value, value))
					}
					// Check if alert contains error message
					if message, ok := event["message"].(string); ok {
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Alert message: %s", esn, message))
						// If alert has status ERROR, treat it as an error
						if status, ok := event["status"].(string); ok && status == "ERROR" {
							logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ❌ Alert contains ERROR status, treating as error", esn))
							errChan <- fmt.Errorf("xiaozhi alert error: %s", message)
							return
						}
					}
				case "mcp":
					// MCP (Model Context Protocol) event - used for IoT/control commands
					// botkct.py (line 960-967) xử lý MCP event nhưng chỉ log, không làm gì đặc biệt
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | 🔧 MCP EVENT received", esn))
					if payload, ok := event["payload"].(map[string]interface{}); ok {
						if result, ok := payload["result"].(map[string]interface{}); ok {
							logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | 🔧 MCP Result: %+v", esn, result))
						} else if err, ok := payload["error"].(map[string]interface{}); ok {
							logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | 🔧 MCP Error: %+v", esn, err))
						} else if method, ok := payload["method"].(string); ok {
							logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | 🔧 MCP Method: %s (initialize - this is normal)", esn, method))
							// MCP initialize is normal - server is setting up MCP protocol
							// Continue waiting for LLM/TTS events
						}
					}
					// Log full MCP event for debugging
					mcpJSON, _ := json.MarshalIndent(event, "", "  ")
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | MCP Event JSON:\n%s", esn, string(mcpJSON)))
					// Continue waiting for LLM/TTS events (MCP is just a protocol setup)
				default:
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  WARNING - Unknown event type: %s", esn, eventType))
					// Log full event for debugging
					unknownJSON, _ := json.MarshalIndent(event, "", "  ")
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Unknown Event JSON:\n%s", esn, string(unknownJSON)))
				}
			} else if messageType == websocket.BinaryMessage {
				// Binary audio data (Opus encoded from xiaozhi server)
				// According to go-xiaozhi-main protocol, audio comes as Opus frames
				// These are already Opus-encoded and ready to decode
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | 🔊 BINARY AUDIO MESSAGE RECEIVED", esn))
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Audio chunk size: %d bytes", esn, len(message)))

				// Log first few bytes to identify format
				if len(message) >= 4 {
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | First 4 bytes: %02x %02x %02x %02x", esn, message[0], message[1], message[2], message[3]))
					// Check if it's OGG format (starts with "OggS")
					if message[0] == 0x4f && message[1] == 0x67 && message[2] == 0x67 && message[3] == 0x53 {
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Audio format: OGG container", esn))
					} else {
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Audio format: Raw OPUS frames (expected)", esn))
					}
				}

				// Log audio buffer status
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Audio buffer before append: %d bytes", esn, len(audioBuffer)))

				// Send Opus frames directly to audio channel
				// The audio player will decode them
				// Note: In production, you'd decode Opus → PCM → Downsample 24k→16k → Send
				// For now, we'll accumulate and send in chunks
				audioBuffer = append(audioBuffer, message...)
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Audio buffer after append: %d bytes", esn, len(audioBuffer)))

				// Process Opus frames (typically 20-60ms each, variable size)
				// Send immediately when we have any audio data (don't wait for large buffer)
				// This ensures real-time streaming without delay
				if len(audioBuffer) > 0 {
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Audio buffer has %d bytes, sending to audio channel immediately", esn, len(audioBuffer)))
					select {
					case audioChunks <- audioBuffer:
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Audio chunk sent to channel: %d bytes", esn, len(audioBuffer)))
						audioBuffer = []byte{}
					default:
						// Channel full, skip this chunk
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  WARNING - Audio channel full, dropping chunk", esn))
					}
				}
			} else {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  WARNING - Unknown message type: %d", esn, messageType))
			}
		}
	}()

	// Text query was already sent above (after hello response)
	// Now we wait for LLM response from the server

	// Play audio chunks to robot - Real-time streaming
	// Audio from xiaozhi is Opus-encoded at 24kHz (raw OPUS frames, not OGG)
	// Strategy: Receive OPUS chunk → Decode → Downsample → Send immediately (real-time)
	// Note: If robot is not available, skip audio processing but continue with text response
	go func() {
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
		opusDecoder, err := opuslib.NewDecoder(24000, 1)
		if err != nil {
			logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ERROR - Failed to create Opus decoder: %v", esn, err))
			return
		}
		logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | Opus decoder created, ready for real-time streaming", esn))

		// Process OPUS chunks in real-time - same approach as /api-sdk/play_sound
		// Simple and direct: decode → downsample → send with 60ms delay (like play_sound)
		for {
			select {
			case <-done:
				// TTS complete, send final completion
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
				return
			case chunk, ok := <-audioChunks:
				if !ok {
					// Channel closed
					logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | Audio channel closed, exiting", esn))
					return
				}
				if len(chunk) == 0 {
					logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  Empty audio chunk received, skipping", esn))
					continue
				}

				logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | 📥 Received audio chunk: %d bytes", esn, len(chunk)))

				if vclient == nil {
					logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  vclient is nil, skipping audio playback", esn))
					continue
				}

				// Decode OPUS → PCM
				pcmBuffer := make([]int16, 1440) // 60ms @ 24kHz max
				n, err := opusDecoder.Decode(chunk, pcmBuffer)
				if err != nil {
					logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  OPUS decode error: %v, skipping chunk", esn, err))
					continue
				}
				if n == 0 {
					logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  OPUS decode returned 0 samples, skipping", esn))
					continue
				}

				logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ✅ OPUS decoded: %d samples (24kHz)", esn, n))

				// Convert int16 → PCM bytes (little-endian)
				framePCMBytes := make([]byte, n*2)
				for i := 0; i < n; i++ {
					framePCMBytes[i*2] = byte(pcmBuffer[i])
					framePCMBytes[i*2+1] = byte(pcmBuffer[i] >> 8)
				}

				logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ✅ Converted to PCM: %d bytes", esn, len(framePCMBytes)))

				// Resample 24kHz → 8kHz (simple linear interpolation, like Play Audio)
				downsampledChunks := resample24kTo8kSimple(framePCMBytes)
				if len(downsampledChunks) == 0 {
					logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  Resample returned 0 chunks, skipping", esn))
					continue
				}

				logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ✅ Resampled to 8kHz: %d chunks", esn, len(downsampledChunks)))

				// Send chunks immediately with 60ms delay (same as /api-sdk/play_sound)
				chunkCount := 0
				for _, c := range downsampledChunks {
					if vclient == nil {
						logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  vclient became nil during sending, stopping", esn))
						break
					}

					// Pad to 1024 bytes if needed
					chunkToSend := c
					if len(c) < 1024 {
						paddedChunk := make([]byte, 1024)
						copy(paddedChunk, c)
						chunkToSend = paddedChunk
					} else if len(c) > 1024 {
						chunkToSend = c[:1024]
					}

					err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
						AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamChunk{
							AudioStreamChunk: &vectorpb.ExternalAudioStreamChunk{
								AudioChunkSizeBytes: 1024,
								AudioChunkSamples:   chunkToSend,
							},
						},
					})
					if err != nil {
						logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ⚠️  ERROR - Failed to send audio chunk: %v", esn, err))
						if strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "closed") {
							vclient = nil
							break
						}
					} else {
						chunkCount++
						if chunkCount == 1 || chunkCount%10 == 0 {
							logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ✅ Sent audio chunk %d to robot (1024 bytes)", esn, chunkCount))
						}
					}

					// Use 60ms delay (same as /api-sdk/play_sound which works well)
					time.Sleep(time.Millisecond * 60)
				}
				if chunkCount > 0 {
					logger.Println(fmt.Sprintf("[Xiaozhi TTS] Device: %s | ✅ Total chunks sent: %d", esn, chunkCount))
				}
			case err, ok := <-errChan:
				if !ok {
					return
				}
				if err != nil {
					logger.Println("Xiaozhi KG error: " + err.Error())
				}
				return
			}
		}
	}()

	// Wait for completion
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⏳ Waiting for LLM response (timeout: 30s)...", esn))
	select {
	case text := <-textResponse:
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ========== LLM TEXT RESPONSE RECEIVED ==========", esn))
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM response text: '%s'", esn, text))
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Text length: %d bytes", esn, len(text)))
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Text will be returned to caller", esn))
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ==============================================", esn))
		// Wait a bit for audio to finish
		time.Sleep(2 * time.Second)

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

		return text, nil
	case <-time.After(30 * time.Second):
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ❌ TIMEOUT - No LLM response received after 30 seconds", esn))
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Debug info: textResponse channel length=%d, errChan length=%d", esn, len(textResponse), len(errChan)))
		return "", fmt.Errorf("timeout waiting for response")
	case err := <-errChan:
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ❌ Error received from errChan: %v", esn, err))
		return "", err
	}
}
