package stt

import "context"

// LocalPlaceholderProviderName 是本地占位供应商的标识。
const LocalPlaceholderProviderName = "local_placeholder"

// localPlaceholderText 是未配置真实 STT 供应商时的占位转写文本。
// 它明确标注“未接入真实供应商”，避免被误当成真实转写——
// 下一步的“用户确认转写”环节会强制展示这段文本让用户编辑替换。
const localPlaceholderText = "〔本地占位转写：尚未接入真实 STT 供应商，请把这段文字改成你刚才实际讲的内容后再确认〕"

// LocalPlaceholderProvider 是未配置任何 STT API Key 时的兜底实现。
// 不发起任何网络调用，保证本地开发和没有第三方密钥的环境也能跑通整条录音确认链路。
type LocalPlaceholderProvider struct{}

// NewLocalPlaceholderProvider 构造本地占位供应商。
func NewLocalPlaceholderProvider() *LocalPlaceholderProvider {
	return &LocalPlaceholderProvider{}
}

func (p *LocalPlaceholderProvider) Name() string { return LocalPlaceholderProviderName }

func (p *LocalPlaceholderProvider) Transcribe(_ context.Context, _ []byte, _ string) (Transcript, error) {
	return Transcript{
		Text:     localPlaceholderText,
		Provider: LocalPlaceholderProviderName,
		Model:    "none",
	}, nil
}
