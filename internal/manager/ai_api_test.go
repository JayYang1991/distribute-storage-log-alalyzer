package manager

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"dist-log-analyzer/internal/config"
	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/store"
)

func TestAIAPIsAndStreaming(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ai_api_test_*")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := config.DefaultConfig()
	cfg.DataDir = tmpDir
	cfg.AI = model.AIConfig{
		Enabled:  true,
		Provider: "mock",
		Model:    "deepseek-r1:70b",
	}

	st, err := store.NewStore(cfg)
	if err != nil {
		t.Fatalf("创建 Store 失败: %v", err)
	}
	defer st.Close()

	srv := NewServer(cfg, st, http.Dir(tmpDir))

	// 创建测试管理员会话
	testToken := "test-admin-ai-token"
	srv.sessions.Store(testToken, "admin")
	_ = st.SaveUser(&model.User{
		Username: "admin",
		Role:     model.RoleAdmin,
	})

	// 1. 测试 GET /api/ai/config
	reqConfig := httptest.NewRequest(http.MethodGet, "/api/ai/config", nil)
	reqConfig.Header.Set("Authorization", "Bearer "+testToken)
	wConfig := httptest.NewRecorder()
	srv.handleAIConfig(wConfig, reqConfig)

	if wConfig.Code != http.StatusOK {
		t.Fatalf("GET /api/ai/config failed: code=%d, body=%s", wConfig.Code, wConfig.Body.String())
	}
	var curCfg model.AIConfig
	if err := json.Unmarshal(wConfig.Body.Bytes(), &curCfg); err != nil {
		t.Fatalf("Unmarshal AIConfig failed: %v", err)
	}
	if curCfg.Provider != "mock" {
		t.Errorf("Provider = %s, expected mock", curCfg.Provider)
	}

	// 2. 测试 POST /api/ai/config (更新为本地 ollama)
	updatePayload := model.AIConfig{
		Enabled:    true,
		Provider:   "mock",
		BaseURL:    "http://127.0.0.1:11434/v1",
		Model:      "qwen2.5:72b",
		TimeoutSec: 60,
	}
	upData, _ := json.Marshal(updatePayload)
	reqUpdate := httptest.NewRequest(http.MethodPost, "/api/ai/config", bytes.NewReader(upData))
	reqUpdate.Header.Set("Authorization", "Bearer "+testToken)
	wUpdate := httptest.NewRecorder()
	srv.handleAIConfig(wUpdate, reqUpdate)

	if wUpdate.Code != http.StatusOK {
		t.Fatalf("POST /api/ai/config failed: code=%d, body=%s", wUpdate.Code, wUpdate.Body.String())
	}

	// 3. 测试 GET /api/ai/knowledge
	reqKnow := httptest.NewRequest(http.MethodGet, "/api/ai/knowledge", nil)
	reqKnow.Header.Set("Authorization", "Bearer "+testToken)
	wKnow := httptest.NewRecorder()
	srv.handleAIKnowledge(wKnow, reqKnow)

	if wKnow.Code != http.StatusOK {
		t.Fatalf("GET /api/ai/knowledge failed: code=%d, body=%s", wKnow.Code, wKnow.Body.String())
	}
	var knowRes struct {
		Glossary   []model.AIGlossaryTerm       `json:"glossary"`
		Workflows  []model.AIWorkflowDefinition `json:"workflows"`
		NoiseRules []model.AINoiseRule          `json:"noise_rules"`
	}
	if err := json.Unmarshal(wKnow.Body.Bytes(), &knowRes); err != nil {
		t.Fatalf("Unmarshal knowledge failed: %v", err)
	}
	if len(knowRes.Glossary) == 0 || len(knowRes.Workflows) == 0 || len(knowRes.NoiseRules) == 0 {
		t.Errorf("默认知识库未能正确初始化: glossary=%d, workflows=%d, noise=%d",
			len(knowRes.Glossary), len(knowRes.Workflows), len(knowRes.NoiseRules))
	}

	// 4. 测试 POST /api/ai/noise (添加误报规则)
	noisePayload := map[string]string{
		"pattern": "ERR: test noise message",
		"reason":  "测试添加的已知误报",
	}
	npData, _ := json.Marshal(noisePayload)
	reqNoise := httptest.NewRequest(http.MethodPost, "/api/ai/noise", bytes.NewReader(npData))
	reqNoise.Header.Set("Authorization", "Bearer "+testToken)
	wNoise := httptest.NewRecorder()
	srv.handleAINoise(wNoise, reqNoise)

	if wNoise.Code != http.StatusOK {
		t.Fatalf("POST /api/ai/noise failed: code=%d, body=%s", wNoise.Code, wNoise.Body.String())
	}

	// 5. 测试 SSE 流式推理接口 handleAIDiagnosisStream
	archID := "arch-ai-test-99"
	_ = st.SaveArchive(&model.LogArchive{
		ID:       archID,
		Filename: "storage-cluster-crash.tar.gz",
		Username: "admin",
	})
	_ = st.SaveReport(&model.DiagnosisReport{
		ArchiveID:   archID,
		ArchiveName: "storage-cluster-crash.tar.gz",
		HealthScore: 50,
		SummaryText: "多副本写入超时与分块封装异常",
		Events: []model.DiagnosisEvent{
			{
				ID:             "ev-1",
				RuleName:       "PeerWalTimeout",
				Severity:       "CRITICAL",
				FilePath:       "chunkserver.log",
				LineNumber:     100,
				MatchedContent: "peer wal commit timeout on target replica node",
				Suggestion:     "排查底层磁盘队列与延迟",
			},
		},
		AnalyzedAt: time.Now(),
	})

	reqStream := httptest.NewRequest(http.MethodGet, "/api/reports/ai-stream/?archive_id="+archID+"&token="+testToken, nil)
	wStream := httptest.NewRecorder()
	srv.handleAIDiagnosisStream(wStream, reqStream)

	if wStream.Code != http.StatusOK {
		t.Fatalf("SSE Stream failed: code=%d, body=%s", wStream.Code, wStream.Body.String())
	}
	bodyStr := wStream.Body.String()
	if !strings.Contains(bodyStr, "data: ") {
		t.Errorf("SSE 输出未包含 data: 前缀")
	}
	if !strings.Contains(bodyStr, "故障时序传播链路") {
		t.Errorf("SSE 输出未包含传播链路报告内容: %s", bodyStr)
	}

	// 验证分析结果是否已持久化回 bbolt 数据库
	savedReport, err := st.GetReport(archID)
	if err != nil || savedReport.AIAnalysis == nil {
		t.Fatalf("AI 分析结果未成功持久化到 DiagnosisReport: %v", err)
	}
	if savedReport.AIAnalysis.Status != "completed" {
		t.Errorf("AI 分析状态非 completed: %s", savedReport.AIAnalysis.Status)
	}
	if !strings.Contains(savedReport.AIAnalysis.RawMarkdown, "根本原因分析") {
		t.Errorf("持久化 Markdown 报告不完整: %s", savedReport.AIAnalysis.RawMarkdown)
	}
}
