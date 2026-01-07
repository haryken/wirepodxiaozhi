package xiaozhi

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kercre123/wire-pod/chipper/pkg/logger"
)

// MessageHandler handles messages from WebSocket connection
type MessageHandler interface {
	HandleMessage(messageType int, message []byte) error
	IsActive() bool
	SetActive(active bool)
}

// ConnectionInfo stores connection information and message handlers
type ConnectionInfo struct {
	Conn          *websocket.Conn
	SessionID     string
	InUse         bool
	STTHandler    MessageHandler
	LLMHandler    MessageHandler
	ReaderRunning bool
	ReaderStop    chan struct{}
	mu            sync.RWMutex // For reading ConnectionInfo fields
	writeMu       sync.Mutex   // For serializing writes to WebSocket connection (WebSocket is not thread-safe for concurrent writes)
}

// ConnectionManager manages WebSocket connections per device
// This allows STT and LLM to reuse the same connection (like botkct.py)
// Uses a single reader goroutine per connection (like go-xiaozhi-main)
type ConnectionManager struct {
	connections map[string]*ConnectionInfo
	mu          sync.RWMutex
}

var connManager = &ConnectionManager{
	connections: make(map[string]*ConnectionInfo),
}

// StartReader starts a single reader goroutine for a connection (like go-xiaozhi-main)
// This reader runs continuously and routes messages to the appropriate handler
func StartReader(deviceID string, conn *websocket.Conn, sessionID string) {
	connManager.mu.Lock()
	connInfo, exists := connManager.connections[deviceID]
	if !exists {
		connInfo = &ConnectionInfo{
			Conn:          conn,
			SessionID:     sessionID,
			InUse:         false,
			ReaderRunning: false,
			ReaderStop:    make(chan struct{}),
		}
		connManager.connections[deviceID] = connInfo
	} else {
		// Update connection info
		connInfo.Conn = conn
		connInfo.SessionID = sessionID
		connInfo.ReaderStop = make(chan struct{})
	}
	connManager.mu.Unlock()

	// Start reader goroutine if not already running
	connInfo.mu.Lock()
	if connInfo.ReaderRunning {
		connInfo.mu.Unlock()
		logger.Println(fmt.Sprintf("[ConnectionManager] Reader already running for device: %s", deviceID))
		return
	}
	connInfo.ReaderRunning = true
	connInfo.mu.Unlock()

	logger.Println(fmt.Sprintf("[ConnectionManager] Starting single reader goroutine for device: %s (sessionID: %s)", deviceID, sessionID))

	// Set PongHandler to automatically respond to server pings
	// This is important to keep connection alive - server may send ping and expect pong
	conn.SetPongHandler(func(appData string) error {
		logger.Println(fmt.Sprintf("[ConnectionManager] ✅ Received pong from server for device %s", deviceID))
		return nil
	})

	// Set PingHandler to automatically respond to server pings (if server sends ping)
	// Note: Usually server sends ping and expects pong, but we also set this just in case
	// Use writeMu to serialize writes
	conn.SetPingHandler(func(appData string) error {
		logger.Println(fmt.Sprintf("[ConnectionManager] ✅ Received ping from server for device %s, sending pong", deviceID))
		// Respond with pong - use writeMu to serialize writes
		connInfo.writeMu.Lock()
		defer connInfo.writeMu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		err := conn.WriteMessage(websocket.PongMessage, []byte(appData))
		conn.SetWriteDeadline(time.Time{})
		return err
	})

	go func() {
		defer func() {
			connInfo.mu.Lock()
			connInfo.ReaderRunning = false
			connInfo.mu.Unlock()
			logger.Println(fmt.Sprintf("[ConnectionManager] Reader goroutine stopped for device: %s", deviceID))
		}()

		// Start ping ticker to keep connection alive (prevent server timeout)
		// Use 1 second like go-xiaozhi-main to keep connection very active
		// Send ping frequently to prevent server from closing idle connection
		pingTicker := time.NewTicker(1 * time.Second)
		defer pingTicker.Stop()

		// Ping goroutine to keep connection alive
		go func() {
			for {
				select {
				case <-pingTicker.C:
					connInfo.mu.RLock()
					connToPing := connInfo.Conn
					connInfo.mu.RUnlock()
					if connToPing != nil && connToPing.RemoteAddr() != nil {
						// Send ping to keep connection alive - use writeMu to serialize writes
						connInfo.writeMu.Lock()
						connToPing.SetWriteDeadline(time.Now().Add(5 * time.Second))
						err := connToPing.WriteMessage(websocket.PingMessage, nil)
						connToPing.SetWriteDeadline(time.Time{}) // Clear deadline
						connInfo.writeMu.Unlock()
						if err != nil {
							logger.Println(fmt.Sprintf("[ConnectionManager] Failed to send ping to device %s: %v", deviceID, err))
						} else {
							logger.Println(fmt.Sprintf("[ConnectionManager] ✅ Ping sent to device %s to keep connection alive", deviceID))
						}
					}
				case <-connInfo.ReaderStop:
					return
				}
			}
		}()

		for {
			// Check if reader should stop (non-blocking check)
			select {
			case <-connInfo.ReaderStop:
				logger.Println(fmt.Sprintf("[ConnectionManager] Reader stop signal received for device: %s", deviceID))
				return
			default:
			}

			// Read message - blocking read like go-xiaozhi-main (no SetReadDeadline, no recover)
			// go-xiaozhi-main pattern: msgType, msg, merr := w.conn.ReadMessage()
			// if merr != nil { w.done <- struct{}{}; break }
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				// Error occurred - stop reader like go-xiaozhi-main
				// Check if this is a graceful close (websocket: close 1005) during active LLM session
				// If LLM is active and audio is playing, wait a bit before removing connection
				// This gives time for audio to finish playing
				connInfo.mu.RLock()
				inUse := connInfo.InUse
				llmHandler := connInfo.LLMHandler
				connInfo.mu.RUnlock()
				
				// If connection is in use by LLM, wait longer before removing (audio might still be playing)
				waitTime := 100 * time.Millisecond
				if inUse && llmHandler != nil {
					// LLM is active - wait longer to allow audio to finish
					waitTime = 2 * time.Second
					logger.Println(fmt.Sprintf("[ConnectionManager] Read error for device %s during active LLM session: %v, will wait %v before removing connection", deviceID, err, waitTime))
				} else {
					logger.Println(fmt.Sprintf("[ConnectionManager] Read error for device %s: %v, stopping reader", deviceID, err))
				}
				
				connInfo.mu.Lock()
				connInfo.ReaderRunning = false
				connInfo.mu.Unlock()
				// Remove connection from manager after wait time
				go func() {
					time.Sleep(waitTime)
					connManager.mu.Lock()
					delete(connManager.connections, deviceID)
					connManager.mu.Unlock()
					logger.Println(fmt.Sprintf("[ConnectionManager] Connection removed from manager for device %s (STT will create new connection when needed)", deviceID))
				}()
				return
			}

			// Route message to appropriate handler
			connInfo.mu.RLock()
			sttHandler := connInfo.STTHandler
			llmHandler := connInfo.LLMHandler
			inUse := connInfo.InUse
			connInfo.mu.RUnlock()

			// Route to LLM handler if active and in use
			if inUse && llmHandler != nil && llmHandler.IsActive() {
				if err := llmHandler.HandleMessage(messageType, message); err != nil {
					logger.Println(fmt.Sprintf("[ConnectionManager] LLM handler error for device %s: %v", deviceID, err))
				}
				continue
			}

			// Route to STT handler if active
			if sttHandler != nil && sttHandler.IsActive() {
				if err := sttHandler.HandleMessage(messageType, message); err != nil {
					logger.Println(fmt.Sprintf("[ConnectionManager] STT handler error for device %s: %v", deviceID, err))
				}
				continue
			}

			// No active handler - log and continue (message will be ignored)
			if messageType == websocket.TextMessage {
				var event map[string]interface{}
				if err := json.Unmarshal(message, &event); err == nil {
					eventType, _ := event["type"].(string)
					logger.Println(fmt.Sprintf("[ConnectionManager] No active handler for device %s, ignoring message type: %s", deviceID, eventType))
				}
			}
		}
	}()
}

