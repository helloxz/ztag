package model

// AnalyzeRequest 图片分析请求体。
// 调用方三选一提供图片：
//   - ImageURL：图片的 HTTP(S) 地址，由服务端下载；
//   - ImageBase64：图片的 base64 编码内容（data URI 或纯 base64 均可）。
//
// 后续如需支持 multipart 文件上传，可在该结构体上扩展 FormFile 字段。
type AnalyzeRequest struct {
	ImageURL    string `json:"image_url"`    // 图片 URL（与 ImageBase64 二选一）
	ImageBase64 string `json:"image_base64"` // 图片 base64 内容（与 ImageURL 二选一）
	Model       string `json:"model"`        // 可选：格式为 "channel/modelId"（如 "default/gpt-4o-mini"），channel 仅允许 default/backup，modelId 可含斜杠；为空则由网关按负载均衡决定
	LoadBalance bool   `json:"load_balance"` // 可选：仅当 Model 为空时生效，true 表示从 default 渠道中随机选择一个模型；与 Model 互斥，Model 优先

	// 内部字段：由 service 层在下载/校验后填充，AI 网关据此构造图片消息，不参与对外 JSON
	ImageDataURI string `json:"-"` // 图片 base64 data URI（data:image/xxx;base64,...）
	ImageMIME    string `json:"-"` // 图片 MIME 类型（如 image/jpeg）
}

// AnalyzeResult 图片分析结果（AI 网关返回的结构化产物）。
type AnalyzeResult struct {
	Keywords       []string       `json:"keywords"`       // 图片 SEO 关键词（3-5 个，中文）
	Description    string         `json:"description"`    // 图片 SEO 描述（不超过 150 字，中文）
	Classification Classification `json:"classification"` // 图片分类明细
	ModelID        string         `json:"model_id"`       // 实际处理该图的大模型标识（由 AI 网关填充）
	ElapsedMs      int64          `json:"elapsed_ms"`     // 请求处理总耗时（毫秒），由 handler 层在响应前填充
	Raw            interface{}    `json:"-"`              // 原始模型输出（调试用，不回传给客户端）
}

// Classification 图片分类明细（简化版，仅保留单分类判定）。
type Classification struct {
	Category   string  `json:"category"`    // 图片类型：normal/porn/suggestive/gore/violence/politics/gambling/drugs/terror/other_risk
	Score      float64 `json:"score"`       // 该图片类型的匹配分数 0.0~1.0
	RiskLevel  string  `json:"risk_level"`  // 综合风险等级：low / medium / high
	RiskReason string  `json:"risk_reason"` // 判定依据概述（便于人工复审/审计）
}

// 对外统一 JSON 响应结构（code + msg + data）：
//   - code = 200 表示成功；code = -1000 表示失败（msg 携带原因）；
//   - data 类型不固定，随场景需要变化（如分析结果）。
//
// 具体拼装逻辑见 internal/helper 包。
type Response struct {
	Code int         `json:"code"` // 业务码：200 成功 / -1000 失败
	Msg  string      `json:"msg"`  // 提示信息（英文）
	Data interface{} `json:"data"` // 业务数据，失败时为 null
}
