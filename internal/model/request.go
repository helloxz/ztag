package model

// ImageInput 图片来源输入：URL 与 base64 二选一。
// 由图片分析请求与场景决策请求共用，保证两个接口的入参契约一致。
type ImageInput struct {
	ImageURL    string `json:"image_url"`    // 图片 URL（与 ImageBase64 二选一）
	ImageBase64 string `json:"image_base64"` // 图片 base64 内容（与 ImageURL 二选一）
}

// AnalyzeRequest 图片分析请求体。
// 调用方三选一提供图片：
//   - ImageURL：图片的 HTTP(S) 地址，由服务端下载；
//   - ImageBase64：图片的 base64 编码内容（data URI 或纯 base64 均可）。
//
// 后续如需支持 multipart 文件上传，可在该结构体上扩展 FormFile 字段。
type AnalyzeRequest struct {
	ImageInput
	Model       string `json:"model"`        // 可选：格式为 "channel/modelId"（如 "default/gpt-5.6-luna"），channel 仅允许 default/backup，modelId 可含斜杠；为空则由网关按负载均衡决定
	LoadBalance bool   `json:"load_balance"` // 可选：仅当 Model 为空时生效，true 表示从 default 渠道中随机选择一个模型；与 Model 互斥，Model 优先

	// 内部字段：由 service 层在下载/校验后填充，AI 网关据此构造图片消息，不参与对外 JSON
	ImageDataURI string `json:"-"` // 图片 base64 data URI（data:image/xxx;base64,...）
	ImageMIME    string `json:"-"` // 图片 MIME 类型（如 image/jpeg）
}

// DecisionRequest 场景决策（图片）请求体。
// Channel 可选：指定决策渠道 id（见 ai.decision.channels），为空则由服务端轮询。
type DecisionRequest struct {
	ImageInput
	Channel string `json:"channel"` // 可选：决策渠道 id

	// LegacyModel 已弃用：旧参数名 model（实际含义一直是渠道 id）。
	// 仅用于把老调用方的请求显式拒绝掉，避免被 JSON 绑定静默忽略后误按轮询处理。
	LegacyModel string `json:"model"`

	// ConvertWebP 可选：是否把图片压缩转为 WebP，缺省（不传）为 true。
	// 传 false 时为兼容模式：所有决策渠道统一规整为 JPEG ≤1568。
	ConvertWebP *bool `json:"convert_webp"`
}

// ShouldConvertWebP 返回是否需要压缩转为 WebP：缺省 true，保持历史行为。
func (r *DecisionRequest) ShouldConvertWebP() bool {
	return r == nil || r.ConvertWebP == nil || *r.ConvertWebP
}

// TextDecisionRequest 场景决策（文本）请求体。
type TextDecisionRequest struct {
	Text    string `json:"text"`    // 待审核文本
	Channel string `json:"channel"` // 可选：决策渠道 id

	// LegacyModel 已弃用：旧参数名 model，非空时由 service 层拒绝并提示改用 channel。
	LegacyModel string `json:"model"`
}

// PreparedImage 预处理完成的图片产物（下载/校验/压缩转 webp 后）。
// DataURI 为 data:image/xxx;base64,... 形式，MIME 为最终图片类型。
type PreparedImage struct {
	DataURI string // 图片 base64 data URI
	MIME    string // 图片 MIME（如 image/webp）
}

// DecisionResult 场景决策结果：只包含场景分类、风险评分与处理耗时。
type DecisionResult struct {
	Category  string `json:"category"`   // 场景分类（固定枚举，见 ai 层白名单）
	Score     int    `json:"score"`      // 风险评分 0~100：越大风险越高；99 为自然判定与普通拦截的上限，100 表示极危拦截（prohibited）
	Channel   string `json:"channel"`    // 实际处理的决策渠道 id（由 ai 层填充）
	ElapsedMs int64  `json:"elapsed_ms"` // 本次请求处理总耗时（毫秒），由 handler 层在响应前填充

	// 内部字段：供 service 层记录日志，不参与对外 JSON
	Confidence     float64            `json:"-"` // 模型对该分类的置信度 0~1
	Probabilities  map[string]float64 `json:"-"` // 各场景概率（调试用）
	BlockedCode    string             `json:"-"` // 供应商安全拦截错误码（如 safety / prohibited_content）
	BlockedMessage string             `json:"-"` // 供应商安全拦截错误描述
}

// 场景决策分类中由服务端合成的特殊分类（供应商安全策略直接拒绝输入，非模型分类）。
const (
	CategoryBlocked    = "blocked"    // 普通安全拦截（safety / image_safety / jailbreak 等），风险分固定 99
	CategoryProhibited = "prohibited" // 极危拦截（prohibited_content / blocklist，通常为 CSAM），风险分固定 100
)

// AnalyzeResult 图片分析结果（AI 网关返回的结构化产物）。
type AnalyzeResult struct {
	Keywords       []string       `json:"keywords"`       // 图片 SEO 关键词（中文，前 1-2 个核心 + 后 2-3 个长尾，共 3-5 个）
	Description    string         `json:"description"`    // 图片 SEO 描述（面向搜索优化，不超过 150 字，中文）
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
