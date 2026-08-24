// Package ai 是 AI 模型网关层，负责：
//   - 统一对外暴露图片分析能力（Analyzer 接口）；
//   - 固定主备双渠道（default / backup）的选择、模型匹配与失败切换；
//   - 支持 mock 模式（无 API Key 本地联调）。
//
// 业务层（service）只依赖本包的 Analyzer 接口，完全不感知底层
// 是哪一家厂商、哪种协议（openai-chat / openai-response / anthropic）。
package ai

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"math/rand/v2"

	"github.com/helloxz/ztag/internal/config"
	"github.com/helloxz/ztag/internal/model"
)

// Analyzer 统一的图片分析能力接口。
type Analyzer interface {
	// AnalyzeImage 分析一张图片，返回结构化结果（关键词、描述、分类明细）。
	AnalyzeImage(ctx context.Context, req *model.AnalyzeRequest) (*model.AnalyzeResult, error)
}

// Gateway 主备双渠道 AI 网关：持有 default（必填）与 backup（可选）渠道，
// 负责渠道与模型选择，并在默认渠道失败时自动切换备用渠道兜底。
type Gateway struct {
	defaultCh   *config.ChannelConfig // 默认渠道（id=default，必填）
	backupCh    *config.ChannelConfig // 备用渠道（id=backup，可为 nil）
	mock        bool                  // 是否启用模拟 AI（联调用）
	timeout     time.Duration         // 单次 AI 调用超时
	maxRetries  int                   // 单渠道内失败重试次数
	temperature float64               // 采样温度（越低输出越稳定）
	topP        float64               // 核采样概率（控制随机性）
}

// NewGateway 根据配置构建 AI 网关，按渠道 id 归类为 default / backup。
func NewGateway(cfg config.AIConfig) *Gateway {
	g := &Gateway{
		mock:        cfg.Mock,
		timeout:     time.Duration(cfg.Timeout) * time.Second,
		maxRetries:  cfg.MaxRetries,
		temperature: cfg.Temperature,
		topP:        cfg.TopP,
	}
	for i := range cfg.Channels {
		switch cfg.Channels[i].ID {
		case config.ChannelIDDefault:
			g.defaultCh = &cfg.Channels[i]
		case config.ChannelIDBackup:
			g.backupCh = &cfg.Channels[i]
		}
	}
	return g
}

// target 一次调用解析出的最终目标（渠道 + 模型标识）。
type target struct {
	channel *config.ChannelConfig // 选中的渠道
	model   string                // 选中的模型标识
}

// Analyze 执行一次图片分析，带主备切换：
//   - Model 非空时按 "channel/modelId" 精确解析（优先级高于 LoadBalance）；
//   - Model 为空且 LoadBalance=true 时随机选 default 模型，否则取 default 第一个；
//   - default 失败 → 随机选 backup 一个模型重试；backup 显式失败 → 再随机 backup 一个重试。
func (g *Gateway) Analyze(ctx context.Context, req *model.AnalyzeRequest) (*model.AnalyzeResult, error) {
	tgt, err := g.resolve(req)
	if err != nil {
		return nil, err
	}

	// 1. 主渠道调用
	result, err := g.invoke(ctx, req, tgt)
	if err == nil {
		return result, nil
	}
	// 记录主渠道失败原因（后续可能降级成功，故用 Warn 级别）
	slog.Warn("AI request failed on primary channel",
		"channel", tgt.channel.ID, "model", tgt.model, "err", err)

	// 2. 失败切换：存在备用渠道时兜底一次（随机选 backup 模型）
	if g.backupCh == nil {
		slog.Error("AI request failed", "channel", tgt.channel.ID, "model", tgt.model, "err", err)
		return nil, model.NewBizError(err.Error())
	}
	// 随机选择 backup 模型；若主目标已是 backup 且抽到同一模型则尽量避开
	backupModel := g.pickRandomModel(g.backupCh)
	if tgt.channel.ID == config.ChannelIDBackup && backupModel == tgt.model && len(g.backupCh.Models) > 1 {
		for i := 0; i < 3 && backupModel == tgt.model; i++ {
			backupModel = g.pickRandomModel(g.backupCh)
		}
		if backupModel == tgt.model {
			for _, m := range g.backupCh.Models {
				if m != tgt.model {
					backupModel = m
					break
				}
			}
		}
	}
	backupTarget := &target{channel: g.backupCh, model: backupModel}
	if result, backupErr := g.invoke(ctx, req, backupTarget); backupErr == nil {
		return result, nil
	} else {
		// 备用渠道也失败：全部失败，按 Error 级别记录
		slog.Error("AI request failed on backup channel",
			"channel", g.backupCh.ID, "model", backupModel, "err", backupErr)
	}
	return nil, model.NewBizError("AI analysis failed on both channels")
}

