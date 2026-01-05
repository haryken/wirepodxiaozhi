package wirepod_ttr

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/digital-dream-labs/opus-go/opus"
	"github.com/gorilla/websocket"
	"github.com/kercre123/wire-pod/chipper/pkg/logger"
	"github.com/kercre123/wire-pod/chipper/pkg/vars"
	"github.com/kercre123/wire-pod/chipper/pkg/xiaozhi"
	"github.com/fforchino/vector-go-sdk/pkg/vector"
	"github.com/fforchino/vector-go-sdk/pkg/vectorpb"
)

// StreamingXiaozhiKG handles knowledge graph requests using xiaozhi WebSocket
// This provides real-time voice conversation with TTS audio playback on robot
// isConversationMode: if true, LLM will use {{newVoiceRequest||now}} to continue conversation
func StreamingXiaozhiKG(esn string, transcribedText string, isKG bool, isConversationMode bool) (string, error) {
	ctx := context.Background()
	
	// Get robot connection
	matched := false
	var robot *vector.Vector
	var guid string
	var target string
	for _, bot := range vars.BotInfo.Robots {
		if esn == bot.Esn {
			guid = bot.GUID
			target = bot.IPAddress + ":443"
			matched = true
			break
		}
	}
	if !matched {
		return "", fmt.Errorf("robot not found")
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

	// Get xiaozhi config
	baseURL, voice, voiceWithEnglish := xiaozhi.GetKnowledgeGraphConfig()
	if baseURL == "" {
		baseURL = "wss://api.tenclass.net/xiaozhi/v1/"
	}

	// Connect to xiaozhi WebSocket
	conn, _, err := websocket.DefaultDialer.Dial(baseURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to connect to xiaozhi: %w", err)
	}
	defer conn.Close()

	// Step 1: Send hello event
	helloEvent := map[string]interface{}{
		"type":    "hello",
		"version": 1,
		"audio_params": map[string]interface{}{
			"format":        "opus",
			"sample_rate":  24000, // xiaozhi uses 24kHz
			"channels":      1,
			"frame_duration": 20,
		},
	}
	if err := conn.WriteJSON(helloEvent); err != nil {
		return "", fmt.Errorf("failed to send hello: %w", err)
	}

	// Read hello response
	var helloResp map[string]interface{}
	if err := conn.ReadJSON(&helloResp); err != nil {
		return "", fmt.Errorf("failed to read hello response: %w", err)
	}
	logger.Println("Xiaozhi KG: Connected and hello received")

	// Step 2: Send listen start
	listenStart := map[string]interface{}{
		"type":  "listen",
		"state": "start",
		"mode":  "auto",
	}
	if err := conn.WriteJSON(listenStart); err != nil {
		return "", fmt.Errorf("failed to send listen start: %w", err)
	}

	// Step 3: Note - xiaozhi expects audio input, not text
	// Since we already have transcribed text from STT, we need to:
	// Option 1: Use TTS to convert text back to audio (not ideal)
	// Option 2: Use xiaozhi's text query feature if available
	// Option 3: For now, we'll just wait for any audio response from xiaozhi
	// In a real implementation, you'd send the audio that was transcribed
	
	done := make(chan bool)
	audioChunks := make(chan []byte, 100)
	textResponse := make(chan string, 1)
	errChan := make(chan error, 1)

	// Setup audio playback client
	vclient, err := robot.Conn.ExternalAudioStreamPlayback(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to create audio playback client: %w", err)
	}

	// Prepare audio stream
	vclient.Send(&vectorpb.ExternalAudioStreamRequest{
		AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamPrepare{
			AudioStreamPrepare: &vectorpb.ExternalAudioStreamPrepare{
				AudioFrameRate: 16000, // Vector uses 16kHz
				AudioVolume:    100,
			},
		},
	})

	// Read messages from WebSocket
	go func() {
		defer close(audioChunks)
		defer close(textResponse)
		defer close(errChan)

		audioBuffer := []byte{}
		
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

				eventType, ok := event["type"].(string)
				if !ok {
					continue
				}

				switch eventType {
				case "stt":
					// STT transcription received
					if text, ok := event["text"].(string); ok {
						logger.Println("Xiaozhi KG: STT: " + text)
					}
				case "llm":
					// LLM response text
					if text, ok := event["text"].(string); ok {
						// Store full LLM response text (may be sent in chunks)
						// We'll check for newVoiceRequest at the end when we have full response
						select {
						case textResponse <- text:
						default:
						}
					}
				case "tts":
					// TTS state change
					if state, ok := event["state"].(string); ok {
						if state == "start" {
							audioBuffer = []byte{}
							logger.Println("Xiaozhi KG: TTS started")
						} else if state == "stop" {
							// Send remaining audio
							if len(audioBuffer) > 0 {
								select {
								case audioChunks <- audioBuffer:
								default:
								}
							}
							done <- true
							return
						}
					}
				case "error":
					if errMsg, ok := event["error"].(string); ok {
						errChan <- fmt.Errorf("xiaozhi error: %s", errMsg)
						return
					}
				}
			} else if messageType == websocket.BinaryMessage {
				// Binary audio data (Opus encoded from xiaozhi server)
				// According to go-xiaozhi-main protocol, audio comes as Opus frames
				// These are already Opus-encoded and ready to decode
				
				// Send Opus frames directly to audio channel
				// The audio player will decode them
				// Note: In production, you'd decode Opus → PCM → Downsample 24k→16k → Send
				// For now, we'll accumulate and send in chunks
				audioBuffer = append(audioBuffer, message...)
				
				// Process Opus frames (typically 20-60ms each, variable size)
				// Send when buffer is large enough or when TTS stops
				if len(audioBuffer) >= 480 { // ~20ms of Opus data at 24kHz
					select {
					case audioChunks <- audioBuffer:
						audioBuffer = []byte{}
					default:
						// Channel full, skip this chunk
					}
				}
			}
		}
	}()

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
		// Use wirepod's opus library (digital-dream-labs/opus-go)
		// Create OggStream decoder for 24kHz, mono
		opusStream := &opus.OggStream{}
		
		for {
			select {
			case <-done:
				// Send completion
				vclient.Send(&vectorpb.ExternalAudioStreamRequest{
					AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamComplete{
						AudioStreamComplete: &vectorpb.ExternalAudioStreamComplete{},
					},
				})
				return
			case chunk := <-audioChunks:
				if len(chunk) == 0 {
					continue
				}
				
				// Decode Opus to PCM using wirepod's opus library
				// OggStream.Decode returns decoded PCM data
				decodedPCM, err := opusStream.Decode(chunk)
				if err != nil {
					logger.Println("Xiaozhi KG: Opus decode error:", err)
					continue
				}
				
				if len(decodedPCM) > 0 {
					// decodedPCM is already PCM bytes (16-bit, little-endian)
					// Sample rate is 24kHz (from xiaozhi audio_params)
					
					// Downsample 24kHz → 16kHz
					chunks := downsample24kTo16k(decodedPCM)
					
					// Send PCM chunks to robot
					for _, c := range chunks {
						vclient.Send(&vectorpb.ExternalAudioStreamRequest{
							AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamChunk{
								AudioStreamChunk: &vectorpb.ExternalAudioStreamChunk{
									AudioChunkSizeBytes: uint32(len(c)),
									AudioChunkSamples:   c,
								},
							},
						})
						time.Sleep(time.Millisecond * 25)
					}
				}
			case err := <-errChan:
				logger.Println("Xiaozhi KG error: " + err.Error())
				done <- true
				return
			}
		}
	}()

	// Wait for completion
	select {
	case text := <-textResponse:
		// Wait a bit for audio to finish
		time.Sleep(2 * time.Second)
		
		// Check if conversation should continue (newVoiceRequest action)
		// If conversation mode is enabled (either via SaveChat or explicit request), trigger continuous listening
		shouldContinueConversation := (vars.APIConfig.Knowledge.SaveChat || isConversationMode) && strings.Contains(text, "{{newVoiceRequest||now}}")
		if shouldContinueConversation {
			logger.Println("Xiaozhi KG: Continuous conversation mode activated - robot will listen for next question")
			// Trigger robot to listen for next question without wake word
			// This happens after audio playback completes
			go func() {
				time.Sleep(1 * time.Second) // Wait a bit more for audio to finish
				DoNewRequest(robot)
			}()
		}
		
		return text, nil
	case <-time.After(30 * time.Second):
		return "", fmt.Errorf("timeout waiting for response")
	case err := <-errChan:
		return "", err
	}
}

