package manager

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"dist-log-analyzer/internal/config"
	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/store"
)

func TestNodeRemovalStrategies(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "node_remove_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	cfg := config.DefaultConfig()
	cfg.DataDir = tempDir
	cfg.InitialAdmin.Username = "admin"
	cfg.InitialAdmin.Password = "admin123"

	st, err := store.NewStore(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer st.Close()

	server := NewServer(cfg, st, nil)

	// 1. 获取 Admin Token
	loginBody, _ := json.Marshal(map[string]string{
		"username": "admin",
		"password": "admin123",
	})
	loginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")
	loginRec := httptest.NewRecorder()
	server.handleLogin(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("Login failed: %d", loginRec.Code)
	}
	var loginResp struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(loginRec.Body.Bytes(), &loginResp)
	adminAuth := "Bearer " + loginResp.Token

	// 2. 模拟源业务节点 Worker-1 与目标节点 Worker-2
	node1 := &model.Node{
		ID:       "worker-node-1",
		Name:     "Worker-1",
		Role:     "worker",
		IP:       "127.0.0.1",
		Port:     18081,
		Status:   "online",
		JoinedAt: time.Now(),
		Resource: model.SystemResource{DiskUsedMB: 500, DiskFreeMB: 2000},
	}
	node2 := &model.Node{
		ID:       "worker-node-2",
		Name:     "Worker-2",
		Role:     "worker",
		IP:       "127.0.0.1",
		Port:     18082,
		Status:   "online",
		JoinedAt: time.Now(),
		Resource: model.SystemResource{DiskUsedMB: 100, DiskFreeMB: 4000},
	}
	_ = st.SaveNode(node1)
	_ = st.SaveNode(node2)

	// 3. 在 node1 上创建两个日志包记录
	arc1 := &model.LogArchive{
		ID:              "arc_001",
		UserID:          "usr_admin_001",
		Username:        "admin",
		Filename:        "ceph_cluster.tar.gz",
		Size:            1024 * 1024,
		Format:          "tar.gz",
		Status:          "ready",
		StorageNodeID:   "worker-node-1",
		StorageNodeName: "Worker-1",
		StorageNodeIP:   "127.0.0.1",
		StorageNodePort: 18081,
		ExtractPath:     "/tmp/extracted/arc_001",
		UploadTime:      time.Now(),
	}
	arc2 := &model.LogArchive{
		ID:              "arc_002",
		UserID:          "usr_admin_001",
		Username:        "admin",
		Filename:        "hdfs_namenode.zip",
		Size:            2048 * 1024,
		Format:          "zip",
		Status:          "ready",
		StorageNodeID:   "worker-node-1",
		StorageNodeName: "Worker-1",
		StorageNodeIP:   "127.0.0.1",
		StorageNodePort: 18081,
		ExtractPath:     "/tmp/extracted/arc_002",
		UploadTime:      time.Now(),
	}
	_ = st.SaveArchive(arc1)
	_ = st.SaveArchive(arc2)

	// 验证 node1 当前存储日志包数为 2
	node1Arcs, _ := st.ListArchivesByNode("worker-node-1")
	if len(node1Arcs) != 2 {
		t.Fatalf("Expected 2 archives for worker-node-1, got %d", len(node1Arcs))
	}

	// -------------------------------------------------------------
	// 测试策略 1: 迁移数据至业务存储节点 (action: migrate)
	// -------------------------------------------------------------
	// 模拟源 Worker-1 提供归档文件下载响应
	dummyContent := []byte("fake log archive binary stream for testing migration")
	mockWorker1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/worker/storage/download-archive" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(dummyContent)
			return
		}
		if r.URL.Path == "/api/worker/storage/clean-archive" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer mockWorker1.Close()

	// 模拟目标 Worker-2 接收迁移上传响应
	mockWorker2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/worker/storage/upload" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"files":        []*model.LogFileItem{},
				"total_lines":  10,
				"extract_path": "/tmp/extracted/worker2_migrated",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer mockWorker2.Close()

	// 更新 node1 和 node2 的端口为 mockWorker 的端口
	mockURL1, _ := http.NewRequest("GET", mockWorker1.URL, nil)
	mockPort1 := mockURL1.URL.Port()
	var portInt1 int
	_, _ = fmt.Sscanf(mockPort1, "%d", &portInt1)
	node1.Port = portInt1
	_ = st.SaveNode(node1)

	mockURL2, _ := http.NewRequest("GET", mockWorker2.URL, nil)
	mockPort2 := mockURL2.URL.Port()
	var portInt2 int
	_, _ = fmt.Sscanf(mockPort2, "%d", &portInt2)
	node2.Port = portInt2
	_ = st.SaveNode(node2)

	// 1.1 尝试迁移至管理节点本地系统盘 -> 必须被拒绝 (HTTP 400)
	rejectReq := RemoveNodeRequest{
		Action:       "migrate",
		TargetNodeID: "manager_primary",
	}
	rejectBody, _ := json.Marshal(rejectReq)
	reqReject := httptest.NewRequest(http.MethodPost, "/api/nodes/worker-node-1/remove", bytes.NewReader(rejectBody))
	reqReject.Header.Set("Authorization", adminAuth)
	reqReject.Header.Set("Content-Type", "application/json")
	wReject := httptest.NewRecorder()
	server.handleNodeItem(wReject, reqReject)
	if wReject.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 Bad Request when migrating to manager_primary, got %d, body: %s", wReject.Code, wReject.Body.String())
	}

	// 1.2 正规迁移至在线业务存储节点 worker-node-2 -> 成功 (HTTP 200)
	removeMigrateReq := RemoveNodeRequest{
		Action:       "migrate",
		TargetNodeID: "worker-node-2",
	}
	body, _ := json.Marshal(removeMigrateReq)
	req := httptest.NewRequest(http.MethodPost, "/api/nodes/worker-node-1/remove", bytes.NewReader(body))
	req.Header.Set("Authorization", adminAuth)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleNodeItem(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK for migrate removal, got %d, body: %s", w.Code, w.Body.String())
	}

	// 验证：node1 已从集群删除
	deletedNode, _ := st.GetNode("worker-node-1")
	if deletedNode != nil {
		t.Errorf("Expected worker-node-1 to be deleted from store")
	}

	// 验证：arc1 和 arc2 的存储节点已更新为 worker-node-2
	updatedArc1, _ := st.GetArchive("arc_001")
	if updatedArc1 == nil || updatedArc1.StorageNodeID != "worker-node-2" {
		t.Errorf("Expected arc_001 migrated to worker-node-2, got %+v", updatedArc1)
	}

	// -------------------------------------------------------------
	// 测试策略 2: 保留数据仅注销节点 (action: retain)
	// -------------------------------------------------------------
	arc3 := &model.LogArchive{
		ID:              "arc_003",
		UserID:          "usr_admin_001",
		Username:        "admin",
		Filename:        "minio_audit.tar.gz",
		Size:            512 * 1024,
		Format:          "tar.gz",
		Status:          "ready",
		StorageNodeID:   "worker-node-2",
		StorageNodeName: "Worker-2",
		StorageNodeIP:   "127.0.0.1",
		StorageNodePort: 18082,
	}
	_ = st.SaveArchive(arc3)

	removeRetainReq := RemoveNodeRequest{
		Action: "retain",
	}
	bodyRetain, _ := json.Marshal(removeRetainReq)
	reqRetain := httptest.NewRequest(http.MethodPost, "/api/nodes/worker-node-2/remove", bytes.NewReader(bodyRetain))
	reqRetain.Header.Set("Authorization", adminAuth)
	reqRetain.Header.Set("Content-Type", "application/json")
	wRetain := httptest.NewRecorder()

	server.handleNodeItem(wRetain, reqRetain)
	if wRetain.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK for retain removal, got %d, body: %s", wRetain.Code, wRetain.Body.String())
	}

	// 验证：worker-node-2 已注销
	delNode2, _ := st.GetNode("worker-node-2")
	if delNode2 != nil {
		t.Errorf("Expected worker-node-2 to be deleted")
	}
	// 验证：arc3 仍存在于数据库中
	savedArc3, _ := st.GetArchive("arc_003")
	if savedArc3 == nil {
		t.Errorf("Expected arc_003 data to be retained")
	}

	// -------------------------------------------------------------
	// 测试策略 3: 已移除节点发送心跳被坚决拦截 (返回 410 Gone)，严防自动加载复活
	// -------------------------------------------------------------
	hbPayload, _ := json.Marshal(&model.Node{
		ID:       "worker-node-2",
		Name:     "Worker-2",
		Role:     "worker",
		IP:       "127.0.0.1",
		Port:     18082,
		Status:   "online",
		JoinedAt: time.Now(),
	})
	hbReq := httptest.NewRequest(http.MethodPost, "/api/cluster/heartbeat", bytes.NewReader(hbPayload))
	hbReq.Header.Set("X-Cluster-Token", cfg.ClusterToken)
	hbReq.Header.Set("Content-Type", "application/json")
	wHb := httptest.NewRecorder()

	server.handleClusterHeartbeat(wHb, hbReq)
	if wHb.Code != http.StatusGone {
		t.Fatalf("Expected 410 Gone for decommissioned node heartbeat, got %d, body: %s", wHb.Code, wHb.Body.String())
	}

	// 再次确认 worker-node-2 没有重新进入数据库
	revivedNode, _ := st.GetNode("worker-node-2")
	if revivedNode != nil {
		t.Fatalf("Decommissioned node must NOT be revived by heartbeat!")
	}

	t.Log("✔ 业务节点移除与数据迁移/保留策略、防自动复活拦截测试全量验证通过！")
}