// invoke 按目标创建分析器并执行分析。
func (g *Gateway) invoke(ctx context.Context, req *model.AnalyzeRequest, tgt *target) (*model.AnalyzeResult, error) {
	return g.newAnalyzer(tgt.channel, tgt.model).AnalyzeImage(ctx, req)
}

// newAnalyzer 分析器工厂：mock 模式下返回模拟实现，否则返回基于 goai 的真实实现。
func (g *Gateway) newAnalyzer(ch *config.ChannelConfig, modelName string) Analyzer {
	if g.mock {
		return &MockAnalyzer{channelID: ch.ID, modelName: modelName}
	}
	return &goaiAnalyzer{
		channel:     ch,
		modelName:   modelName,
		timeout:     g.timeout,
		maxRetries:  g.maxRetries,
		temperature: g.temperature,
		topP:        g.topP,
	}
}

// resolve 解析本次调用应使用哪个渠道、哪个模型：
//   - Model 非空时必须为 "channel/modelId" 格式（严格含 /），按第一个 / 切分；
//   - Model 为空时按 LoadBalance 决定：true 随机选 default，否则取 default 第一个；
//   - Model 优先级高于 LoadBalance（同时传时忽略 LoadBalance）。
func (g *Gateway) resolve(req *model.AnalyzeRequest) (*target, error) {
	// 指定 model 优先：必须为 "channel/modelId" 格式
	if req.Model != "" {
		if req.LoadBalance {
			slog.Warn("both model and load_balance provided, model takes precedence",
				"model", req.Model)
		}
		idx := strings.Index(req.Model, "/")
		if idx <= 0 || idx == len(req.Model)-1 {
			return nil, model.NewBizError(
				fmt.Sprintf("model must be in format \"channel/model\", got: %q", req.Model))
		}
		channelID := req.Model[:idx]
		modelID := req.Model[idx+1:]
		var ch *config.ChannelConfig
		switch channelID {
		case config.ChannelIDDefault:
			ch = g.defaultCh
		case config.ChannelIDBackup:
			ch = g.backupCh
		default:
			return nil, model.NewBizError(
				fmt.Sprintf("channel id must be \"default\" or \"backup\", got: %q", channelID))
		}
		if ch == nil {
			return nil, model.NewBizError(
				fmt.Sprintf("channel %q is not configured", channelID))
		}
		for _, m := range ch.Models {
			if m == modelID {
				return &target{channel: ch, model: m}, nil
			}
		}
		return nil, model.NewBizError(
			fmt.Sprintf("model %q does not exist in channel %q", modelID, channelID))
	}

	// 未指定 model：按 LoadBalance 决定 default 模型
	if g.defaultCh == nil {
		return nil, model.NewBizError("default channel is not configured")
	}
	if req.LoadBalance {
		return &target{channel: g.defaultCh, model: g.pickRandomModel(g.defaultCh)}, nil
	}
	return &target{channel: g.defaultCh, model: g.defaultCh.Models[0]}, nil
}

// pickRandomModel 从渠道模型列表中随机选一个（单次尝试）。
func (g *Gateway) pickRandomModel(ch *config.ChannelConfig) string {
	if len(ch.Models) == 1 {
		return ch.Models[0]
	}
	return ch.Models[rand.IntN(len(ch.Models))]
}
