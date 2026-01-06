package xiaozhi

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

type PairingCode struct {
	Code      string
	ExpiresAt time.Time
	DeviceID  string
	ClientID  string // Client ID (UUID) - giống ESP32
	Challenge string // Challenge string để device tính HMAC
}

type ActivationRequest struct {
	Algorithm    string `json:"algorithm"`
	SerialNumber string `json:"serial_number"`
	Challenge    string `json:"challenge"`
	HMAC         string `json:"hmac"`
}

type ActivationResponse struct {
	Success bool   `json:"success"`
	Token   string `json:"token,omitempty"`
	Message string `json:"message,omitempty"`
}

var (
	pairingCodes = make(map[string]*PairingCode)
	codesMutex   sync.RWMutex
	codeExpiry   = 10 * time.Minute // Mã hết hạn sau 10 phút

	// Lưu trữ các device đã được activate (MAC address -> token)
	activatedDevices = make(map[string]*ActivatedDevice)
	devicesMutex     sync.RWMutex

	// Lưu trữ các device đã kết nối WebSocket (MAC address -> thông tin kết nối)
	connectedDevices = make(map[string]*ConnectedDevice)
	connectedMutex   sync.RWMutex
)

type ConnectedDevice struct {
	DeviceID    string    // MAC address
	ClientID    string    // Client ID
	ConnectedAt time.Time // Thời gian kết nối
	LastSeen    time.Time // Thời gian hoạt động gần nhất
}

type ActivatedDevice struct {
	DeviceID    string
	ClientID    string // Client ID (UUID) - giống ESP32
	Token       string
	ActivatedAt time.Time
	LastSeen    time.Time
}

// GeneratePairingCode tạo mã 6 chữ số ngẫu nhiên và challenge
// clientID: Client-Id (UUID) - giống ESP32, optional (nếu không có sẽ lấy từ config)
func GeneratePairingCode(deviceID string, clientID ...string) (string, string, error) {
	// Tạo mã 6 chữ số ngẫu nhiên
	max := big.NewInt(1000000) // 0-999999
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate random number: %w", err)
	}

	code := fmt.Sprintf("%06d", n.Int64())

	// Lấy Client-Id từ parameter hoặc config
	var clientIDValue string
	if len(clientID) > 0 && clientID[0] != "" {
		clientIDValue = clientID[0]
	} else {
		clientIDValue = GetClientIDFromConfig()
	}

	// Debug: log code được generate
	fmt.Printf("[DEBUG] GeneratePairingCode: generated code='%s' (length=%d) for deviceID='%s', clientID='%s'\n", code, len(code), deviceID, clientIDValue)

	// Tạo challenge string ngẫu nhiên (32 bytes hex = 64 chars)
	challengeBytes := make([]byte, 32)
	if _, err := rand.Read(challengeBytes); err != nil {
		return "", "", fmt.Errorf("failed to generate challenge: %w", err)
	}
	challenge := hex.EncodeToString(challengeBytes)

	codesMutex.Lock()
	defer codesMutex.Unlock()

	// Lưu mã với thời gian hết hạn, challenge và Client-Id
	pairingCodes[code] = &PairingCode{
		Code:      code,
		ExpiresAt: time.Now().Add(codeExpiry),
		DeviceID:  deviceID,
		ClientID:  clientIDValue, // Lưu Client-Id vào PairingCode
		Challenge: challenge,
	}

	fmt.Printf("[DEBUG] GeneratePairingCode: Saved pairing code to local storage - code='%s', deviceID='%s', clientID='%s', expiresAt='%s'\n",
		code, deviceID, clientIDValue, time.Now().Add(codeExpiry).Format(time.RFC3339))
	fmt.Printf("[DEBUG] GeneratePairingCode: Total active pairing codes: %d\n", len(pairingCodes))

	// Xóa mã cũ đã hết hạn
	go cleanupExpiredCodes()

	return code, challenge, nil
}

