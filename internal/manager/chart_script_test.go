package manager

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dist-log-analyzer/internal/config"
	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/store"
)

func setupChartTestServer(t *testing.T) (*Server, *store.Store, func()) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.DataDir = t.TempDir()
	s, err := store.NewStore(cfg)
	if err != nil {
		t.Fatalf("failed to init store: %v", err)
	}

	srv := &Server{
		cfg:   cfg,
		store: s,
	}

	cleanup := func() {
		_ = s.Close()
	}
	return srv, s, cleanup
}

func TestChartScriptRules_ManagementAndPermissions(t *testing.T) {
	srv, s, cleanup := setupChartTestServer(t)
	defer cleanup()

	// 1. 验证内置默认脚本是否存在
	rules, err := s.ListChartScripts()
	if err != nil || len(rules) == 0 {
		t.Fatalf("内置图表脚本未正确初始化: %v", err)
	}
	foundIostat := false
	for _, r := range rules {
		if r.ID == "chart_script_iostat_default" {
			foundIostat = true
			break
		}
	}
	if !foundIostat {
		t.Fatalf("未找到默认预置的 iostat 图表脚本")
	}

	// 2. 普通用户试图添加脚本 -> 预期 403
	userToken := "test_token_normal_user"
	srv.sessions.Store(userToken, "normal_user")
	_ = s.SaveUser(&model.User{
		ID:       "user_normal",
		Username: "normal_user",
		Role:     model.RoleUser,
		Status:   "active",
	})

	newRule := model.ChartScriptRule{
		Name:          "自定义 vmstat 分析",
		FilePattern:   `(?i).*vmstat.*`,
		Interpreter:   "/usr/bin/python3",
		ScriptContent: `print('{"title":"test","x_axis":{"data":[]},"series":[]}')`,
		Enabled:       true,
	}
	reqBody, _ := json.Marshal(newRule)

	req := httptest.NewRequest(http.MethodPost, "/api/chart-scripts", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+userToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChartScripts(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("普通用户创建脚本应被 403 拦截，实际状态码: %d", w.Code)
	}

	// 3. 管理员添加脚本 -> 预期 201 Created
	adminToken := "test_token_admin"
	srv.sessions.Store(adminToken, "admin")

	reqAdmin := httptest.NewRequest(http.MethodPost, "/api/chart-scripts", bytes.NewReader(reqBody))
	reqAdmin.Header.Set("Authorization", "Bearer "+adminToken)
	reqAdmin.Header.Set("Content-Type", "application/json")
	wAdmin := httptest.NewRecorder()
	srv.handleChartScripts(wAdmin, reqAdmin)
	if wAdmin.Code != http.StatusCreated {
		t.Fatalf("管理员创建脚本失败，状态码: %d, 输出: %s", wAdmin.Code, wAdmin.Body.String())
	}

	var created model.ChartScriptRule
	if err := json.Unmarshal(wAdmin.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("解析创建的规则失败: %v", err)
	}

	// 4. 管理员修改脚本 -> 预期 200 OK
	created.Name = "修改后的 vmstat 分析"
	updateBody, _ := json.Marshal(created)
	reqUpdate := httptest.NewRequest(http.MethodPut, "/api/chart-scripts/"+created.ID, bytes.NewReader(updateBody))
	reqUpdate.Header.Set("Authorization", "Bearer "+adminToken)
	reqUpdate.Header.Set("Content-Type", "application/json")
	wUpdate := httptest.NewRecorder()
	srv.handleChartScriptItem(wUpdate, reqUpdate)
	if wUpdate.Code != http.StatusOK {
		t.Fatalf("修改脚本失败: %d", wUpdate.Code)
	}

	// 5. 管理员删除脚本 -> 预期 204
	reqDel := httptest.NewRequest(http.MethodDelete, "/api/chart-scripts/"+created.ID, nil)
	reqDel.Header.Set("Authorization", "Bearer "+adminToken)
	wDel := httptest.NewRecorder()
	srv.handleChartScriptItem(wDel, reqDel)
	if wDel.Code != http.StatusNoContent {
		t.Fatalf("删除脚本失败: %d", wDel.Code)
	}

	// 确认已被删除
	_, err = s.GetChartScript(created.ID)
	if err == nil {
		t.Fatalf("脚本已被删除，但依然能被检索到")
	}

	t.Logf("✔ 图表脚本权限隔离与管理员 CRUD 全生命周期测试通过！")
}

func TestChartScript_FileMatching(t *testing.T) {
	srv, s, cleanup := setupChartTestServer(t)
	defer cleanup()

	adminToken := "test_token_admin"
	srv.sessions.Store(adminToken, "admin")

	// 构造归档包记录
	archive := &model.LogArchive{
		ID:          "arc_test_chart",
		Username:    "admin",
		Status:      "ready",
		ExtractPath: t.TempDir(),
		UploadTime:  time.Now(),
	}
	_ = s.SaveArchive(archive)

	// 测试文件是否命中 iostat 默认规则
	targetFile := "sysstat/iostat-20260913.log"
	req := httptest.NewRequest(http.MethodGet, "/api/archives/arc_test_chart/chart-match?file="+targetFile, nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	w := httptest.NewRecorder()
	srv.handleArchiveItem(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("chart-match 接口调用失败: %d, %s", w.Code, w.Body.String())
	}

	var matched []*model.ChartScriptRule
	if err := json.Unmarshal(w.Body.Bytes(), &matched); err != nil {
		t.Fatalf("反序列化匹配规则失败: %v", err)
	}
	if len(matched) == 0 {
		t.Fatalf("未能成功匹配到 iostat 默认规则")
	}
	if matched[0].ID != "chart_script_iostat_default" {
		t.Fatalf("匹配规则不符合预期: %s", matched[0].ID)
	}

	// 测试不相关文件是否不会误命中
	unrelatedFile := "var/log/messages"
	req2 := httptest.NewRequest(http.MethodGet, "/api/archives/arc_test_chart/chart-match?file="+unrelatedFile, nil)
	req2.Header.Set("Authorization", "Bearer "+adminToken)
	w2 := httptest.NewRecorder()
	srv.handleArchiveItem(w2, req2)
	var matched2 []*model.ChartScriptRule
	_ = json.Unmarshal(w2.Body.Bytes(), &matched2)
	if len(matched2) != 0 {
		t.Fatalf("不相关文件错误匹配到了规则: %d", len(matched2))
	}

	t.Logf("✔ 文件名正则嗅探与匹配逻辑验证通过！")
}
