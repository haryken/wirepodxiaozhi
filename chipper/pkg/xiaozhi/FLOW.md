# Luồng hoạt động của Xiaozhi trong Wirepod

## Tổng quan luồng

```
Client (Thiết bị)  →  Wirepod (Xiaozhi Handler)  →  OpenAI/Xiaozhi Server  →  Wirepod  →  Client
```

## Chi tiết từng bước

### 1. Kết nối và Pairing
```
Client → WebSocket Connect → /xiaozhi/v1/
Client → Gửi "hello" event với audio params
Server → Trả về "hello" response với session_id
```

**Event Hello:**
```json
{
  "type": "hello",
  "version": 1,
  "transport": "websocket",
  "audio_params": {
    "format": "opus",
    "sample_rate": 16000,
    "channels": 1,
    "frame_duration": 20
  }
}
```

### 2. Bắt đầu nghe (Listen Start)
```
Client → Gửi "listen" event với state="start"
Server → Sẵn sàng nhận audio
```

**Event Listen:**
```json
{
  "type": "listen",
  "state": "start",
  "mode": "auto"
}
```

### 3. Gửi Audio Input (Người dùng nói)
```
Client → Gửi binary Opus frames (append.buffer)
Server → Nhận Opus → Decode thành PCM → Gửi đến OpenAI Realtime API
```

**Luồng xử lý:**
- Client gửi: Binary Opus data (raw bytes)
- Server nhận: `ClientEventAppendBuffer` với `Bytes []byte`
- Server xử lý:
  1. Opus → PCM (decode)
  2. PCM → Base64 (encode)
  3. Gửi đến OpenAI: `InputAudioBufferAppendEvent` với `Audio: base64_string`

### 4. Xử lý tại OpenAI Server
```
OpenAI nhận PCM audio → 
  - STT: Chuyển speech thành text
  - LLM: Xử lý và tạo response
  - TTS: Chuyển text thành audio (PCM)
```

**Events từ OpenAI:**
- `conversation.item.input_audio_transcription.completed` → STT text
- `response.content_part.done` → LLM text response
- `response.audio.delta` → Audio PCM (base64)

### 5. Nhận và xử lý Audio Output
```
OpenAI → Gửi audio delta (PCM base64)
Server → Nhận PCM → Encode thành Opus → Gửi binary về Client
Client → Nhận Opus → Decode → Phát ra loa
```

**Luồng xử lý audio output:**
1. OpenAI gửi: `ResponseAudioDeltaEvent` với `Delta: "base64_pcm_string"`
2. Server xử lý:
   - Decode base64 → PCM bytes
   - Convert PCM → Opus (encode)
   - Gửi binary Opus về client qua `writeQueue`
3. Client nhận: Binary Opus data
4. Client decode Opus → PCM → Phát ra loa

### 6. Kết thúc
```
Client → Gửi "listen" với state="stop"
Server → Xử lý và trả về "tts" với state="stop"
```

## Code Flow trong go-xiaozhi

### Input Flow (Client → Server → OpenAI)
```
1. Client gửi binary Opus
   ↓
2. handler/xiaozhi/conn.go: ReadLoop() nhận binary
   ↓
3. handler/openai/conn.go: ReadLoop() → UnmarshalClientBinEvent()
   ↓
4. handler/openai/client_handler.go: handleInputAudioBufferAppend()
   - audioConverter.OpusToPcmBase64() - Convert Opus → PCM base64
   ↓
5. handler/openai/proxy_handler.go: SendToRealtimeAPI()
   - Gửi InputAudioBufferAppendEvent đến OpenAI
```

### Output Flow (OpenAI → Server → Client)
```
1. OpenAI gửi ResponseAudioDeltaEvent (PCM base64)
   ↓
2. handler/openai/proxy_handler.go: handleRealtimeApiEvent()
   ↓
3. handler/openai/proxy_handler.go: handleAudioDelta()
   - audioConverter.ResolvePCM() - Convert PCM base64 → Opus
   ↓
4. pkg/audio/audio_converter.go: parseFrames()
   - Encode PCM → Opus frames
   - Gọi callback: WriteRespEvent()
   ↓
5. handler/openai/client_handler.go: WriteRespEvent()
   - Đưa Opus binary vào writeQueue
   ↓
6. handler/openai/conn.go: WriteLoop()
   - Đọc từ writeQueue → Gửi binary về client
   ↓
7. Client nhận binary Opus → Decode → Phát loa
```

## Audio Converter

### Opus → PCM (Input)
- `audioConverter.OpusToPcmBase64()`:
  - Decode Opus → PCM int16
  - Resample nếu cần
  - Encode PCM → Base64 string

### PCM → Opus (Output)
- `audioConverter.ResolvePCM()`:
  - Decode Base64 → PCM bytes
  - Convert bytes → int16 samples
  - Encode PCM → Opus frames
  - Gửi Opus binary qua callback

## Events quan trọng

### Client Events (Client → Server)
- `hello`: Khởi tạo session
- `listen` (state=start): Bắt đầu nghe
- `append.buffer`: Gửi audio Opus (binary)
- `listen` (state=stop): Dừng nghe

### Server Events (Server → Client)
- `hello`: Xác nhận session
- `stt`: Text transcription từ audio input
- `llm`: Text response từ LLM
- `tts` (state=start): Bắt đầu phát audio
- `tts` (state=sentence_start): Bắt đầu câu
- `tts` (state=sentence_end): Kết thúc câu
- Binary Opus: Audio data để phát loa
- `tts` (state=stop): Kết thúc phát audio

## Lưu ý quan trọng

1. **Audio Format**: 
   - Input: Opus (binary)
   - Internal: PCM (base64)
   - Output: Opus (binary)

2. **Sample Rate**: 
   - Client: 16kHz hoặc 24kHz
   - Server: Convert về 24kHz cho OpenAI
   - Output: 24kHz Opus

3. **Frame Duration**: 
   - Thường là 20ms hoặc 60ms
   - Ứng với 960 samples @ 16kHz hoặc 1440 samples @ 24kHz

4. **Real-time Processing**:
   - Audio được xử lý streaming (delta chunks)
   - Không cần đợi toàn bộ audio mới xử lý
