package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"KnowledgeMirror/internal/service"
)

func TestCapabilitiesHandlerReportsInjectedServices(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	server := &Server{
		realtimeVoice: &service.RealtimeVoiceService{},
		voice:         &service.VoiceCaptureService{},
		speech:        &service.SpeechService{},
		coach:         &service.CoachService{},
		practice:      &service.FeynmanDialogService{},
		engine:        gin.New(),
	}
	server.engine.GET("/api/v1/capabilities", server.capabilitiesHandler)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("HTTP 状态 = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data applicationCapabilities `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if !response.Data.RealtimeVoice || !response.Data.Speech || !response.Data.Coach {
		t.Fatalf("能力位未反映已注入服务: %+v", response.Data)
	}
}

func TestCapabilitiesHandlerRequiresCoachAndPracticeTogether(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	for name, server := range map[string]*Server{
		"coach only":    {coach: &service.CoachService{}, engine: gin.New()},
		"practice only": {practice: &service.FeynmanDialogService{}, engine: gin.New()},
	} {
		t.Run(name, func(t *testing.T) {
			server.engine.GET("/api/v1/capabilities", server.capabilitiesHandler)
			request := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
			recorder := httptest.NewRecorder()
			server.engine.ServeHTTP(recorder, request)
			var response struct {
				Data applicationCapabilities `json:"data"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("解析响应失败: %v", err)
			}
			if response.Data.Coach {
				t.Fatalf("只有一项 Coach 依赖时不应报告可执行: %+v", response.Data)
			}
		})
	}
}

func TestCoachRoutesRequireCoachAndPracticeTogether(t *testing.T) {
	for name, server := range map[string]*Server{
		"coach only": {coach: &service.CoachService{}, engine: gin.New()},
		"both":       {coach: &service.CoachService{}, practice: &service.FeynmanDialogService{}, engine: gin.New()},
	} {
		t.Run(name, func(t *testing.T) {
			server.routes()
			registered := false
			for _, route := range server.engine.Routes() {
				if route.Path == "/api/v1/coach/today" {
					registered = true
					break
				}
			}
			if registered != (name == "both") {
				t.Fatalf("coach route registered=%v", registered)
			}
		})
	}
}

func TestCapabilitiesHandlerReportsUnavailableServices(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	server := &Server{engine: gin.New()}
	server.engine.GET("/api/v1/capabilities", server.capabilitiesHandler)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, request)

	var response struct {
		Data applicationCapabilities `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if response.Data.RealtimeVoice || response.Data.Speech || response.Data.Coach {
		t.Fatalf("未注入服务不应报告可用: %+v", response.Data)
	}
}