// SetSTTHandler sets the STT message handler for a device
func SetSTTHandler(deviceID string, handler MessageHandler) {
	connManager.mu.Lock()
	defer connManager.mu.Unlock()

	if connInfo, exists := connManager.connections[deviceID]; exists {
		connInfo.mu.Lock()
		// Clear old handler if exists
		if connInfo.STTHandler != nil {
			connInfo.STTHandler.SetActive(false)
		}
		// Set new handler and ensure it's active
		connInfo.STTHandler = handler
		if handler != nil {
			handler.SetActive(true)
		}
		connInfo.mu.Unlock()
		logger.Println(fmt.Sprintf("[ConnectionManager] STT handler set for device: %s (active: true)", deviceID))
	}
}

// SetLLMHandler sets the LLM message handler for a device
func SetLLMHandler(deviceID string, handler MessageHandler) {
	connManager.mu.Lock()
	defer connManager.mu.Unlock()

	if connInfo, exists := connManager.connections[deviceID]; exists {
		connInfo.mu.Lock()
		connInfo.LLMHandler = handler
		connInfo.mu.Unlock()
		logger.Println(fmt.Sprintf("[ConnectionManager] LLM handler set for device: %s", deviceID))
	}
}

// StoreConnection stores a WebSocket connection for a device
// This is called by STT after establishing connection
// Starts the single reader goroutine (like go-xiaozhi-main)
// Returns error if old connection is in use by LLM
func StoreConnection(deviceID string, conn *websocket.Conn, sessionID string) error {
	connManager.mu.Lock()
	defer connManager.mu.Unlock()

	// Close old connection if exists, BUT NOT if it's currently in use by LLM
	if oldConnInfo, exists := connManager.connections[deviceID]; exists && oldConnInfo.Conn != nil {
		// Check if old connection is in use
		oldConnInfo.mu.RLock()
		inUse := oldConnInfo.InUse
		oldConnInfo.mu.RUnlock()
		
		if inUse {
			// Old connection is in use by LLM, don't close it
			// This prevents interrupting LLM audio playback
			logger.Println(fmt.Sprintf("[ConnectionManager] ⚠️  Old connection for device %s is IN USE by LLM, cannot replace it. New connection will not be stored.", deviceID))
			// Close the new connection since we can't use it
			conn.Close()
			return fmt.Errorf("connection for device %s is in use by LLM, cannot store new connection", deviceID)
		}
		
		logger.Println(fmt.Sprintf("[ConnectionManager] Closing old connection for device: %s", deviceID))
		// Stop old reader
		if oldConnInfo.ReaderRunning {
			select {
			case oldConnInfo.ReaderStop <- struct{}{}:
			default:
			}
		}
		oldConnInfo.Conn.Close()
	}

	connInfo := &ConnectionInfo{
		Conn:          conn,
		SessionID:     sessionID,
		InUse:         false,
		ReaderRunning: false,
		ReaderStop:    make(chan struct{}),
	}
	connManager.connections[deviceID] = connInfo
	logger.Println(fmt.Sprintf("[ConnectionManager] Stored connection for device: %s, sessionID: %s", deviceID, sessionID))

	// Start reader goroutine (like go-xiaozhi-main)
	go StartReader(deviceID, conn, sessionID)
	return nil
}

