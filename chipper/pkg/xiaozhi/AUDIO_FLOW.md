# Luồng Audio trong Wirepod Xiaozhi Integration

## Tình trạng hiện tại

### ❌ CHƯA TỰ ĐỘNG GỬI AUDIO ĐẾN ROBOT

Hiện tại implementation chỉ là **proxy đơn giản**:
- Client → Wirepod → Xiaozhi/OpenAI Server → Wirepod → Client
- Audio chỉ được forward qua lại giữa client và server
- **KHÔNG** tự động gửi đến Vector robot

## Luồng hiện tại (Proxy Mode)

```
┌─────────┐                    ┌──────────────┐                    ┌─────────────┐
│ Client  │                    │ Wirepod      │                    │ OpenAI/     │
│ Device  │                    │ (Proxy)      │                    │ Xiaozhi     │
└────┬────┘                    └──────┬───────┘                    └──────┬──────┘
     │                                 │                                   │
     │ 1. Audio Input (Opus)           │                                   │
     ├─────────────────────────────────>│                                   │
     │                                 │ Forward (Opus)                    │
     │                                 ├──────────────────────────────────>│
     │                                 │                                   │
     │                                 │ 2. Audio Output (PCM/Opus)        │
     │                                 │<──────────────────────────────────┤
     │ 3. Audio Output (Opus)          │                                   │
     │<────────────────────────────────┤                                   │
     │                                 │                                   │
     │ 4. Client phát loa              │                                   │
     │                                 │                                   │
```

**Vấn đề**: Audio không được gửi đến Vector robot!

## Luồng mong muốn (Tích hợp với Vector)

```
┌─────────┐    ┌──────────────┐    ┌─────────────┐    ┌─────────────┐
│ Client  │    │ Wirepod      │    │ OpenAI/     │    │ Vector      │
│ Device  │    │ (Xiaozhi)    │    │ Xiaozhi     │    │ Robot       │
└────┬────┘    └──────┬───────┘    └──────┬──────┘    └──────┬──────┘
     │                │                    │                   │
     │ 1. Audio Input │                    │                   │
     ├───────────────>│                    │                   │
     │                │ 2. Forward         │                   │
     │                ├───────────────────>│                   │
     │                │                    │                   │
     │                │ 3. Audio Output    │                   │
     │                │<───────────────────┤                   │
     │                │                    │                   │
     │                │ 4. Convert & Send  │                   │
     │                ├────────────────────────────────────────>│
     │                │                    │                   │
     │                │                    │ 5. Robot phát loa │
     │                │                    │                   │
```

## Cần làm gì để tích hợp

### 1. Map Device ID với Robot ESN
- Lưu mapping: `device_id` → `robot_esn` khi pairing
- Hoặc lấy từ request headers: `Device-Id` → ESN

### 2. Xử lý Audio Output
Khi nhận audio từ OpenAI/Xiaozhi:
- Convert Opus → PCM (nếu cần)
- Resample về 16kHz (Vector yêu cầu)
- Gửi đến robot qua `ExternalAudioStreamPlayback`

### 3. Code cần thêm

```go
// Khi nhận audio delta từ OpenAI
func (w *XiaozhiHandler) handleAudioDelta(...) {
    // 1. Convert PCM base64 → Opus (hiện tại)
    // 2. THÊM: Gửi đến Vector robot
    deviceID := w.GetDeviceID() // từ request
    robotESN := getRobotESNFromDeviceID(deviceID)
    if robotESN != "" {
        robot, _ := vars.GetRobot(robotESN)
        sendAudioToRobot(robot, opusData)
    }
}
```

## Cách Vector nhận audio

Vector robot nhận audio qua:
- `ExternalAudioStreamPlayback` gRPC stream
- Format: PCM 16kHz, 16-bit, mono
- Chunk size: 1024 bytes
- Frame rate: 16000 Hz

## Tóm tắt

**Hiện tại**: Audio chỉ được proxy giữa client và xiaozhi server, **KHÔNG** tự động gửi đến robot.

**Cần**: Tích hợp logic để:
1. Map device_id với robot ESN
2. Intercept audio output từ xiaozhi/openai
3. Convert format nếu cần
4. Gửi đến Vector robot qua ExternalAudioStreamPlayback
