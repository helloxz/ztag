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
	"time"

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
	defaultCh  *config.ChannelConfig // 默认渠道（id=default，必填）
	backupCh   *config.ChannelConfig // 备用渠道（id=backup，可为 nil）
	mock       bool                  // 是否启用模拟 AI（联调用）
	timeout    time.Duration         // 单次 AI 调用超时
	maxRetries int                   // 单渠道内失败重试次数
}

// NewGateway 根据配置构建 AI 网关，按渠道 id 归类为 default / backup。
func NewGateway(cfg config.AIConfig) *Gateway {
	g := &Gateway{
		mock:       cfg.Mock,
		timeout:    time.Duration(cfg.Timeout) * time.Second,
		maxRetries: cfg.MaxRetries,
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
//   - 默认走 default 渠道（请求未指定渠道时）；
//   - default 调用失败且配置了 backup 渠道 → 自动切换到 backup 的第一个模型重试；
//   - 全部失败（或未配置 backup）→ 返回业务错误。
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

	// 2. 失败切换：仅当默认渠道失败且存在备用渠道时兜底一次
	if tgt.channel.ID == config.ChannelIDDefault && g.backupCh != nil {
		backupTarget := &target{channel: g.backupCh, model: g.backupCh.Models[0]}
		if result, backupErr := g.invoke(ctx, req, backupTarget); backupErr == nil {
			return result, nil
		} else {
			// 备用渠道也失败：全部失败，按 Error 级别记录
			slog.Error("AI request failed on backup channel",
				"channel", g.backupCh.ID, "model", g.backupCh.Models[0], "err", backupErr)
		}
		return nil, model.NewBizError("AI analysis failed on both channels")
	}

	// 3. 无备用渠道可切，直接返回失败（err 已含 AI analysis failed 前缀）
	slog.Error("AI request failed", "channel", tgt.channel.ID, "model", tgt.model, "err", err)
	return nil, model.NewBizError(err.Error())
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
		channel:    ch,
		modelName:  modelName,
		timeout:    g.timeout,
		maxRetries: g.maxRetries,
	}
}

// resolve 解析本次调用应使用哪个渠道、哪个模型：
//   - 渠道：请求 channel 字段为空时用 default；非空时按 id 匹配 default / backup；
//   - 模型：请求 model 字段须属于该渠道，否则取该渠道第一个模型。
func (g *Gateway) resolve(req *model.AnalyzeRequest) (*target, error) {
	// 1. 选择渠道（固定主备，无遍历开销）
	var ch *config.ChannelConfig
	switch req.Channel {
	case "":
		ch = g.defaultCh
	case config.ChannelIDDefault:
		ch = g.defaultCh
	case config.ChannelIDBackup:
		ch = g.backupCh
	default:
		return nil, model.NewBizError(
			fmt.Sprintf("channel id must be \"default\" or \"backup\", got: %q", req.Channel))
	}
	if ch == nil {
		return nil, model.NewBizError(
			fmt.Sprintf("channel %q is not configured", req.Channel))
	}

	// 2. 选择模型（在渠道模型列表中匹配，未命中则报参数错误）
	if req.Model != "" {
		for _, m := range ch.Models {
			if m == req.Model {
				return &target{channel: ch, model: m}, nil
			}
		}
		return nil, model.NewBizError(
			fmt.Sprintf("model %q does not exist in channel %q", req.Model, ch.ID))
	}

	// 未指定模型：默认取该渠道配置的第一个模型
	return &target{channel: ch, model: ch.Models[0]}, nil
}
