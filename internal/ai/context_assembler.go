package ai

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"dist-log-analyzer/internal/model"
)

// AssemblerOptions 上下文组装选项
type AssemblerOptions struct {
	GlossaryTerms []model.AIGlossaryTerm
	Workflows     []model.AIWorkflowDefinition
	NoiseRules    []model.AINoiseRule
}

// ContextAssembler AI 上下文组装与降噪引擎
type ContextAssembler struct {
	terms      []model.AIGlossaryTerm
	workflows  []model.AIWorkflowDefinition
	noiseRegex []*compiledNoise
}

type compiledNoise struct {
	rule *model.AINoiseRule
	reg  *regexp.Regexp
}

// NewContextAssembler 实例化上下文装配器
func NewContextAssembler(opts AssemblerOptions) *ContextAssembler {
	ca := &ContextAssembler{
		terms:     opts.GlossaryTerms,
		workflows: opts.Workflows,
	}
	for i := range opts.NoiseRules {
		r := &opts.NoiseRules[i]
		if !r.Enabled || strings.TrimSpace(r.Pattern) == "" {
			continue
		}
		if reg, err := regexp.Compile(r.Pattern); err == nil {
			ca.noiseRegex = append(ca.noiseRegex, &compiledNoise{
				rule: r,
				reg:  reg,
			})
		}
	}
	return ca
}

// AssemblyResult 上下文提炼与组装结果
type AssemblyResult struct {
	Prompt           string
	MatchedTerms     []model.AIGlossaryTerm
	AffectedWorkflow *model.AIWorkflowDefinition
	BrokenStage      string
	IgnoredNoises    []string
	CleanedEvents    []model.DiagnosisEvent
}

// Assemble 过滤伪告警、提取术语、投影流程并生成用于大模型的高纯度结构化 Prompt
func (ca *ContextAssembler) Assemble(rep *model.DiagnosisReport) AssemblyResult {
	res := AssemblyResult{
		CleanedEvents: make([]model.DiagnosisEvent, 0),
		IgnoredNoises: make([]string, 0),
		MatchedTerms:  make([]model.AIGlossaryTerm, 0),
	}

	if rep == nil {
		return res
	}

	// 1. 过滤假告警与已知误打日志 (Noise Filtering)
	for _, ev := range rep.Events {
		isNoise := false
		var noiseReason string
		for _, cn := range ca.noiseRegex {
			if cn.reg.MatchString(ev.MatchedContent) {
				isNoise = true
				noiseReason = cn.rule.Reason
				break
			}
		}

		if isNoise {
			res.IgnoredNoises = append(res.IgnoredNoises, fmt.Sprintf("[%s L%d] %s (排除原因: %s)",
				ev.FilePath, ev.LineNumber, strings.TrimSpace(ev.MatchedContent), noiseReason))
		} else {
			res.CleanedEvents = append(res.CleanedEvents, ev)
		}
	}

	// 2. 动态扫描内部专业术语与缩写 (Glossary Matching)
	termSet := make(map[string]bool)
	allText := rep.SummaryText + " "
	for _, ev := range res.CleanedEvents {
		allText += ev.RuleName + " " + ev.MatchedContent + " " + ev.Suggestion + " "
	}
	allTextUpper := strings.ToUpper(allText)

	for _, t := range ca.terms {
		matched := false
		if strings.Contains(allTextUpper, strings.ToUpper(t.Term)) {
			matched = true
		}
		if !matched {
			for _, pat := range t.CommonPatterns {
				if strings.Contains(allTextUpper, strings.ToUpper(pat)) {
					matched = true
					break
				}
			}
		}
		if matched && !termSet[t.Term] {
			termSet[t.Term] = true
			res.MatchedTerms = append(res.MatchedTerms, t)
		}
	}

	// 3. 业务流程状态机投影与断裂点分析 (Workflow Projection)
	res.AffectedWorkflow, res.BrokenStage = ca.projectWorkflow(res.CleanedEvents)

	// 4. 构建结构化高纯度 Prompt
	res.Prompt = ca.renderPrompt(rep, res)

	return res
}