// IsConnectionValid checks if a WebSocket connection is still valid
// This is a basic check - actual validity should be verified by trying to use the connection
func IsConnectionValid(conn *websocket.Conn) bool {
	if conn == nil {
		return false
	}
	// Check if connection is still alive by checking remote address
	// If connection is closed, RemoteAddr() will return nil
	// Note: This is a basic check - connection might still be closed even if RemoteAddr() != nil
	// The actual validity should be verified by trying to send/receive data
	return conn.RemoteAddr() != nil
}

// GetConnection retrieves a WebSocket connection for a device
// Returns connection, sessionID, and whether connection exists and is valid
// NOTE: This marks the connection as "in use" - caller should call ReleaseConnection when done
func GetConnection(deviceID string) (*websocket.Conn, string, bool) {
	connManager.mu.Lock()
	defer connManager.mu.Unlock()

	connInfo, exists := connManager.connections[deviceID]
	if !exists {
		return nil, "", false
	}

	conn := connInfo.Conn
	sessionID := connInfo.SessionID
	inUse := connInfo.InUse

	// Check if connection exists, is still valid, and NOT currently in use
	if exists && IsConnectionValid(conn) && !inUse {
		// Mark as in use
		connInfo.InUse = true
		logger.Println(fmt.Sprintf("[ConnectionManager] Connection for device %s marked as IN USE", deviceID))
		return conn, sessionID, true
	}

	// Connection exists but is invalid or in use
	if exists && conn != nil {
		if inUse {
			logger.Println(fmt.Sprintf("[ConnectionManager] Connection for device %s exists but is IN USE by another request, cannot reuse", deviceID))
		} else {
			logger.Println(fmt.Sprintf("[ConnectionManager] Connection for device %s exists but is invalid, will be removed", deviceID))
		}
	}

	return nil, "", false
}

