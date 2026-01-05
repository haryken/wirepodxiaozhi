package xiaozhi

import (
	"os"

	"github.com/kercre123/wire-pod/chipper/pkg/vars"
)

type ProviderType string

const (
	ProviderXiaozhi ProviderType = "xiaozhi"
	ProviderOpenAI   ProviderType = "openai"
)

var XiaozhiConfig = struct {
	Provider      ProviderType
	BaseURL       string
	Enabled       bool
	OpenAIBaseURL string
	OpenAIAPIKey  string
	OpenAIModel   string
	OpenAIVoice   string
}{
	Provider:      ProviderType(getEnvOrDefault("XIAOZHI_PROVIDER", "xiaozhi")),
	BaseURL:       getEnvOrDefault("XIAOZHI_BASE_URL", "wss://api.tenclass.net/xiaozhi/v1/"),
	Enabled:       getEnvOrDefault("XIAOZHI_ENABLED", "true") == "true",
	OpenAIBaseURL: getEnvOrDefault("OPENAI_BASE_URL", "wss://api.stepfun.com/v1/realtime"),
	OpenAIAPIKey:  getEnvOrDefault("OPENAI_API_KEY", ""),
	OpenAIModel:   getEnvOrDefault("OPENAI_MODEL", "step-1o-audio"),
	OpenAIVoice:   getEnvOrDefault("OPENAI_VOICE", "alloy"),
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// GetBaseURL returns the xiaozhi base URL, checking Knowledge config first
func GetBaseURL() string {
	// If xiaozhi is configured as Knowledge provider, use that config
	if vars.APIConfig.Knowledge.Provider == "xiaozhi" && vars.APIConfig.Knowledge.Endpoint != "" {
		return vars.APIConfig.Knowledge.Endpoint
	}
	return XiaozhiConfig.BaseURL
}

func GetProvider() ProviderType {
	return XiaozhiConfig.Provider
}

func IsEnabled() bool {
	// Enable if configured in Knowledge Graph or via env var
	return vars.APIConfig.Knowledge.Provider == "xiaozhi" || XiaozhiConfig.Enabled
}

// GetKnowledgeGraphConfig returns xiaozhi config from Knowledge Graph settings
func GetKnowledgeGraphConfig() (baseURL, voice string, voiceWithEnglish bool) {
	if vars.APIConfig.Knowledge.Provider == "xiaozhi" {
		baseURL = vars.APIConfig.Knowledge.Endpoint
		if baseURL == "" {
			baseURL = "wss://api.tenclass.net/xiaozhi/v1/"
		}
		voice = vars.APIConfig.Knowledge.OpenAIVoice
		if voice == "" {
			voice = "fable"
		}
		voiceWithEnglish = vars.APIConfig.Knowledge.OpenAIVoiceWithEnglish
		return
	}
	return XiaozhiConfig.BaseURL, XiaozhiConfig.OpenAIVoice, false
}

func GetOpenAIConfig() (baseURL, apiKey, model, voice string) {
	return XiaozhiConfig.OpenAIBaseURL, XiaozhiConfig.OpenAIAPIKey, XiaozhiConfig.OpenAIModel, XiaozhiConfig.OpenAIVoice
}
