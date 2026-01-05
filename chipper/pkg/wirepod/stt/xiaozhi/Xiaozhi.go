package wirepod_vosk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kercre123/wire-pod/chipper/pkg/logger"
	"github.com/kercre123/wire-pod/chipper/pkg/vars"
	"github.com/kercre123/wire-pod/chipper/pkg/xiaozhi"
	sr "github.com/kercre123/wire-pod/chipper/pkg/wirepod/speechrequest"
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

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, baseURL, nil)
	if err != nil {
		logger.Println("Xiaozhi STT: Failed to connect:", err)
		return "", fmt.Errorf("failed to connect to xiaozhi: %w", err)
	}
	defer conn.Close()

	// Step 1: Send hello event (following xiaozhi protocol from go-xiaozhi-main)
	helloEvent := map[string]interface{}{
		"type":    "hello",
		"version": 1,
		"audio_params": map[string]interface{}{
			"format":        "opus",
			"sample_rate":  16000,
			"channels":      1,
			"frame_duration": 20,
		},
	}
	if err := conn.WriteJSON(helloEvent); err != nil {
		return "", fmt.Errorf("failed to send hello: %w", err)
	}

	// Step 2: Read hello response
	var helloResp map[string]interface{}
	if err := conn.ReadJSON(&helloResp); err != nil {
		return "", fmt.Errorf("failed to read hello response: %w", err)
	}
	logger.Println("Xiaozhi STT: Hello response received")

	// Step 3: Send listen start event
	listenStart := map[string]interface{}{
		"type":  "listen",
		"state": "start",
		"mode":  "auto",
	}
	if err := conn.WriteJSON(listenStart); err != nil {
		return "", fmt.Errorf("failed to send listen start: %w", err)
	}

	// Step 4: Setup channels for async communication
	done := make(chan bool)
	transcriptChan := make(chan string, 1)
	errChan := make(chan error, 1)

	// Step 5: Read messages from WebSocket (following xiaozhi protocol)
	go func() {
		defer close(transcriptChan)
		defer close(errChan)

		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					errChan <- fmt.Errorf("websocket error: %w", err)
				}
				return
			}

			if messageType == websocket.TextMessage {
				var event map[string]interface{}
				if err := json.Unmarshal(message, &event); err != nil {
					continue
				}

				// Check event type (following xiaozhi protocol from go-xiaozhi-main)
				if eventType, ok := event["type"].(string); ok {
					switch eventType {
					case "stt":
						// STT event: {'type': 'stt', 'text': 'are youOK。', 'session_id': '9842a257'}
						if text, ok := event["text"].(string); ok && text != "" {
							select {
							case transcriptChan <- text:
								logger.Println("Xiaozhi STT: Received transcript: " + text)
							default:
							}
						}
					case "listen":
						// Listen state change
						if state, ok := event["state"].(string); ok && state == "stop" {
							done <- true
							return
						}
					case "error":
						// Error event
						if errMsg, ok := event["error"].(string); ok {
							errChan <- fmt.Errorf("xiaozhi error: %s", errMsg)
							return
						}
					}
				}
			}
		}
	}()

	// Step 6: Send audio chunks using append.buffer event (following xiaozhi protocol)
	go func() {
		defer func() {
			// Send listen stop when done
			listenStop := map[string]interface{}{
				"type":  "listen",
				"state": "stop",
			}
			conn.WriteJSON(listenStop)
		}()

		for {
			select {
			case <-done:
				return
			default:
				chunk, err := sreq.GetNextStreamChunkOpus()
				if err != nil {
					if err == io.EOF {
						done <- true
						return
					}
					errChan <- err
					return
				}

				// Send binary audio data directly (xiaozhi protocol accepts binary Opus)
				// According to go-xiaozhi-main, audio is sent as binary messages
				// after listen start event
				if err := conn.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
					errChan <- fmt.Errorf("failed to send audio: %w", err)
					return
				}

				speechDone, _ := sreq.DetectEndOfSpeech()
				if speechDone {
					// Wait a bit for server to process
					time.Sleep(500 * time.Millisecond)
					done <- true
					return
				}
			}
		}
	}()

	// Step 7: Wait for transcript or error
	select {
	case transcript := <-transcriptChan:
		logger.Println("Bot " + sreq.Device + " Transcribed text: " + transcript)
		return transcript, nil
	case err := <-errChan:
		return "", err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(30 * time.Second):
		return "", fmt.Errorf("timeout waiting for transcript")
	}
}