// ReleaseConnection marks a connection as no longer in use
// This should be called when LLM finishes using the connection
func ReleaseConnection(deviceID string) {
	connManager.mu.Lock()
	defer connManager.mu.Unlock()

	if connInfo, exists := connManager.connections[deviceID]; exists {
		connInfo.InUse = false
		logger.Println(fmt.Sprintf("[ConnectionManager] Connection for device %s released (no longer in use)", deviceID))
	}
}

// CheckConnection checks if a connection exists and can be reused (not in use)
// This is used by STT to check if it can reuse a connection without marking it as in use
func CheckConnection(deviceID string) (*websocket.Conn, string, bool) {
	connManager.mu.RLock()
	defer connManager.mu.RUnlock()

	connInfo, exists := connManager.connections[deviceID]
	if !exists {
		logger.Println(fmt.Sprintf("[ConnectionManager] CheckConnection: No connection found for device %s", deviceID))
		return nil, "", false
	}

	conn := connInfo.Conn
	sessionID := connInfo.SessionID
	inUse := connInfo.InUse
	readerRunning := connInfo.ReaderRunning

	// Log detailed status for debugging
	logger.Println(fmt.Sprintf("[ConnectionManager] CheckConnection for device %s: exists=%v, conn!=nil=%v, valid=%v, inUse=%v, readerRunning=%v",
		deviceID, exists, conn != nil, conn != nil && IsConnectionValid(conn), inUse, readerRunning))

	// Only return connection if it exists, is valid, NOT in use, and reader is running
	// If Conn is nil, it means connection has been marked as failed
	if exists && conn != nil && IsConnectionValid(conn) && !inUse && readerRunning {
		logger.Println(fmt.Sprintf("[ConnectionManager] CheckConnection: ✅ Connection available for reuse (device: %s, sessionID: %s)", deviceID, sessionID))
		return conn, sessionID, true
	}

	// Log why connection cannot be reused
	if conn == nil {
		logger.Println(fmt.Sprintf("[ConnectionManager] CheckConnection: ❌ Connection is nil (marked as failed) for device %s", deviceID))
	} else if !IsConnectionValid(conn) {
		logger.Println(fmt.Sprintf("[ConnectionManager] CheckConnection: ❌ Connection is invalid (RemoteAddr is nil) for device %s", deviceID))
	} else if inUse {
		logger.Println(fmt.Sprintf("[ConnectionManager] CheckConnection: ❌ Connection is IN USE for device %s", deviceID))
	} else if !readerRunning {
		logger.Println(fmt.Sprintf("[ConnectionManager] CheckConnection: ❌ Reader goroutine is NOT RUNNING for device %s", deviceID))
	}

	return nil, "", false
}

// IsConnectionInUse checks if a connection is currently being used
func IsConnectionInUse(deviceID string) bool {
	connManager.mu.RLock()
	defer connManager.mu.RUnlock()

	if connInfo, exists := connManager.connections[deviceID]; exists {
		return connInfo.InUse
	}
	return false
}

// IsReaderRunning checks if the reader goroutine is still running for a connection
func IsReaderRunning(deviceID string) bool {
	connManager.mu.RLock()
	defer connManager.mu.RUnlock()

	if connInfo, exists := connManager.connections[deviceID]; exists {
		return connInfo.ReaderRunning
	}
	return false
}

// CheckConnectionExists checks if a connection exists and is valid (regardless of in-use status)
// This is used by STT to check if connection exists before deciding to wait or create new one
func CheckConnectionExists(deviceID string) (*websocket.Conn, string, bool) {
	connManager.mu.RLock()
	defer connManager.mu.RUnlock()

	connInfo, exists := connManager.connections[deviceID]
	if !exists {
		return nil, "", false
	}

	conn := connInfo.Conn
	sessionID := connInfo.SessionID
	readerRunning := connInfo.ReaderRunning

	// Return connection if it exists, is valid, and reader is running (regardless of in-use status)
	if conn != nil && IsConnectionValid(conn) && readerRunning {
		return conn, sessionID, true
	}

	return nil, "", false
}

