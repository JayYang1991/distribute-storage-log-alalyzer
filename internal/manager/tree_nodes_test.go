package manager

import (
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
)

func TestTreeNodesAPI(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "test_tree_nodes_*")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tempDir)

	extractDir := filepath.Join(tempDir, "extracted")
	_ = os.MkdirAll(filepath.Join(extractDir, "var", "log"), 0755)
	_ = os.WriteFile(filepath.Join(extractDir, "var", "log", "syslog"), []byte("log 1\n"), 0644)
	_ = os.WriteFile(filepath.Join(extractDir, "root.txt"), []byte("root file\n"), 0644)

	cfg := config.DefaultConfig()
	cfg.DataDir = tempDir
	st, err := store.NewStore(cfg)
	if err != nil {
		t.Fatalf("初始化 store 失败: %v", err)
	}
	defer st.Close()

	server := NewServer(cfg, st, nil)

	adminUser, _ := st.GetUserByUsername("admin")
	testToken := "test-session-tree-001"
	server.sessions.Store(testToken, adminUser.Username)

	arc := &model.LogArchive{
		ID:          "test_arc_tree",
		Filename:    "test.tar.gz",
		ExtractPath: extractDir,
		Status:      "ready",
		UploadTime:  time.Now(),
		Username:    "admin",
	}
	_ = st.SaveArchive(arc)

	// 1. 请求根目录子节点
	req := httptest.NewRequest(http.MethodGet, "/api/archives/test_arc_tree/tree-nodes", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	server.handleArchiveItem(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("根目录获取失败: code=%d, body=%s", w.Code, w.Body.String())
	}

	var rootNodes []*model.TreeNodeItem
	if err := json.NewDecoder(w.Body).Decode(&rootNodes); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	if len(rootNodes) != 2 {
		t.Fatalf("预期 2 个根节点 (var 和 root.txt)，实际: %d", len(rootNodes))
	}
	// 验证排序：var (目录) 在前，root.txt (文件) 在后
	if !rootNodes[0].IsDirectory || rootNodes[0].Name != "var" {
		t.Errorf("第 1 个节点应为目录 var: %+v", rootNodes[0])
	}
	if rootNodes[1].IsDirectory || rootNodes[1].Name != "root.txt" {
		t.Errorf("第 2 个节点应为文件 root.txt: %+v", rootNodes[1])
	}

	// 2. 请求子目录 var 的子节点
	reqVar := httptest.NewRequest(http.MethodGet, "/api/archives/test_arc_tree/tree-nodes?dir=var", nil)
	reqVar.Header.Set("Authorization", "Bearer "+testToken)
	wVar := httptest.NewRecorder()
	server.handleArchiveItem(wVar, reqVar)

	if wVar.Code != http.StatusOK {
		t.Fatalf("var 目录获取失败: code=%d", wVar.Code)
	}

	var varNodes []*model.TreeNodeItem
	_ = json.NewDecoder(wVar.Body).Decode(&varNodes)
	if len(varNodes) != 1 || varNodes[0].Name != "log" {
		t.Fatalf("var 下预期 1 个 log 目录，实际: %+v", varNodes)
	}

	t.Logf("✔ tree-nodes 懒加载接口测试通过")
}
