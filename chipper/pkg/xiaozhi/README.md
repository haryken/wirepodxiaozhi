# Xiaozhi Integration cho Wirepod

Package này tích hợp xiaozhi WebSocket handler vào wirepod, hỗ trợ chuyển đổi giữa xiaozhi và OpenAI Realtime API.

## Cấu hình

Bạn có thể chuyển đổi giữa xiaozhi và OpenAI bằng cách set các environment variables:

### Sử dụng Xiaozhi (mặc định)

```bash
export XIAOZHI_PROVIDER="xiaozhi"
export XIAOZHI_BASE_URL="wss://api.tenclass.net/xiaozhi/v1/"
export XIAOZHI_ENABLED="true"
```

### Sử dụng OpenAI Realtime API

```bash
export XIAOZHI_PROVIDER="openai"
export OPENAI_BASE_URL="wss://api.stepfun.com/v1/realtime"
export OPENAI_API_KEY="your-api-key-here"
export OPENAI_MODEL="step-1o-audio"
export OPENAI_VOICE="alloy"
export XIAOZHI_ENABLED="true"
```

## Environment Variables

### Common
- `XIAOZHI_ENABLED`: Bật/tắt xiaozhi handler (mặc định: `true`)
- `XIAOZHI_PROVIDER`: Chọn provider - `xiaozhi` hoặc `openai` (mặc định: `xiaozhi`)

### Xiaozhi Provider
- `XIAOZHI_BASE_URL`: Base URL của xiaozhi server (mặc định: `wss://api.tenclass.net/xiaozhi/v1/`)

### OpenAI Provider
- `OPENAI_BASE_URL`: Base URL của OpenAI Realtime API (mặc định: `wss://api.stepfun.com/v1/realtime`)
- `OPENAI_API_KEY`: API key của OpenAI (bắt buộc khi dùng OpenAI provider)
- `OPENAI_MODEL`: Model name (mặc định: `step-1o-audio`)
- `OPENAI_VOICE`: Voice name (mặc định: `alloy`)

## Endpoint

Sau khi khởi động wirepod, xiaozhi handler sẽ có sẵn tại:

```
ws://localhost:8080/xiaozhi/v1/
```

## Ví dụ sử dụng

### Chuyển sang OpenAI
```bash
export XIAOZHI_PROVIDER="openai"
export OPENAI_API_KEY="sk-your-key-here"
export OPENAI_BASE_URL="wss://api.stepfun.com/v1/realtime"
```

### Chuyển về Xiaozhi
```bash
export XIAOZHI_PROVIDER="xiaozhi"
export XIAOZHI_BASE_URL="wss://api.tenclass.net/xiaozhi/v1/"
```

### Tắt xiaozhi handler
```bash
export XIAOZHI_ENABLED="false"
```

## Lưu ý

- Khi sử dụng OpenAI provider, bạn **phải** set `OPENAI_API_KEY`
- Provider được chọn khi khởi động wirepod, không thể thay đổi khi đang chạy
- Để thay đổi provider, cần restart wirepod sau khi set environment variables mới
