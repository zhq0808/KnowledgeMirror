// Package stt 提供语音转文字（STT）供应商的统一接口。
//
// 职责单一：把一段音频字节转成文本，返回供应商信息，不关心业务规则——
// “转写结果要不要落库、要不要让用户确认”全部是 service 层的事。
package stt

import "context"

// Transcript 是一次转写的结果。
type Transcript struct {
	Text      string // 转写文本（未经用户确认，不可直接当作可信内容使用）
	Provider  string // 供应商标识，如 openai_whisper / local_placeholder
	Model     string // 供应商侧模型名
	RequestID string // 供应商侧请求 ID，便于对账和问题排查；供应商不返回时留空
}

// Provider 是 STT 供应商的统一接口。实现必须是并发安全的（可能被多个请求同时调用）。
type Provider interface {
	// Name 返回供应商标识，写入审计字段。
	Name() string
	// Transcribe 把音频字节转成文本。mimeType 是客户端上报的音频格式。
	Transcribe(ctx context.Context, audio []byte, mimeType string) (Transcript, error)
}