// RemoveConnection removes a connection for a device from the manager
// NOTE: This does NOT close the connection - the reader goroutine will handle it
func RemoveConnection(deviceID string) {
	connManager.mu.Lock()
	defer connManager.mu.Unlock()

	if connInfo, exists := connManager.connections[deviceID]; exists {
		logger.Println(fmt.Sprintf("[ConnectionManager] Removing connection from manager for device: %s", deviceID))
		// Stop reader goroutine
		if connInfo.ReaderRunning {
			select {
			case connInfo.ReaderStop <- struct{}{}:
			default:
			}
		}
		delete(connManager.connections, deviceID)
	}
}

// CloseConnection closes and removes a connection for a device
// Use this when you want to explicitly close the connection
func CloseConnection(deviceID string) {
	connManager.mu.Lock()
	defer connManager.mu.Unlock()

	if connInfo, exists := connManager.connections[deviceID]; exists {
		logger.Println(fmt.Sprintf("[ConnectionManager] Closing and removing connection for device: %s", deviceID))
		// Stop reader goroutine
		if connInfo.ReaderRunning {
			select {
			case connInfo.ReaderStop <- struct{}{}:
			default:
			}
		}
		if connInfo.Conn != nil {
			connInfo.Conn.Close()
		}
		delete(connManager.connections, deviceID)
	}
}

// CloseAllConnections closes all stored connections
func CloseAllConnections() {
	connManager.mu.Lock()
	defer connManager.mu.Unlock()

	for deviceID, connInfo := range connManager.connections {
		if connInfo.ReaderRunning {
			select {
			case connInfo.ReaderStop <- struct{}{}:
			default:
			}
		}
		if connInfo.Conn != nil {
			logger.Println(fmt.Sprintf("[ConnectionManager] Closing connection for device: %s", deviceID))
			connInfo.Conn.Close()
		}
	}

	connManager.connections = make(map[string]*ConnectionInfo)
}

// PingConnection sends a ping message to verify connection is alive
func PingConnection(conn *websocket.Conn) error {
	if conn == nil {
		return fmt.Errorf("connection is nil")
	}
	if conn.RemoteAddr() == nil {
		return fmt.Errorf("connection is invalid (RemoteAddr is nil)")
	}
	conn.SetWriteDeadline(time.Now().Add(1 * time.Second))
	err := conn.WriteMessage(websocket.PingMessage, nil)
	conn.SetWriteDeadline(time.Time{})
	return err
}

// WriteMessage writes a message to the WebSocket connection with write mutex protection
// This ensures thread-safe writes to the connection
func WriteMessage(deviceID string, messageType int, data []byte) error {
	connManager.mu.RLock()
	connInfo, exists := connManager.connections[deviceID]
	connManager.mu.RUnlock()

	if !exists || connInfo == nil {
		return fmt.Errorf("connection not found for device %s", deviceID)
	}

	connInfo.mu.RLock()
	conn := connInfo.Conn
	connInfo.mu.RUnlock()

	if conn == nil || conn.RemoteAddr() == nil {
		return fmt.Errorf("connection is closed for device %s", deviceID)
	}

	// Use writeMu to serialize writes
	connInfo.writeMu.Lock()
	defer connInfo.writeMu.Unlock()

	return conn.WriteMessage(messageType, data)
}

// WriteJSON writes a JSON message to the WebSocket connection with write mutex protection
// This ensures thread-safe writes to the connection
func WriteJSON(deviceID string, v interface{}) error {
	connManager.mu.RLock()
	connInfo, exists := connManager.connections[deviceID]
	connManager.mu.RUnlock()

	if !exists || connInfo == nil {
		return fmt.Errorf("connection not found for device %s", deviceID)
	}

	connInfo.mu.RLock()
	conn := connInfo.Conn
	connInfo.mu.RUnlock()

	if conn == nil || conn.RemoteAddr() == nil {
		return fmt.Errorf("connection is closed for device %s", deviceID)
	}

	// Use writeMu to serialize writes
	connInfo.writeMu.Lock()
	defer connInfo.writeMu.Unlock()

	return conn.WriteJSON(v)
}