// ValidatePairingCode kiểm tra mã có hợp lệ không
func ValidatePairingCode(code string) (bool, string) {
	// Chuẩn hóa code input
	normalizedCode := normalizeCode(code)

	// Debug: log code input và normalized code
	fmt.Printf("[DEBUG] ValidatePairingCode: input='%s', normalized='%s'\n", code, normalizedCode)

	codesMutex.RLock()
	// Debug: log tất cả codes đang có
	fmt.Printf("[DEBUG] Available codes: %v\n", getAllCodeKeys())

	pairingCode, exists := pairingCodes[normalizedCode]
	if !exists {
		codesMutex.RUnlock()
		fmt.Printf("[DEBUG] Code '%s' (normalized: '%s') not found in pairingCodes\n", code, normalizedCode)
		return false, ""
	}

	expired := time.Now().After(pairingCode.ExpiresAt)
	codesMutex.RUnlock()

	if expired {
		// Mã đã hết hạn, xóa nó
		codesMutex.Lock()
		// Kiểm tra lại sau khi lock để tránh race condition
		if pairingCode, stillExists := pairingCodes[normalizedCode]; stillExists && time.Now().After(pairingCode.ExpiresAt) {
			delete(pairingCodes, normalizedCode)
		}
		codesMutex.Unlock()
		return false, ""
	}

	return true, pairingCode.DeviceID
}

// normalizeCode chuẩn hóa mã pairing (trim spaces, chỉ giữ số)
func normalizeCode(code string) string {
	// Loại bỏ spaces và chỉ giữ số
	normalized := ""
	for _, c := range code {
		if c >= '0' && c <= '9' {
			normalized += string(c)
		}
	}
	// Đảm bảo có đúng 6 chữ số (pad với 0 nếu thiếu)
	if len(normalized) > 6 {
		normalized = normalized[:6]
	} else if len(normalized) < 6 {
		// Pad với 0 ở đầu
		for len(normalized) < 6 {
			normalized = "0" + normalized
		}
	}
	return normalized
}

// getAllCodeKeys helper function để debug
func getAllCodeKeys() []string {
	keys := make([]string, 0, len(pairingCodes))
	for k := range pairingCodes {
		keys = append(keys, k)
	}
	return keys
}

// ConsumePairingCode validate và xóa mã sau khi sử dụng (one-time use)
// Đây là hàm nên được dùng khi pair thiết bị để tránh tái sử dụng mã
func ConsumePairingCode(code string) (bool, string) {
	// Chuẩn hóa code input
	normalizedCode := normalizeCode(code)

	// Debug: log code input và normalized code
	fmt.Printf("[DEBUG] ConsumePairingCode: input='%s', normalized='%s'\n", code, normalizedCode)

	codesMutex.Lock()
	defer codesMutex.Unlock()

	// Debug: log tất cả codes đang có
	fmt.Printf("[DEBUG] Available codes: %v\n", getAllCodeKeys())

	pairingCode, exists := pairingCodes[normalizedCode]
	if !exists {
		fmt.Printf("[DEBUG] Code '%s' (normalized: '%s') not found in pairingCodes\n", code, normalizedCode)
		return false, ""
	}

	if time.Now().After(pairingCode.ExpiresAt) {
		// Mã đã hết hạn, xóa nó
		delete(pairingCodes, normalizedCode)
		return false, ""
	}

	// Lưu deviceID trước khi xóa
	deviceID := pairingCode.DeviceID

	// Xóa mã sau khi validate thành công để tránh tái sử dụng
	delete(pairingCodes, normalizedCode)

	return true, deviceID
}

// GetPairingCode lấy thông tin mã pairing
func GetPairingCode(code string) (*PairingCode, bool) {
	// Chuẩn hóa code input
	normalizedCode := normalizeCode(code)

	codesMutex.RLock()
	defer codesMutex.RUnlock()

	pairingCode, exists := pairingCodes[normalizedCode]
	if !exists {
		return nil, false
	}

	if time.Now().After(pairingCode.ExpiresAt) {
		return nil, false
	}

	return pairingCode, true
}

// cleanupExpiredCodes xóa các mã đã hết hạn
func cleanupExpiredCodes() {
	codesMutex.Lock()
	defer codesMutex.Unlock()

	now := time.Now()
	for code, pairingCode := range pairingCodes {
		if now.After(pairingCode.ExpiresAt) {
			delete(pairingCodes, code)
		}
	}
}

