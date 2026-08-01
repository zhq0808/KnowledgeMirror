package handler

import "github.com/gin-gonic/gin"

type applicationCapabilities struct {
	RealtimeVoice bool `json:"realtime_voice"`
	Speech        bool `json:"speech"`
}

// capabilitiesHandler 告诉前端当前进程实际注册了哪些可选能力。
// 返回的是运行态事实，不是静态配置；配置不完整时前端不会再展示一个必然失败的入口。
func (s *Server) capabilitiesHandler(c *gin.Context) {
	ok(c, applicationCapabilities{
		RealtimeVoice: s.realtimeVoice != nil,
		Speech:        s.speech != nil,
	})
}
