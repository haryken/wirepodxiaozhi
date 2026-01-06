package processreqs

import (
	"fmt"
	"sync"

	"github.com/kercre123/wire-pod/chipper/pkg/logger"
	"github.com/kercre123/wire-pod/chipper/pkg/vars"
	sr "github.com/kercre123/wire-pod/chipper/pkg/wirepod/speechrequest"
	ttr "github.com/kercre123/wire-pod/chipper/pkg/wirepod/ttr"
	xiaozhi_stt "github.com/kercre123/wire-pod/chipper/pkg/wirepod/stt/xiaozhi"
)

// Server stores the config
type Server struct{}

var VoiceProcessor = ""

type JsonIntent struct {
	Name              string   `json:"name"`
	Keyphrases        []string `json:"keyphrases"`
	RequireExactMatch bool     `json:"requiresexact"`
}

var sttLanguage string = "en-US"

// speech-to-text
var sttHandler func(sr.SpeechRequest) (string, error)
var sttHandlerXiaozhi func(sr.SpeechRequest) (string, error)
var sttHandlerHoundify func(sr.SpeechRequest) (string, error)
var xiaozhiHandlerLoadOnce sync.Once
var xiaozhiHandlerLoadMutex sync.Mutex

// lazyLoadXiaozhiHandler attempts to load xiaozhi STT handler if Knowledge Graph = xiaozhi
// This allows xiaozhi STT to work even when server started with vosk binary
func lazyLoadXiaozhiHandler() {
	xiaozhiHandlerLoadMutex.Lock()
	defer xiaozhiHandlerLoadMutex.Unlock()
	
	// Double-check after acquiring lock
	if sttHandlerXiaozhi != nil {
		return
	}
	
	// Only load if Knowledge Graph = xiaozhi
	if vars.APIConfig.Knowledge.Provider != "xiaozhi" {
		return
	}
	
	// Try to initialize xiaozhi STT
	err := xiaozhi_stt.Init()
	if err != nil {
		logger.Println(fmt.Sprintf("[STT Router] Failed to lazy load xiaozhi STT handler: %v", err))
		return
	}
	
	// Register the handler
	SetSTTHandlerXiaozhi(xiaozhi_stt.STT)
	logger.Println("[STT Router] Successfully lazy loaded xiaozhi STT handler!")
}

// Wrapper STT handler that routes based on config
// Logic: Nếu Knowledge Graph provider = "xiaozhi" → tự động dùng xiaozhi STT cloud
//        Nếu Knowledge Graph provider khác → dùng vosk hoặc provider được set trong STT.Service
func sttHandlerWrapper(sreq sr.SpeechRequest, defaultHandler func(sr.SpeechRequest) (string, error)) (string, error) {
	// Nếu Knowledge Graph provider là xiaozhi → tự động dùng xiaozhi STT cloud
	if vars.APIConfig.Knowledge.Provider == "xiaozhi" {
		// Try to lazy load xiaozhi handler if not available
		if sttHandlerXiaozhi == nil {
			lazyLoadXiaozhiHandler()
		}
		
		if sttHandlerXiaozhi != nil {
			logger.Println(fmt.Sprintf("[STT Router] Device: %s | Knowledge Graph = xiaozhi → Auto-route to xiaozhi STT cloud", sreq.Device))
			return sttHandlerXiaozhi(sreq)
		} else {
			// Handler still not available after lazy load attempt
			logger.Println(fmt.Sprintf("[STT Router] Device: %s | WARNING: Knowledge Graph = xiaozhi but xiaozhi STT handler could not be loaded.", sreq.Device))
			logger.Println(fmt.Sprintf("[STT Router] Device: %s | Falling back to vosk.", sreq.Device))
			return defaultHandler(sreq)
		}
	}
	
	// Nếu Knowledge Graph provider khác (openai, together, etc.) → luôn dùng vosk
	// (Xiaozhi STT chỉ dùng khi Knowledge Graph = xiaozhi)
	logger.Println(fmt.Sprintf("[STT Router] Device: %s | Knowledge Graph = %s (not xiaozhi) → Using vosk STT", 
		sreq.Device, vars.APIConfig.Knowledge.Provider))
	return defaultHandler(sreq)
}