// GetAllActiveCodes lấy tất cả mã đang hoạt động (cho debug)
func GetAllActiveCodes() map[string]*PairingCode {
	codesMutex.RLock()
	defer codesMutex.RUnlock()

	result := make(map[string]*PairingCode)
	now := time.Now()

	for code, pairingCode := range pairingCodes {
		if now.Before(pairingCode.ExpiresAt) {
			result[code] = pairingCode
		}
	}

	return result
}

// ValidateActivationRequest validate activation request với HMAC
// key là secret key để tính HMAC (thường là device-specific key hoặc shared secret)
func ValidateActivationRequest(req *ActivationRequest, key []byte) (bool, string) {
	// Tìm pairing code từ challenge
	codesMutex.RLock()
	var pairingCode *PairingCode
	var code string
	for c, pc := range pairingCodes {
		if pc.Challenge == req.Challenge {
			pairingCode = pc
			code = c
			break
		}
	}
	codesMutex.RUnlock()

	if pairingCode == nil {
		return false, "Invalid challenge"
	}

	// Kiểm tra code đã được nhập chưa (thông qua validate trước đó)
	// Trong luồng thực tế, code phải được validate trước khi activate

	// Validate HMAC
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(req.Challenge))
	expectedHMAC := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(req.HMAC), []byte(expectedHMAC)) {
		return false, "Invalid HMAC"
	}

	// Kiểm tra hết hạn
	if time.Now().After(pairingCode.ExpiresAt) {
		return false, "Pairing code expired"
	}

	// Xóa code sau khi activate thành công
	codesMutex.Lock()
	delete(pairingCodes, code)
	codesMutex.Unlock()

	return true, pairingCode.DeviceID
}

// GetChallengeFromCode lấy challenge từ pairing code (để device tính HMAC)
func GetChallengeFromCode(code string) (string, bool) {
	normalizedCode := normalizeCode(code)

	codesMutex.RLock()
	defer codesMutex.RUnlock()

	pairingCode, exists := pairingCodes[normalizedCode]
	if !exists {
		return "", false
	}

	if time.Now().After(pairingCode.ExpiresAt) {
		return "", false
	}

	return pairingCode.Challenge, true
}

// IsDeviceActivated kiểm tra xem device đã được activate chưa
func IsDeviceActivated(deviceID string) bool {
	devicesMutex.RLock()
	defer devicesMutex.RUnlock()

	_, exists := activatedDevices[deviceID]
	return exists
}

// RegisterActivatedDevice đăng ký device đã được activate
func RegisterActivatedDevice(deviceID, clientID, token string) {
	devicesMutex.Lock()
	defer devicesMutex.Unlock()

	activatedDevices[deviceID] = &ActivatedDevice{
		DeviceID:    deviceID,
		ClientID:    clientID,
		Token:       token,
		ActivatedAt: time.Now(),
		LastSeen:    time.Now(),
	}
}

// GetActivatedDevice lấy thông tin device đã activate
func GetActivatedDevice(deviceID string) (*ActivatedDevice, bool) {
	devicesMutex.RLock()
	defer devicesMutex.RUnlock()

	device, exists := activatedDevices[deviceID]
	return device, exists
}

// UpdateDeviceLastSeen cập nhật thời gian device kết nối gần nhất
func UpdateDeviceLastSeen(deviceID string) {
	devicesMutex.Lock()
	defer devicesMutex.Unlock()

	if device, exists := activatedDevices[deviceID]; exists {
		device.LastSeen = time.Now()
	}
}

// RemoveActivatedDevice xóa device khỏi danh sách activated (để có thể pair lại)
func RemoveActivatedDevice(deviceID string) bool {
	devicesMutex.Lock()
	defer devicesMutex.Unlock()

	if _, exists := activatedDevices[deviceID]; exists {
		delete(activatedDevices, deviceID)
		return true
	}
	return false
}

// GetAllActivatedDevices lấy tất cả devices đã activate (cho debug/admin)
func GetAllActivatedDevices() map[string]*ActivatedDevice {
	devicesMutex.RLock()
	defer devicesMutex.RUnlock()

	result := make(map[string]*ActivatedDevice)
	for deviceID, device := range activatedDevices {
		result[deviceID] = device
	}
	return result
}

