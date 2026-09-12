package manager

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"dist-log-analyzer/internal/config"
	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/store"
)

func TestPreprocessRulesManagementAndOnlineTest(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "prep_test_*")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := config.DefaultConfig()
	cfg.DataDir = tmpDir
	st, err := store.NewStore(cfg)
	if err != nil {
		t.Fatalf("创建 Store 失败: %v", err)
	}
	defer st.Close()

	srv := &Server{
		cfg:   cfg,
		store: st,
	}

	// 1. 验证开箱即用的预设预处理规则初始化
	initRules, err := st.ListPreprocessRules()
	if err != nil || len(initRules) == 0 {
		t.Fatalf("预期内置预处理规则已初始化，实际获得: %d 条, err=%v", len(initRules), err)
	}

	// 2. 模拟管理员添加自定义预处理规则: 将 tenant_xxx 通配为 <*>
	adminToken := "test_token_admin"
	srv.sessions.Store(adminToken, "admin")

	newRule := model.PreprocessRule{
		Name:        "订单号与用户前缀泛化",
		Type:        model.PreprocessTypeMask,
		Pattern:     `\b(?:order|user)_\d+\b`,
		Replacement: "<*>",
		Description: "订单号动态变量泛化",
		Enabled:     true,
		Order:       5,
	}
	body, _ := json.Marshal(newRule)
	req := httptest.NewRequest(http.MethodPost, "/api/preprocess/rules", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	w := httptest.NewRecorder()

	srv.handlePreprocessRules(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("管理员添加预处理规则失败: code=%d, body=%s", w.Code, w.Body.String())
	}

	var created model.PreprocessRule
	_ = json.NewDecoder(w.Body).Decode(&created)
	if created.ID == "" || created.Pattern != newRule.Pattern {
		t.Fatalf("返回创建规则异常: %+v", created)
	}

	// 3. 在线清洗测试接口: /api/preprocess/test
	testPayload := map[string]interface{}{
		"sample_log":    "2026-09-13 12:00:00 [ERR] processing request for order_123456 failed",
		"use_persisted": true,
	}
	testBody, _ := json.Marshal(testPayload)
	testReq := httptest.NewRequest(http.MethodPost, "/api/preprocess/test", bytes.NewReader(testBody))
	testW := httptest.NewRecorder()

	srv.handlePreprocessTest(testW, testReq)
	if testW.Code != http.StatusOK {
		t.Fatalf("在线预处理测试失败: code=%d, body=%s", testW.Code, testW.Body.String())
	}

	var testRes struct {
		RawLog    string `json:"raw_log"`
		Cleaned   string `json:"cleaned"`
		Level     string `json:"level"`
		Timestamp string `json:"timestamp"`
	}
	_ = json.NewDecoder(testW.Body).Decode(&testRes)

	// 验证日志级别缩写 [ERR] 自动转换为 ERROR
	if testRes.Level != "ERROR" {
		t.Errorf("expected level to be converted to ERROR, got: %s", testRes.Level)
	}
	// 验证自定义掩码生效: order_123456 替换为 <*>
	if bytes.Contains([]byte(testRes.Cleaned), []byte("order_123456")) {
		t.Errorf("expected order_123456 to be masked, got: %s", testRes.Cleaned)
	}

	// 4. 普通用户禁止添加规则校验 (403 Forbidden)
	normalToken := "test_token_user"
	srv.sessions.Store(normalToken, "user")

	reqForbidden := httptest.NewRequest(http.MethodPost, "/api/preprocess/rules", bytes.NewReader(body))
	reqForbidden.Header.Set("Authorization", "Bearer "+normalToken)
	wForbidden := httptest.NewRecorder()
	srv.handlePreprocessRules(wForbidden, reqForbidden)
	if wForbidden.Code != http.StatusForbidden {
		t.Fatalf("普通用户未被拒绝，预期 403，实际: %d", wForbidden.Code)
	}
}