// speech-to-intent (rhino)
var stiHandler func(sr.SpeechRequest) (string, map[string]string, error)

var isSti bool = false

func ReloadVosk() {
	logger.Println(fmt.Sprintf("ReloadVosk called with STT.Service = %s", vars.APIConfig.STT.Service))
	if vars.APIConfig.STT.Service == "vosk" || vars.APIConfig.STT.Service == "whisper.cpp" {
		vars.SttInitFunc()
		vars.IntentList, _ = vars.LoadIntents()
		logger.Println("Reloaded vosk/whisper.cpp STT")
	} else if vars.APIConfig.STT.Service == "xiaozhi" || vars.APIConfig.Knowledge.Provider == "xiaozhi" {
		// Try to lazy load xiaozhi handler if not available
		if sttHandlerXiaozhi == nil {
			lazyLoadXiaozhiHandler()
		}
		
		// Switch to xiaozhi STT handler if available
		if sttHandlerXiaozhi != nil {
			// Note: sttHandler is already a wrapper that routes based on config
			// So we just need to ensure xiaozhi handler is registered
			VoiceProcessor = "xiaozhi"
			logger.Println("Switched to xiaozhi STT handler (handler will be used via wrapper)")
		} else {
			logger.Println("WARNING: Xiaozhi STT handler could not be loaded. Current handler will be used.")
		}
	} else if vars.APIConfig.STT.Service == "houndify" {
		// Switch to houndify STT handler if available
		if sttHandlerHoundify != nil {
			// Note: sttHandler is already a wrapper that routes based on config
			// So we just need to ensure houndify handler is registered
			VoiceProcessor = "houndify"
			logger.Println("Switched to houndify STT handler (handler will be used via wrapper)")
		} else {
			logger.Println("WARNING: Houndify STT handler not available. Current handler will be used. Please restart server with houndify binary to use houndify STT.")
		}
	}
}

// SetSTTHandlerXiaozhi allows xiaozhi STT package to register its handler
func SetSTTHandlerXiaozhi(handler func(sr.SpeechRequest) (string, error)) {
	sttHandlerXiaozhi = handler
}

// SetSTTHandlerHoundify allows houndify STT package to register its handler
func SetSTTHandlerHoundify(handler func(sr.SpeechRequest) (string, error)) {
	sttHandlerHoundify = handler
}

// New returns a new server
func New(InitFunc func() error, SttHandler interface{}, voiceProcessor string) (*Server, error) {

	// Decide the TTS language
	if voiceProcessor != "vosk" && voiceProcessor != "whisper.cpp" {
		vars.APIConfig.STT.Language = "en-US"
	}
	sttLanguage = vars.APIConfig.STT.Language
	vars.IntentList, _ = vars.LoadIntents()
	logger.Println("Initiating " + voiceProcessor + " voice processor with language " + sttLanguage)
	vars.SttInitFunc = InitFunc
	err := InitFunc()
	if err != nil {
		return nil, err
	}

	// SttHandler can either be `func(sr.SpeechRequest) (string, error)` or `func (sr.SpeechRequest) (string, map[string]string, error)`
	// second one exists to accomodate Rhino

	// check function type
	if str, is := SttHandler.(func(sr.SpeechRequest) (string, error)); is {
		// Store the original handler
		originalHandler := str
		// Register xiaozhi or houndify handler if this is the xiaozhi/houndify binary
		if voiceProcessor == "xiaozhi" {
			sttHandlerXiaozhi = str
		} else if voiceProcessor == "houndify" {
			sttHandlerHoundify = str
		}
		// Replace sttHandler with wrapper that routes based on config
		sttHandler = func(sreq sr.SpeechRequest) (string, error) {
			return sttHandlerWrapper(sreq, originalHandler)
		}
	} else if str, is := SttHandler.(func(sr.SpeechRequest) (string, map[string]string, error)); is {
		stiHandler = str
		isSti = true
	} else {
		return nil, fmt.Errorf("stthandler not of correct type")
	}

	// Initiating the chosen voice processor and load intents from json
	VoiceProcessor = voiceProcessor

	// Load plugins
	ttr.LoadPlugins()

	return &Server{}, err
}