// RegisterConnectedDevice đăng ký device đã kết nối WebSocket (lưu MAC address)
func RegisterConnectedDevice(deviceID, clientID string) {
	connectedMutex.Lock()
	defer connectedMutex.Unlock()

	connectedDevices[deviceID] = &ConnectedDevice{
		DeviceID:    deviceID,
		ClientID:    clientID,
		ConnectedAt: time.Now(),
		LastSeen:    time.Now(),
	}
}

// UpdateConnectedDeviceLastSeen cập nhật thời gian device hoạt động gần nhất
func UpdateConnectedDeviceLastSeen(deviceID string) {
	connectedMutex.Lock()
	defer connectedMutex.Unlock()

	if device, exists := connectedDevices[deviceID]; exists {
		device.LastSeen = time.Now()
	}
}

// GetConnectedDevice lấy thông tin device đã kết nối
func GetConnectedDevice(deviceID string) (*ConnectedDevice, bool) {
	connectedMutex.RLock()
	defer connectedMutex.RUnlock()

	device, exists := connectedDevices[deviceID]
	return device, exists
}

// GetAllConnectedDevices lấy tất cả devices đã kết nối (MAC addresses)
func GetAllConnectedDevices() map[string]*ConnectedDevice {
	connectedMutex.RLock()
	defer connectedMutex.RUnlock()

	result := make(map[string]*ConnectedDevice)
	for deviceID, device := range connectedDevices {
		result[deviceID] = device
	}
	return result
}

// RemoveConnectedDevice xóa device khỏi danh sách connected
func RemoveConnectedDevice(deviceID string) bool {
	connectedMutex.Lock()
	defer connectedMutex.Unlock()

	if _, exists := connectedDevices[deviceID]; exists {
		delete(connectedDevices, deviceID)
		return true
	}
	return false
}

// GetLocalMACAddresses lấy tất cả MAC addresses của máy đang chạy code
func GetLocalMACAddresses() []string {
	var macAddresses []string

	interfaces, err := net.Interfaces()
	if err != nil {
		return macAddresses
	}

	for _, iface := range interfaces {
		// Bỏ qua loopback và interface không có hardware address
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.HardwareAddr == nil || len(iface.HardwareAddr) == 0 {
			continue
		}

		mac := iface.HardwareAddr.String()
		if mac != "" {
			macAddresses = append(macAddresses, mac)
		}
	}

	return macAddresses
}

// GetPrimaryMACAddress lấy MAC address chính của máy (interface đầu tiên không phải loopback)
func GetPrimaryMACAddress() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}

	for _, iface := range interfaces {
		// Bỏ qua loopback và interface không có hardware address
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.HardwareAddr == nil || len(iface.HardwareAddr) == 0 {
			continue
		}

		// Ưu tiên interface đang up và không phải loopback
		if iface.Flags&net.FlagUp != 0 {
			return iface.HardwareAddr.String()
		}
	}

	// Nếu không tìm thấy interface up, lấy interface đầu tiên
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.HardwareAddr != nil && len(iface.HardwareAddr) > 0 {
			return iface.HardwareAddr.String()
		}
	}

	return ""
}

// ValidateDeviceIDFormat kiểm tra format của Device-Id (MAC address hoặc UUID hoặc pc_xxx)
func ValidateDeviceIDFormat(deviceID string) bool {
	if deviceID == "" {
		return false
	}

	// MAC address format: xx:xx:xx:xx:xx:xx hoặc xx-xx-xx-xx-xx-xx
	macRegex := regexp.MustCompile(`^([0-9A-Fa-f]{2}[:-]){5}([0-9A-Fa-f]{2})$`)
	if macRegex.MatchString(deviceID) {
		return true
	}

	// UUID format: xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
	uuidRegex := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	if uuidRegex.MatchString(deviceID) {
		return true
	}

	// PC IP-based format: pc_xxx hoặc pc_xxx:port
	pcRegex := regexp.MustCompile(`^pc_[a-zA-Z0-9._:]+$`)
	if pcRegex.MatchString(deviceID) {
		return true
	}

	// Pending format: pending_xxx
	pendingRegex := regexp.MustCompile(`^pending_\d+$`)
	if pendingRegex.MatchString(deviceID) {
		return true
	}

	return false
}

