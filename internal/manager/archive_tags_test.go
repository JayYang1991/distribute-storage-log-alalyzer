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

func TestParseTags(t *testing.T) {
	// 测试逗号分隔
	res1 := parseTags("Ceph, OSD故障, 生产环境")
	if len(res1) != 3 || res1[0] != "Ceph" || res1[1] != "OSD故障" || res1[2] != "生产环境" {
		t.Fatalf("parseTags comma failed: %+v", res1)
	}

	// 测试中文逗号与分号
	res2 := parseTags("HDFS，NameNode；宕机; 重启")
	if len(res2) != 4 || res2[1] != "NameNode" || res2[2] != "宕机" {
		t.Fatalf("parseTags chinese delimiter failed: %+v", res2)
	}

	// 测试 JSON 数组格式
	res3 := parseTags(`["Ceph", "Mon", "网络超时"]`)
	if len(res3) != 3 || res3[2] != "网络超时" {
		t.Fatalf("parseTags json failed: %+v", res3)
	}

	// 测试空值
	res4 := parseTags("   ")
	if len(res4) != 0 {
		t.Fatalf("parseTags empty failed: %+v", res4)
	}
}

func TestArchiveTagsAndRemarkAPI(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataDir = tmpDir
	st, err := store.NewStore(cfg)
	if err != nil {
		t.Fatalf("New store failed: %v", err)
	}
	defer st.Close()

	srv := NewServer(cfg, st, nil)

	adminUser, err := st.GetUserByUsername("admin")
	if err != nil {
		t.Fatalf("Get admin failed: %v", err)
	}
	testToken := "test-token-archive-tags"
	srv.sessions.Store(testToken, adminUser.Username)

	// 创建带标签与备注的测试归档包
	arc1 := &model.LogArchive{
		ID:         "arc_tag_01",
		Filename:   "ceph_cluster_warning.tar.gz",
		Username:   adminUser.Username,
		UserID:     adminUser.ID,
		Format:     "tar.gz",
		Status:     "ready",
		Tags:       []string{"Ceph", "警告", "生产集群"},
		Remark:     "核心生产集群 OSD 降级告警排查日志",
		UploadTime: time.Now(),
	}
	arc2 := &model.LogArchive{
		ID:         "arc_tag_02",
		Filename:   "hdfs_datanode_error.zip",
		Username:   adminUser.Username,
		UserID:     adminUser.ID,
		Format:     "zip",
		Status:     "ready",
		Tags:       []string{"HDFS", "DataNode", "测试环境"},
		Remark:     "测试环境块损坏日志",
		UploadTime: time.Now(),
	}
	_ = st.SaveArchive(arc1)
	_ = st.SaveArchive(arc2)

	// 1. 测试按 tag 过滤：?tag=Ceph
	reqTag := httptest.NewRequest(http.MethodGet, "/api/archives?tag=Ceph", nil)
	reqTag.Header.Set("Authorization", "Bearer "+testToken)
	wTag := httptest.NewRecorder()
	srv.handleArchives(wTag, reqTag)

	if wTag.Code != http.StatusOK {
		t.Fatalf("handleArchives with tag failed: %d", wTag.Code)
	}
	var tagList []*model.LogArchive
	_ = json.Unmarshal(wTag.Body.Bytes(), &tagList)
	if len(tagList) != 1 || tagList[0].ID != "arc_tag_01" {
		t.Fatalf("Expected 1 archive for tag Ceph, got %d", len(tagList))
	}

	// 2. 测试按综合关键字过滤：?keyword=损坏 (命中 remark)
	reqKw := httptest.NewRequest(http.MethodGet, "/api/archives?keyword=损坏", nil)
	reqKw.Header.Set("Authorization", "Bearer "+testToken)
	wKw := httptest.NewRecorder()
	srv.handleArchives(wKw, reqKw)

	if wKw.Code != http.StatusOK {
		t.Fatalf("handleArchives with keyword failed: %d", wKw.Code)
	}
	var kwList []*model.LogArchive
	_ = json.Unmarshal(wKw.Body.Bytes(), &kwList)
	if len(kwList) != 1 || kwList[0].ID != "arc_tag_02" {
		t.Fatalf("Expected 1 archive for keyword '损坏', got %d", len(kwList))
	}

	// 3. 测试更新标签和备注：PUT /api/archives/arc_tag_01
	updatePayload, _ := json.Marshal(map[string]interface{}{
		"tags":   []string{"Ceph", "已解决", "归档"},
		"remark": "已于 2026-09-09 完成处理，恢复三副本正常",
	})
	reqPut := httptest.NewRequest(http.MethodPut, "/api/archives/arc_tag_01", bytes.NewReader(updatePayload))
	reqPut.Header.Set("Authorization", "Bearer "+testToken)
	reqPut.Header.Set("Content-Type", "application/json")
	wPut := httptest.NewRecorder()
	srv.handleArchiveItem(wPut, reqPut)

	if wPut.Code != http.StatusOK {
		t.Fatalf("PUT /api/archives/arc_tag_01 failed: %d", wPut.Code)
	}
	var updatedArc model.LogArchive
	_ = json.Unmarshal(wPut.Body.Bytes(), &updatedArc)
	if len(updatedArc.Tags) != 3 || updatedArc.Tags[1] != "已解决" || updatedArc.Remark != "已于 2026-09-09 完成处理，恢复三副本正常" {
		t.Fatalf("Updated archive mismatch: %+v", updatedArc)
	}

	// 重新从 store 读取验证持久化
	persisted, err := st.GetArchive("arc_tag_01")
	if err != nil || persisted.Remark != "已于 2026-09-09 完成处理，恢复三副本正常" {
		t.Fatalf("Persisted archive mismatch: %+v", persisted)
	}
	t.Logf("✔ 日志包标签与备注设置、查询与更新全量测试通过！")
}
