package ai

import (
	"context"
	"strings"
	"testing"
	"time"

	"dist-log-analyzer/internal/model"
)

func TestContextAssembler(t *testing.T) {
	glossary := GetDefaultGlossary()
	workflows := GetDefaultWorkflows()
	noiseRules := GetDefaultNoiseRules()

	ca := NewContextAssembler(AssemblerOptions{
		GlossaryTerms: glossary,
		Workflows:     workflows,
		NoiseRules:    noiseRules,
	})

	// 模拟混合了专有术语、业务流程和误打日志的真实诊断报告
	report := &model.DiagnosisReport{
		ArchiveID:   "arch-test-1",
		ArchiveName: "ceph-cluster-crash.tar.gz",
		HealthScore: 45,
		SummaryText: "集群检测到严重副本延迟与写入异常",
		Events: []model.DiagnosisEvent{
			// 1. 误打的假错误 (应该被过滤到 IgnoredNoises)
			{
				ID:             "ev-1",
				RuleName:       "ConfigMissing",
				Severity:       "ERROR",
				FilePath:       "client.log",
				LineNumber:     12,
				MatchedContent: "ERR: lease file not found, creating new one for session",
				Suggestion:     "请检查配置文件",
			},
			// 2. 真实严重错误：包含内部专有术语 CHUNK_SEAL
			{
				ID:             "ev-2",
				RuleName:       "ChunkSealTimeout",
				Severity:       "CRITICAL",
				FilePath:       "chunkserver-3.log",
				LineNumber:     891,
				MatchedContent: "failed to seal chunk 102941: timeout waiting for replica commit",
				Suggestion:     "检查从副本落盘延迟",
				Timestamp:      "2026-09-13 10:15:05",
			},
			// 3. 业务流程卡滞事件 (符合 PEER_WAL_COMMIT)
			{
				ID:             "ev-3",
				RuleName:       "PeerWalTimeout",
				Severity:       "FATAL",
				FilePath:       "chunkserver-3.log",
				LineNumber:     905,
				MatchedContent: "peer wal commit timeout on target node 192.168.1.13",
				Suggestion:     "检查物理磁盘健康",
				Timestamp:      "2026-09-13 10:15:06",
			},
		},
		AnalyzedAt: time.Now(),
	}

	res := ca.Assemble(report)

	// 1. 验证误报过滤
	if len(res.IgnoredNoises) != 1 {
		t.Fatalf("预期过滤 1 条误打日志，实际得到 %d 条: %v", len(res.IgnoredNoises), res.IgnoredNoises)
	}
	if !strings.Contains(res.IgnoredNoises[0], "lease file not found") {
		t.Errorf("过滤内容异常: %s", res.IgnoredNoises[0])
	}

	// 2. 验证清洗后的事件列表 (排除假告警后剩 2 条)
	if len(res.CleanedEvents) != 2 {
		t.Fatalf("预期保留 2 条真实事件，实际得到 %d 条", len(res.CleanedEvents))
	}

	// 3. 验证专有术语命中 (应该命中 CHUNK_SEAL 和 PEER_WAL_COMMIT)
	var foundSeal, foundWal bool
	for _, term := range res.MatchedTerms {
		if term.Term == "CHUNK_SEAL" {
			foundSeal = true
		}
		if term.Term == "PEER_WAL_COMMIT" {
			foundWal = true
		}
	}
	if !foundSeal {
		t.Errorf("未能成功识别专有术语 CHUNK_SEAL")
	}
	if !foundWal {
		t.Errorf("未能成功识别专有术语 PEER_WAL_COMMIT")
	}

	// 4. 验证业务流程状态机断裂点定位
	if res.AffectedWorkflow == nil {
		t.Fatalf("未能成功识别受影响业务流程")
	}
	if res.AffectedWorkflow.ID != "WRITE_DATA_FLOW" {
		t.Errorf("流程识别不符: %s", res.AffectedWorkflow.ID)
	}
	if res.BrokenStage != "PEER_WAL_COMMIT" {
		t.Errorf("断裂阶段识别不符: %s (预期 PEER_WAL_COMMIT)", res.BrokenStage)
	}

	// 5. 验证 Prompt 内容完整度
	if !strings.Contains(res.Prompt, "CHUNK_SEAL") {
		t.Errorf("Prompt 未包含术语释义")
	}
	if !strings.Contains(res.Prompt, "PEER_WAL_COMMIT") {
		t.Errorf("Prompt 未包含断裂阶段")
	}
	if !strings.Contains(res.Prompt, "已排除的【误打/良性干扰日志】") {
		t.Errorf("Prompt 未包含消噪说明")
	}
}

func TestMockStreamingClient(t *testing.T) {
	client := NewClient(model.AIConfig{
		Provider: "mock",
	})

	ca := NewContextAssembler(AssemblerOptions{
		GlossaryTerms: GetDefaultGlossary(),
		Workflows:     GetDefaultWorkflows(),
		NoiseRules:    GetDefaultNoiseRules(),
	})

	rep := &model.DiagnosisReport{
		ArchiveName: "test.tar.gz",
		HealthScore: 60,
		Events: []model.DiagnosisEvent{
			{
				RuleName:       "IOHang",
				Severity:       "CRITICAL",
				MatchedContent: "peer wal commit timeout",
			},
		},
	}
	asm := ca.Assemble(rep)

	var streamOutput strings.Builder
	err := client.StreamChat(context.Background(), asm.Prompt, asm, func(chunk string) error {
		streamOutput.WriteString(chunk)
		return nil
	})

	if err != nil {
		t.Fatalf("StreamChat failed: %v", err)
	}

	result := streamOutput.String()
	if !strings.Contains(result, "故障时序传播链路") {
		t.Errorf("输出未包含传播链路: %s", result)
	}
	if !strings.Contains(result, "根本原因分析") {
		t.Errorf("输出未包含根因分析: %s", result)
	}
}
