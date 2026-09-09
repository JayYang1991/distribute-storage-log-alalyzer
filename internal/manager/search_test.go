package manager

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dist-log-analyzer/internal/config"
	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/store"
	"dist-log-analyzer/internal/worker"
)

func TestSearchWithinArchiveAndFileFilter(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "search_test_*")
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

	srv := NewServer(cfg, st, nil)

	adminUser, _ := st.GetUserByUsername("admin")
	testToken := "test-session-token-search-001"
	srv.sessions.Store(testToken, adminUser.Username)

	// 构造测试解压目录，写入多个日志文件
	extractDir := filepath.Join(tmpDir, "extracted", "arc-search-01")
	_ = os.MkdirAll(filepath.Join(extractDir, "ceph"), 0755)

	monLog := "2026-09-09 10:00:01 [INF] mon.a started at 192.168.1.10:6789\n2026-09-09 10:00:02 [WRN] slow request 12 ops blocked > 30 sec\n2026-09-09 10:00:03 [ERR] cluster health degraded\n"
	osdLog := "2026-09-09 10:00:01 [INF] osd.0 heartbeat ok\n2026-09-09 10:00:02 [ERR] osd.1 marked down by monitor\n2026-09-09 10:00:03 [WRN] slow request waiting on peering\n"

	_ = os.WriteFile(filepath.Join(extractDir, "ceph", "ceph-mon.log"), []byte(monLog), 0644)
	_ = os.WriteFile(filepath.Join(extractDir, "ceph", "ceph-osd.log"), []byte(osdLog), 0644)

	archive := &model.LogArchive{
		ID:          "arc-search-01",
		Filename:    "ceph_debug_logs.tar.gz",
		Username:    adminUser.Username,
		UserID:      adminUser.ID,
		Format:      "tar.gz",
		Status:      "ready",
		ExtractPath: extractDir,
		UploadTime:  time.Now(),
	}
	_ = st.SaveArchive(archive)

	// 1. 归档包内全局检索 (不指定文件路径)：查找 "slow request"，应该匹配 2 个文件
	qGlobal := model.SearchQuery{
		ArchiveID: archive.ID,
		Keyword:   "slow request",
		Page:      1,
		PageSize:  10,
	}
	body, _ := json.Marshal(qGlobal)
	req := httptest.NewRequest(http.MethodPost, "/api/search", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()

	srv.handleSearch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("全包检索预期 200，实际: %d, body: %s", w.Code, w.Body.String())
	}

	var respGlobal model.SearchResponse
	_ = json.Unmarshal(w.Body.Bytes(), &respGlobal)
	if respGlobal.TotalHits != 2 {
		t.Fatalf("全包搜索 'slow request' 预期匹配 2 处，实际得到: %d", respGlobal.TotalHits)
	}

	// 2. 指定单个文件检索 (FilePath = "ceph-mon.log")：只在 mon.log 中搜索
	qSingleFile := model.SearchQuery{
		ArchiveID: archive.ID,
		Keyword:   "slow request",
		FilePath:  "ceph-mon.log",
		Page:      1,
		PageSize:  10,
	}
	body, _ = json.Marshal(qSingleFile)
	req = httptest.NewRequest(http.MethodPost, "/api/search", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	w = httptest.NewRecorder()

	srv.handleSearch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("单文件检索预期 200，实际: %d", w.Code)
	}

	var respSingle model.SearchResponse
	_ = json.Unmarshal(w.Body.Bytes(), &respSingle)
	if respSingle.TotalHits != 1 {
		t.Fatalf("过滤单文件后预期匹配 1 处，实际: %d", respSingle.TotalHits)
	}
	if filepath.Base(respSingle.Hits[0].FilePath) != "ceph-mon.log" {
		t.Errorf("匹配结果文件名预期为 ceph-mon.log，实际: %s", respSingle.Hits[0].FilePath)
	}

	// 3. 级别过滤搜索 (Level = "ERROR")
	qLevel := model.SearchQuery{
		ArchiveID: archive.ID,
		Level:     "ERROR",
		Page:      1,
		PageSize:  10,
	}
	respLevel, err := worker.SearchLogs(extractDir, &qLevel)
	if err != nil {
		t.Fatalf("SearchLogs 失败: %v", err)
	}
	if respLevel.TotalHits != 2 {
		t.Fatalf("级别过滤 ERROR 预期 2 处，实际: %d", respLevel.TotalHits)
	}
}

func TestFileContentPagingAndHasMore(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "content_test_*")
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

	srv := NewServer(cfg, st, nil)
	adminUser, _ := st.GetUserByUsername("admin")
	testToken := "test-session-token-content-002"
	srv.sessions.Store(testToken, adminUser.Username)

	extractDir := filepath.Join(tmpDir, "extracted", "arc-content-02")
	_ = os.MkdirAll(extractDir, 0755)

	// 写入 20 行日志
	var buffer bytes.Buffer
	for i := 1; i <= 20; i++ {
		buffer.WriteString(filepath.Join("line content", string(rune('A'+i-1))) + "\n")
	}
	_ = os.WriteFile(filepath.Join(extractDir, "audit.log"), buffer.Bytes(), 0644)

	archive := &model.LogArchive{
		ID:          "arc-content-02",
		Filename:    "audit.tar.gz",
		Username:    adminUser.Username,
		UserID:      adminUser.ID,
		Format:      "tar.gz",
		Status:      "ready",
		ExtractPath: extractDir,
		UploadTime:  time.Now(),
	}
	_ = st.SaveArchive(archive)

	// 请求第 1~5 行 (limit=5)
	req := httptest.NewRequest(http.MethodGet, "/api/archives/arc-content-02/file-content?path=audit.log&start_line=1&limit=5&token="+testToken, nil)
	w := httptest.NewRecorder()
	srv.handleArchiveItem(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("file-content 预期 200，实际: %d", w.Code)
	}

	var data map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &data)
	lines := data["lines"].([]interface{})
	if len(lines) != 5 {
		t.Errorf("预期返回 5 行，实际: %d", len(lines))
	}
	if data["has_more"] != true {
		t.Errorf("还有剩余行时 has_more 预期为 true，实际: %v", data["has_more"])
	}

	// 请求第 16~25 行 (limit=10，后无更多行)
	req2 := httptest.NewRequest(http.MethodGet, "/api/archives/arc-content-02/file-content?path=audit.log&start_line=16&limit=10&token="+testToken, nil)
	w2 := httptest.NewRecorder()
	srv.handleArchiveItem(w2, req2)

	var data2 map[string]interface{}
	_ = json.Unmarshal(w2.Body.Bytes(), &data2)
	lines2 := data2["lines"].([]interface{})
	if len(lines2) != 5 {
		t.Errorf("最后区间预期返回 5 行，实际: %d", len(lines2))
	}
	if data2["has_more"] != false {
		t.Errorf("已到文件末尾 has_more 预期为 false，实际: %v", data2["has_more"])
	}
}
