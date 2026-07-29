// Package tts 提供文字转语音（TTS）供应商的统一接口。
//
// 职责单一：把一段文本合成成音频字节。“念哪句话、什么时候念、要不要自动播放”
// 全部是 service 与前端的事，这里既不做业务判断，也不做内容改写。
package tts

import "context"

// Speech 是一次语音合成的结果。
type Speech struct {
	Audio     []byte // 音频字节
	MIMEType  string // 音频 MIME 类型，供前端直接喂给 <audio>
	Provider  string // 供应商标识
	Model     string // 供应商侧模型名
	Voice     string // 实际使用的音色
	RequestID string // 供应商侧请求 ID，便于对账；供应商不返回时留空
}

// Provider 是 TTS 供应商的统一接口。实现必须是并发安全的。
type Provider interface {
	// Name 返回供应商标识，写入日志与审计字段。
	Name() string
	// Synthesize 把 text 合成成音频。
	//
	// styleHint 是可选的风格指令（例如“沉稳的技术面试官，语速平缓”）。
	// 它描述的是“怎么念”，绝不会被念出来；供应商不支持时实现应直接忽略，而不是报错。
	Synthesize(ctx context.Context, text, styleHint string) (Speech, error)
}
