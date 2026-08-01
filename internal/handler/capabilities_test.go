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
	if !response.Data.RealtimeVoice || !response.Data.Speech {
		t.Fatalf("能力位未反映已注入服务: %+v", response.Data)
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
	if response.Data.RealtimeVoice || response.Data.Speech {
		t.Fatalf("未注入服务不应报告可用: %+v", response.Data)
	}
}
