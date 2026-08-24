package ai

import (
	"context"

	"github.com/helloxz/ztag/internal/model"
)

// MockAnalyzer 模拟分析器：不调用任何大模型，直接返回固定结果，
// 供无 API Key 时的本地联调与端到端链路测试（config ai.mock = true）。
// 输出结构（keywords/description/classification）与真实模型保持一致。
type MockAnalyzer struct {
	channelID string // 渠道 id（default / backup，便于日志定位）
	modelName string // 模型标识
}

// AnalyzeImage 返回一组固定的模拟分析结果（二层分类结构）。
func (m *MockAnalyzer) AnalyzeImage(ctx context.Context, req *model.AnalyzeRequest) (*model.AnalyzeResult, error) {
	// 骨架/联调阶段忽略 ctx 与 req，仅返回固定结果
	// 示例按新约定：前 1-2 个为核心关键词，后 2-3 个为长尾关键词
	return &model.AnalyzeResult{
		Keywords:    []string{"自然风景", "高山湖泊森林风景照"},
		Description: "模拟输出：高清自然风景图片，蓝天白云倒映高山湖泊，画面宁静治愈，适合作为桌面壁纸与旅行灵感参考。",
		Classification: model.Classification{
			Category:   "normal",
			Score:      0.98,
			RiskLevel:  "low",
			RiskReason: "未检测到敏感内容。",
		},
		ModelID: m.modelName,
	}, nil
}
