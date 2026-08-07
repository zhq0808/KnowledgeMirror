package handler

// 业务错误码。约定：0 成功；其余按 HTTP 语义分段，便于前端与日志排查。
const (
	CodeOK = 0

	CodeBadRequest   = 40000 // 通用参数错误
	CodeUnauthorized = 40100 // 未认证或身份凭证无效
	CodeForbidden    = 40300 // 已识别请求但禁止执行
	CodeNotFound     = 40400 // 资源不存在
	CodeMethodNA     = 40500 // 方法不允许
	CodeConflict     = 40900 // 幂等键冲突或资源状态冲突

	CodeCoachTaskNotFound      = 40410
	CodeCoachTaskNotStartable  = 40910
	CodeCoachTaskIDRequired    = 40911
	CodeCoachTaskMismatch      = 40912
	CodeCoachAttemptConflict   = 40913
	CodeCoachCorrectionPending = 40914
	CodeCoachAnalysisInput     = 40010
	CodeCoachUnavailable       = 50310

	CodeInternal = 50000 // 内部错误
	CodeUpstream = 50200 // 上游（大模型）不可用
)
