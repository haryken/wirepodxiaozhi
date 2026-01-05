package xiaozhi

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"sync"
	"time"
)

type PairingCode struct {
	Code      string
	ExpiresAt time.Time
	DeviceID  string
}

var (
	pairingCodes = make(map[string]*PairingCode)
	codesMutex   sync.RWMutex
	codeExpiry   = 10 * time.Minute // Mã hết hạn sau 10 phút
)

// GeneratePairingCode tạo mã 6 chữ số ngẫu nhiên
func GeneratePairingCode(deviceID string) (string, error) {
	// Tạo mã 6 chữ số ngẫu nhiên
	max := big.NewInt(1000000) // 0-999999
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", fmt.Errorf("failed to generate random number: %w", err)
	}
	
	code := fmt.Sprintf("%06d", n.Int64())
	
	codesMutex.Lock()
	defer codesMutex.Unlock()
	
	// Lưu mã với thời gian hết hạn
	pairingCodes[code] = &PairingCode{
		Code:      code,
		ExpiresAt: time.Now().Add(codeExpiry),
		DeviceID:  deviceID,
	}
	
	// Xóa mã cũ đã hết hạn
	go cleanupExpiredCodes()
	
	return code, nil
}

// ValidatePairingCode kiểm tra mã có hợp lệ không
func ValidatePairingCode(code string) (bool, string) {
	codesMutex.RLock()
	defer codesMutex.RUnlock()
	
	pairingCode, exists := pairingCodes[code]
	if !exists {
		return false, ""
	}
	
	if time.Now().After(pairingCode.ExpiresAt) {
		// Mã đã hết hạn, xóa nó
		codesMutex.RUnlock()
		codesMutex.Lock()
		delete(pairingCodes, code)
		codesMutex.Unlock()
		codesMutex.RLock()
		return false, ""
	}
	
	return true, pairingCode.DeviceID
}

// GetPairingCode lấy thông tin mã pairing
func GetPairingCode(code string) (*PairingCode, bool) {
	codesMutex.RLock()
	defer codesMutex.RUnlock()
	
	pairingCode, exists := pairingCodes[code]
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
