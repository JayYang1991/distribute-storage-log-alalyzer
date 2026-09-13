package ai

import (
	"context"
	"strings"
	"sync"
	"time"

	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/store"
)

// Service AI 分析与领域知识管理中枢
type Service struct {
	store     *store.Store
	mu        sync.RWMutex
	cfg       model.AIConfig
	client    *Client
	assembler *ContextAssembler
}

// NewService 初始化 AI Service
func NewService(s *store.Store, defaultCfg model.AIConfig) (*Service, error) {
	svc := &Service{
		store: s,
		cfg:   defaultCfg,
	}

	// 1. 从 store 尝试加载用户自定义配置，若无则初始化默认值
	if savedCfg, err := s.GetAIConfig(); err == nil && savedCfg != nil {
		svc.cfg = *savedCfg
	} else {
		_ = s.SaveAIConfig(&svc.cfg)
	}

	// 2. 检查并初始化专有术语词典
	glossary, err := s.GetAIGlossary()
	if err != nil || len(glossary) == 0 {
		glossary = GetDefaultGlossary()
		_ = s.SaveAIGlossary(glossary)
	}

	// 3. 检查并初始化核心业务流程
	workflows, err := s.GetAIWorkflows()
	if err != nil || len(workflows) == 0 {
		workflows = GetDefaultWorkflows()
		_ = s.SaveAIWorkflows(workflows)
	}

	// 4. 检查并初始化误报抑制规则
	noiseRules, err := s.GetAINoiseRules()
	if err != nil || len(noiseRules) == 0 {
		noiseRules = GetDefaultNoiseRules()
		_ = s.SaveAINoiseRules(noiseRules)
	}

	// 5. 初始化 Client 与 Assembler
	svc.client = NewClient(svc.cfg)
	svc.assembler = NewContextAssembler(AssemblerOptions{
		GlossaryTerms: glossary,
		Workflows:     workflows,
		NoiseRules:    noiseRules,
	})

	return svc, nil
}

// ReloadKnowledge 重新加载知识库 (术语、流程、误报规则)
func (s *Service) ReloadKnowledge() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	glossary, _ := s.store.GetAIGlossary()
	workflows, _ := s.store.GetAIWorkflows()
	noiseRules, _ := s.store.GetAINoiseRules()

	s.assembler = NewContextAssembler(AssemblerOptions{
		GlossaryTerms: glossary,
		Workflows:     workflows,
		NoiseRules:    noiseRules,
	})
	return nil
}

// UpdateConfig 更新 AI 配置并重启客户端
func (s *Service) UpdateConfig(cfg model.AIConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.store.SaveAIConfig(&cfg); err != nil {
		return err
	}
	s.cfg = cfg
	s.client = NewClient(cfg)
	return nil
}

// GetConfig 获取当前配置
func (s *Service) GetConfig() model.AIConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// StreamAnalyzeReport 执行针对特定报告的流式 AI 诊断
func (s *Service) StreamAnalyzeReport(ctx context.Context, rep *model.DiagnosisReport, onChunk StreamCallback) (*model.AIAnalysisResult, error) {
	s.mu.RLock()
	client := s.client
	assembler := s.assembler
	cfg := s.cfg
	s.mu.RUnlock()

	// 1. 装配高纯度上下文与 Prompt
	asm := assembler.Assemble(rep)

	var fullOutput strings.Builder
	streamWrapper := func(chunk string) error {
		fullOutput.WriteString(chunk)
		return onChunk(chunk)
	}

	startTime := time.Now()
	err := client.StreamChat(ctx, asm.Prompt, asm, streamWrapper)

	rawMarkdown := fullOutput.String()

	// 构造持久化分析结果
	result := &model.AIAnalysisResult{
		Status:           "completed",
		ModelName:        cfg.Model,
		BrokenStage:      asm.BrokenStage,
		IgnoredNoises:    asm.IgnoredNoises,
		RawMarkdown:      rawMarkdown,
		AnalyzedAt:       time.Now(),
		PromptTokens:     len(asm.Prompt) / 4,
		CompletionTokens: len(rawMarkdown) / 4,
	}

	if err != nil {
		result.Status = "failed"
		if rawMarkdown == "" {
			return nil, err
		}
	}

	// 提取根因推断摘要
	if len(asm.CleanedEvents) > 0 {
		result.RootCause = asm.CleanedEvents[0].RuleName + " (" + asm.CleanedEvents[0].MatchedContent + ")"
		if asm.BrokenStage != "" {
			result.PropagationChain = []string{
				"阶段阻塞: " + asm.BrokenStage,
				"节点告警: " + asm.CleanedEvents[0].RuleName,
				"业务超时",
			}
		}
	} else {
		result.RootCause = "未检出高危存储故障"
	}

	_ = startTime // 预留指标采集
	return result, nil
}