// CheckDeviceActivationFromServer kiểm tra trạng thái activation từ upstream server
// Sử dụng endpoint check version của server (giống như ESP32)
// clientID: Client-Id từ request header hoặc config (optional, nhưng nên có để tương thích với ESP32)
func CheckDeviceActivationFromServer(deviceID string, clientID ...string) (bool, string, error) {
	// Validate Device-Id format trước khi check
	if !ValidateDeviceIDFormat(deviceID) {
		return false, fmt.Sprintf("Invalid Device-Id format: %s (must be MAC address, UUID, pc_xxx, or pending_xxx)", deviceID), nil
	}

	// Lấy base URL từ config
	baseURL := GetBaseURL()

	// Chuyển từ WebSocket URL sang HTTP URL
	// wss://api.tenclass.net/xiaozhi/v1/ -> https://api.tenclass.net/
	httpURL := strings.Replace(baseURL, "wss://", "https://", 1)
	httpURL = strings.Replace(httpURL, "ws://", "http://", 1)

	// Loại bỏ path /xiaozhi/v1/
	if strings.Contains(httpURL, "/xiaozhi/v1/") {
		httpURL = strings.TrimSuffix(httpURL, "/xiaozhi/v1/")
	}
	if !strings.HasSuffix(httpURL, "/") {
		httpURL += "/"
	}

	// Endpoint check version (giống botkct.py: self.OTA_URL)
	// botkct.py: DEFAULT_OTA_URL = "https://api.tenclass.net/xiaozhi/ota/"
	// Chỉ thử endpoint OTA (giống botkct.py)

	// Lấy Client-Id từ parameter hoặc config (giống botkct.py)
	var clientIDValue string
	if len(clientID) > 0 && clientID[0] != "" {
		clientIDValue = clientID[0]
	} else {
		clientIDValue = GetClientIDFromConfig()
	}

	// botkct.py chỉ dùng GET request (không POST với system info)
	// Chỉ thử endpoint OTA (giống botkct.py: self.OTA_URL)
	checkURLs := []string{
		httpURL + "xiaozhi/ota/", // botkct.py: DEFAULT_OTA_URL = "https://api.tenclass.net/xiaozhi/ota/"
		httpURL + "xiaozhi/ota",  // Không có trailing slash
	}

	var lastErr error
	for _, checkURL := range checkURLs {
		// Log endpoint đang thử
		fmt.Printf("[DEBUG] CheckDeviceActivationFromServer: Trying endpoint: %s (GET method, giống botkct.py)\n", checkURL)

		// botkct.py chỉ dùng GET request
		req, err := http.NewRequest("GET", checkURL, nil)
		if err != nil {
			fmt.Printf("[DEBUG] CheckDeviceActivationFromServer: Failed to create GET request for %s: %v\n", checkURL, err)
			lastErr = err
			continue
		}

		// Headers giống botkct.py
		req.Header.Set("Device-Id", deviceID)
		if clientIDValue != "" {
			req.Header.Set("Client-Id", clientIDValue)
		}
		req.Header.Set("Accept-Language", "vi")            // botkct.py: "Accept-Language": "vi"
		req.Header.Set("User-Agent", "wirepodxiaozhi/1.0") // botkct.py: "User-Agent": "Xiaozhi-Python-Client/1.0.0"

		// Log headers đang gửi
		fmt.Printf("[DEBUG] CheckDeviceActivationFromServer: GET %s - Request headers: Device-Id=%s, Client-Id=%s, Accept-Language=%s, User-Agent=%s\n",
			checkURL, req.Header.Get("Device-Id"), req.Header.Get("Client-Id"),
			req.Header.Get("Accept-Language"), req.Header.Get("User-Agent"))

		// Tạo HTTP client với timeout (giống botkct.py: timeout=10)
		client := &http.Client{
			Timeout: 10 * time.Second,
		}

		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("[DEBUG] CheckDeviceActivationFromServer: Request failed for %s: %v\n", checkURL, err)
			lastErr = err
			continue
		}
		defer resp.Body.Close()

		// Log response status
		fmt.Printf("[DEBUG] CheckDeviceActivationFromServer: Response from %s - StatusCode=%d, Status=%s\n", checkURL, resp.StatusCode, resp.Status)

		// botkct.py: if resp.status_code != 200: return False
		if resp.StatusCode != 200 {
			fmt.Printf("[DEBUG] CheckDeviceActivationFromServer: Status code != 200 (%d), trying next endpoint\n", resp.StatusCode)
			lastErr = fmt.Errorf("server returned status %d from %s", resp.StatusCode, checkURL)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			fmt.Printf("[DEBUG] CheckDeviceActivationFromServer: Failed to read response body from %s: %v\n", checkURL, err)
			lastErr = err
			continue
		}

		// Log response body (truncate nếu quá dài)
		bodyStr := string(body)
		if len(bodyStr) > 500 {
			fmt.Printf("[DEBUG] CheckDeviceActivationFromServer: Response body (first 500 chars): %s...\n", bodyStr[:500])
		} else {
			fmt.Printf("[DEBUG] CheckDeviceActivationFromServer: Response body: %s\n", bodyStr)
		}

		// botkct.py: try: data = resp.json() except Exception: return False
		var result map[string]interface{}
		if err := json.Unmarshal(body, &result); err != nil {
			fmt.Printf("[DEBUG] CheckDeviceActivationFromServer: Failed to parse JSON from %s: %v -> return False (giống botkct.py)\n", checkURL, err)
			return false, "", nil // botkct.py: return False nếu không parse được JSON
		}

		// Log parsed JSON result
		fmt.Printf("[DEBUG] CheckDeviceActivationFromServer: Successfully parsed JSON from %s: %+v\n", checkURL, result)

		// botkct.py logic (theo thứ tự):
		// 1. text_blob = json.dumps(data).lower()
		//    if any(k in text_blob for k in ["activated", "verified", "success"]): return True
		bodyLower := strings.ToLower(bodyStr)
		if strings.Contains(bodyLower, "activated") ||
			strings.Contains(bodyLower, "verified") ||
			strings.Contains(bodyLower, "success") {
			fmt.Printf("[DEBUG] CheckDeviceActivationFromServer: Found activation keywords in response body -> Device is activated (giống botkct.py)\n")
			return true, "Device is activated (server response contains activation keywords)", nil
		}

		// 2. if isinstance(data, dict):
		//    status = str(data.get("status", "")).lower()
		//    if status in ["ok", "success", "verified", "activated"]: return True
		if status, ok := result["status"].(string); ok {
			statusLower := strings.ToLower(status)
			if statusLower == "ok" || statusLower == "success" || statusLower == "verified" || statusLower == "activated" {
				fmt.Printf("[DEBUG] CheckDeviceActivationFromServer: Status field='%s' -> Device is activated (giống botkct.py)\n", status)
				return true, fmt.Sprintf("Device is activated (status: %s)", status), nil
			}
		}

		// 3. if bool(data.get("verified", False)) or bool(data.get("activated", False)): return True
		if verified, ok := result["verified"].(bool); ok && verified {
			fmt.Printf("[DEBUG] CheckDeviceActivationFromServer: verified=true -> Device is activated (giống botkct.py)\n")
			return true, "Device is activated (verified: true)", nil
		}
		if activated, ok := result["activated"].(bool); ok && activated {
			fmt.Printf("[DEBUG] CheckDeviceActivationFromServer: activated=true -> Device is activated (giống botkct.py)\n")
			return true, "Device is activated (activated: true)", nil
		}

		// botkct.py: return False (nếu không match bất kỳ điều kiện nào)
		fmt.Printf("[DEBUG] CheckDeviceActivationFromServer: No activation indicators found -> Device not activated (giống botkct.py)\n")
		return false, "Device not activated (no activation indicators in server response)", nil
	}

	// Nếu tất cả endpoints đều fail, trả về lỗi
	fmt.Printf("[DEBUG] CheckDeviceActivationFromServer: All endpoints failed, last error: %v\n", lastErr)
	return false, "", fmt.Errorf("failed to check device status from server: %w", lastErr)
}
