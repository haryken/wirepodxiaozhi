package xiaozhi

import (
	"fmt"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/kercre123/wire-pod/chipper/pkg/logger"
)

// ConnectionManager manages WebSocket connections per device
// This allows STT and LLM to reuse the same connection (like botkct.py)
type ConnectionManager struct {
	connections map[string]*websocket.Conn
	sessionIDs  map[string]string // deviceID -> sessionID
	mu          sync.RWMutex
}

var connManager = &ConnectionManager{
	connections: make(map[string]*websocket.Conn),
	sessionIDs:  make(map[string]string),
}

// StoreConnection stores a WebSocket connection for a device
// This is called by STT after establishing connection
func StoreConnection(deviceID string, conn *websocket.Conn, sessionID string) {
	connManager.mu.Lock()
	defer connManager.mu.Unlock()
	
	// Close old connection if exists
	if oldConn, exists := connManager.connections[deviceID]; exists && oldConn != nil {
		logger.Println(fmt.Sprintf("[ConnectionManager] Closing old connection for device: %s", deviceID))
		oldConn.Close()
	}
	
	connManager.connections[deviceID] = conn
	connManager.sessionIDs[deviceID] = sessionID
	logger.Println(fmt.Sprintf("[ConnectionManager] Stored connection for device: %s, sessionID: %s", deviceID, sessionID))
}

// GetConnection retrieves a WebSocket connection for a device
// Returns connection, sessionID, and whether connection exists
func GetConnection(deviceID string) (*websocket.Conn, string, bool) {
	connManager.mu.RLock()
	defer connManager.mu.RUnlock()
	
	conn, exists := connManager.connections[deviceID]
	sessionID := connManager.sessionIDs[deviceID]
	return conn, sessionID, exists && conn != nil
}

// RemoveConnection removes a connection for a device from the manager
// NOTE: This does NOT close the connection - the caller (STT reader) will close it when done
// This prevents "use of closed network connection" errors when LLM finishes but STT reader is still active
func RemoveConnection(deviceID string) {
	connManager.mu.Lock()
	defer connManager.mu.Unlock()
	
	logger.Println(fmt.Sprintf("[ConnectionManager] Removing connection from manager for device: %s (connection will be closed by STT reader)", deviceID))
	
	// KHÔNG đóng connection ở đây - STT reader sẽ tự đóng khi xong
	// Chỉ xóa khỏi manager để LLM không dùng lại nữa
	
	delete(connManager.connections, deviceID)
	delete(connManager.sessionIDs, deviceID)
}

// CloseConnection closes and removes a connection for a device
// Use this when you want to explicitly close the connection
func CloseConnection(deviceID string) {
	connManager.mu.Lock()
	defer connManager.mu.Unlock()
	
	if conn, exists := connManager.connections[deviceID]; exists && conn != nil {
		logger.Println(fmt.Sprintf("[ConnectionManager] Closing and removing connection for device: %s", deviceID))
		conn.Close()
	}
	
	delete(connManager.connections, deviceID)
	delete(connManager.sessionIDs, deviceID)
}

// CloseAllConnections closes all stored connections
func CloseAllConnections() {
	connManager.mu.Lock()
	defer connManager.mu.Unlock()
	
	for deviceID, conn := range connManager.connections {
		if conn != nil {
			logger.Println(fmt.Sprintf("[ConnectionManager] Closing connection for device: %s", deviceID))
			conn.Close()
		}
	}
	
	connManager.connections = make(map[string]*websocket.Conn)
	connManager.sessionIDs = make(map[string]string)
}