// projectWorkflow 投影事件到业务流程并检测卡滞/断裂阶段
func (ca *ContextAssembler) projectWorkflow(events []model.DiagnosisEvent) (*model.AIWorkflowDefinition, string) {
	if len(ca.workflows) == 0 || len(events) == 0 {
		return nil, ""
	}

	for i := range ca.workflows {
		wf := &ca.workflows[i]
		for _, st := range wf.Stages {
			stageUpper := strings.ToUpper(st.Name)
			stageWords := strings.ReplaceAll(stageUpper, "_", " ")
			var reg *regexp.Regexp
			if st.SuccessPattern != "" {
				reg, _ = regexp.Compile(st.SuccessPattern)
			}
			for _, ev := range events {
				textUpper := strings.ToUpper(ev.RuleName + " " + ev.MatchedContent)
				if strings.Contains(textUpper, stageUpper) ||
					strings.Contains(textUpper, stageWords) ||
					(reg != nil && reg.MatchString(ev.MatchedContent)) {
					return wf, st.Name
				}
			}
		}
	}

	return nil, ""
}

// renderPrompt 组装提供给大模型输入的结构化 Prompt
func (ca *ContextAssembler) renderPrompt(rep *model.DiagnosisReport, asm AssemblyResult) string {
	var sb strings.Builder

	sb.WriteString("你是一位专精于企业自研分布式存储系统的资深内核与存储SRE排障专家。\n")
	sb.WriteString("系统规则引擎已完成海量日志的初步特征扫描与噪音过滤，请基于以下经过提炼的高纯度事实，深入分析故障传播链并推断底层根因(RCA)。\n\n")

	// 一、基本环境与健康态
	sb.WriteString("### 一、日志归档与基础指标概览\n")
	sb.WriteString(fmt.Sprintf("- **日志包名称**: %s\n", rep.ArchiveName))
	sb.WriteString(fmt.Sprintf("- **系统健康评分**: %d/100 (已检出高危特征数: %d)\n", rep.HealthScore, len(asm.CleanedEvents)))
	if rep.SummaryText != "" {
		sb.WriteString(fmt.Sprintf("- **规则初筛摘要**: %s\n", rep.SummaryText))
	}
	sb.WriteString("\n")

	// 二、内部专有术语释义
	if len(asm.MatchedTerms) > 0 {
		sb.WriteString("### 二、本次分析涉及的企业内部专有术语与机制释义 (请严格依据以下定义分析，勿主观臆测):\n")
		for _, t := range asm.MatchedTerms {
			sb.WriteString(fmt.Sprintf("1. **[%s] (%s)** - 所属模块: `%s`\n", t.Term, t.FullName, t.Module))
			sb.WriteString(fmt.Sprintf("   - **机理定义**: %s\n", t.Definition))
			if t.FailureImpact != "" {
				sb.WriteString(fmt.Sprintf("   - **连锁影响**: %s\n", t.FailureImpact))
			}
		}
		sb.WriteString("\n")
	}

	// 三、业务流程断裂分析
	if asm.AffectedWorkflow != nil {
		sb.WriteString("### 三、关联的核心业务流程状态机与异常断裂点\n")
		sb.WriteString(fmt.Sprintf("- **业务流程**: %s (`%s`)\n", asm.AffectedWorkflow.Name, asm.AffectedWorkflow.ID))
		sb.WriteString(fmt.Sprintf("- **流程简介**: %s\n", asm.AffectedWorkflow.Description))
		sb.WriteString(fmt.Sprintf("- **定位到的流程阻断/异常阶段**: ❌ **`%s`**\n", asm.BrokenStage))
		sb.WriteString("- **标准阶段时序参照**:\n")
		for _, st := range asm.AffectedWorkflow.Stages {
			mark := "✔"
			if st.Name == asm.BrokenStage {
				mark = "❌ [异常受阻]"
			}
			sb.WriteString(fmt.Sprintf("  - 步骤 %d: %s (%s ➔ %s) %s\n", st.Step, st.Name, st.SourceModule, st.TargetModule, mark))
		}
		sb.WriteString("\n")
	}

	// 四、已排除的伪错误与干扰项
	if len(asm.IgnoredNoises) > 0 {
		sb.WriteString("### 四、经系统预检已排除的【误打/良性干扰日志】(请勿采纳为本次故障的根因):\n")
		limit := len(asm.IgnoredNoises)
		if limit > 5 {
			limit = 5
		}
		for i := 0; i < limit; i++ {
			sb.WriteString(fmt.Sprintf("- %s\n", asm.IgnoredNoises[i]))
		}
		if len(asm.IgnoredNoises) > 5 {
			sb.WriteString(fmt.Sprintf("- *(另有 %d 条良性初始化/探针日志已自动抑制)*\n", len(asm.IgnoredNoises)-5))
		}
		sb.WriteString("\n")
	}

	// 五、高可信时序异常事实
	sb.WriteString("### 五、高可信时序故障事件清单 (按时间发生顺序)\n")
	if len(asm.CleanedEvents) == 0 {
		sb.WriteString("*(当前未检出高危致命事件，日志整体平稳)*\n")
	} else {
		// 优先展示 FATAL / CRITICAL 事件
		sort.SliceStable(asm.CleanedEvents, func(i, j int) bool {
			sevWeight := map[string]int{"FATAL": 4, "CRITICAL": 3, "ERROR": 2, "WARNING": 1}
			return sevWeight[asm.CleanedEvents[i].Severity] > sevWeight[asm.CleanedEvents[j].Severity]
		})

		maxShow := 15
		if len(asm.CleanedEvents) < maxShow {
			maxShow = len(asm.CleanedEvents)
		}
		for i := 0; i < maxShow; i++ {
			ev := asm.CleanedEvents[i]
			timeStr := ev.Timestamp
			if timeStr == "" {
				timeStr = "时序点"
			}
			sb.WriteString(fmt.Sprintf("- **[%s] [%s] [%s]** `位置: %s:L%d`\n",
				timeStr, ev.Severity, ev.RuleName, ev.FilePath, ev.LineNumber))
			sb.WriteString(fmt.Sprintf("  - 日志特征内容: `%s`\n", strings.TrimSpace(ev.MatchedContent)))
			if ev.Suggestion != "" {
				sb.WriteString(fmt.Sprintf("  - 专家预置线索: %s\n", ev.Suggestion))
			}
		}
		if len(asm.CleanedEvents) > maxShow {
			sb.WriteString(fmt.Sprintf("- *(其余 %d 处同类或次级事件已聚合)*\n", len(asm.CleanedEvents)-maxShow))
		}
	}
	sb.WriteString("\n")

	// 六、严格格式输出指令
	sb.WriteString("### 六、专家任务要求与输出格式规范\n")
	sb.WriteString("请严格按照以下四个核心章节输出 Markdown 报告：\n\n")
	sb.WriteString("#### 1. 【故障时序传播链路】\n")
	sb.WriteString("以清晰的单向箭头推演故障从起源到波及扩散的完整级联路径。格式样例：\n")
	sb.WriteString("`底层组件/节点 (源头起因) ➔ 中间复制/协调层 (级联超时) ➔ 上层客户端/网关 (业务受损表象)`\n\n")
	sb.WriteString("#### 2. 【根本原因分析 (RCA)】\n")
	sb.WriteString("明确指出最底层的物理或逻辑根因（明确是磁盘硬件瓶颈、网络丢包抖动、配置容量溢出、还是并发逻辑缺陷），并结合内部专有术语解释为什么会发生断裂。\n\n")
	sb.WriteString("#### 3. 【业务流程受阻评估】\n")
	sb.WriteString("说明故障在哪一个业务时序阶段卡滞，并解释该卡滞是如何导致上层应用产生不可用超时的。\n\n")
	sb.WriteString("#### 4. 【可执行的专家处置行动方案】\n")
	sb.WriteString("给出针对底层根因节点的具体命令行（CLI）排查排障步骤、应急恢复操作及长期架构防范建议。\n")

	return sb.String()
}
