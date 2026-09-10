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

func setupTestNodeUniqueServer(t *testing.T) (*Server, *store.Store, string, func()) {
	tmpDir, err := os.MkdirTemp("", "dist-log-node-unique-*")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.DataDir = tmpDir
	cfg.ClusterToken = "test-cluster-token"
	cfg.InitialAdmin.Username = "admin"
	cfg.InitialAdmin.Password = "admin123"

	st, err := store.NewStore(cfg)
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("初始化存储失败: %v", err)
	}

	srv := NewServer(cfg, st, nil)

	// 登录获取管理员 Token
	loginBody, _ := json.Marshal(map[string]string{
		"username": "admin",
		"password": "admin123",
	})
	loginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(loginBody))
	loginRec := httptest.NewRecorder()
	srv.handleLogin(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		st.Close()
		os.RemoveAll(tmpDir)
		t.Fatalf("登录获取管理员 token 失败: %s", loginRec.Body.String())
	}
	var loginResp struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(loginRec.Body.Bytes(), &loginResp)

	cleanup := func() {
		st.Close()
		os.RemoveAll(tmpDir)
	}

	return srv, st, loginResp.Token, cleanup
}

// 1. 验证通过一键部署添加业务节点时，若 IP 和端口冲突则严格禁止重复添加
func TestNodeUniqueByIPAndPort_DeployRejectDuplicate(t *testing.T) {
	srv, st, token, cleanup := setupTestNodeUniqueServer(t)
	defer cleanup()

	// 预先存入一个正常在线的业务节点
	existingNode := &model.Node{
		ID:            "worker_existing_8081",
		Name:          "worker-node-1",
		IP:            "192.168.122.100",
		Port:          8081,
		Role:          "worker",
		Status:        "online",
		DiskDevice:    "/dev/vdb",
		MountPoint:    "/data/dist-log-storage/disk-vdb",
		JoinedAt:      time.Now(),
		LastHeartbeat: time.Now(),
	}
	if err := st.SaveNode(existingNode); err != nil {
		t.Fatalf("预设节点失败: %v", err)
	}

	// 模拟管理员发起一键部署，指定相同的 IP 和相同的服务端口 8081
	deployPayload := SSHDeployOptions{
		Host:        "192.168.122.100",
		Port:        22,
		WorkerPort:  8081,
		Username:    "jason",
		DiskDevices: []string{"/dev/vdc"},
	}
	body, _ := json.Marshal(deployPayload)
	req := httptest.NewRequest(http.MethodPost, "/api/nodes/deploy", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()

	srv.handleDeployWorker(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("重复添加相同 IP:Port 的节点期望返回 400 Bad Request, 实际返回: %d (响应: %s)", w.Code, w.Body.String())
	}

	if !strings.Contains(w.Body.String(), "禁止重复添加") || !strings.Contains(w.Body.String(), "192.168.122.100:8081") {
		t.Fatalf("错误提示未包含禁止重复添加或冲突地址: %s", w.Body.String())
	}
	t.Logf("✔ 重复部署拦截成功: %s", w.Body.String())
}

// 2. 验证多盘部署时，后续递增端口若与已有节点冲突，同样严格拦截
func TestNodeUniqueByIPAndPort_DeployMultiDiskRejectDuplicatePort(t *testing.T) {
	srv, st, token, cleanup := setupTestNodeUniqueServer(t)
	defer cleanup()

	// 预先存入 8082 端口的业务节点
	existingNode := &model.Node{
		ID:            "worker_existing_8082",
		Name:          "worker-node-2",
		IP:            "192.168.122.100",
		Port:          8082,
		Role:          "worker",
		Status:        "online",
		DiskDevice:    "/dev/vdc",
		MountPoint:    "/data/dist-log-storage/disk-vdc",
		JoinedAt:      time.Now(),
		LastHeartbeat: time.Now(),
	}
	if err := st.SaveNode(existingNode); err != nil {
		t.Fatalf("预设节点失败: %v", err)
	}

	// 尝试向该主机多盘部署 2 块盘，起始端口 8081，第二块盘将占用 8082 (与已有节点冲突)
	deployPayload := SSHDeployOptions{
		Host:        "192.168.122.100",
		Port:        22,
		WorkerPort:  8081,
		Username:    "jason",
		DiskDevices: []string{"/dev/vdb", "/dev/vdc"},
	}
	body, _ := json.Marshal(deployPayload)
	req := httptest.NewRequest(http.MethodPost, "/api/nodes/deploy", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()

	srv.handleDeployWorker(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("多盘部署端口重叠期望返回 400, 实际返回: %d (响应: %s)", w.Code, w.Body.String())
	}

	if !strings.Contains(w.Body.String(), "192.168.122.100:8082") || !strings.Contains(w.Body.String(), "禁止重复添加") {
		t.Fatalf("错误响应未正确定位冲突端口 8082: %s", w.Body.String())
	}
	t.Logf("✔ 多盘部署端口冲突拦截成功: %s", w.Body.String())
}

// 3. 验证通过 POST /api/nodes 手动添加节点时以 IP 和端口为唯一标识
func TestNodeUniqueByIPAndPort_ManualAddWorker(t *testing.T) {
	srv, _, token, cleanup := setupTestNodeUniqueServer(t)
	defer cleanup()

	// 第一次添加 192.168.10.50:8081
	req1Body := `{"name":"test-worker-1","ip":"192.168.10.50","port":8081,"mount_point":"/data"}`
	req1 := httptest.NewRequest(http.MethodPost, "/api/nodes", strings.NewReader(req1Body))
	req1.Header.Set("Authorization", "Bearer "+token)
	w1 := httptest.NewRecorder()
	srv.handleNodes(w1, req1)

	if w1.Code != http.StatusOK {
		t.Fatalf("初次添加节点期望 200, 实际: %d, body: %s", w1.Code, w1.Body.String())
	}

	// 再次添加相同 IP 和相同端口 192.168.10.50:8081 (即使名称不同)
	req2Body := `{"name":"different-name","ip":"192.168.10.50","port":8081,"mount_point":"/data2"}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/nodes", strings.NewReader(req2Body))
	req2.Header.Set("Authorization", "Bearer "+token)
	w2 := httptest.NewRecorder()
	srv.handleNodes(w2, req2)

	if w2.Code != http.StatusBadRequest {
		t.Fatalf("重复添加相同 IP:Port 期望 400, 实际: %d, body: %s", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), "禁止重复添加") || !strings.Contains(w2.Body.String(), "192.168.10.50:8081") {
		t.Fatalf("错误提示不正确: %s", w2.Body.String())
	}
	t.Logf("✔ 手动添加重复 IP:Port 拦截成功: %s", w2.Body.String())

	// 添加不同端口 192.168.10.50:8082 期望成功
	req3Body := `{"name":"test-worker-2","ip":"192.168.10.50","port":8082,"mount_point":"/data3"}`
	req3 := httptest.NewRequest(http.MethodPost, "/api/nodes", strings.NewReader(req3Body))
	req3.Header.Set("Authorization", "Bearer "+token)
	w3 := httptest.NewRecorder()
	srv.handleNodes(w3, req3)

	if w3.Code != http.StatusOK {
		t.Fatalf("不同端口添加期望 200, 实际: %d, body: %s", w3.Code, w3.Body.String())
	}

	// 添加不同 IP 192.168.10.51:8081 期望成功
	req4Body := `{"name":"test-worker-3","ip":"192.168.10.51","port":8081,"mount_point":"/data4"}`
	req4 := httptest.NewRequest(http.MethodPost, "/api/nodes", strings.NewReader(req4Body))
	req4.Header.Set("Authorization", "Bearer "+token)
	w4 := httptest.NewRecorder()
	srv.handleNodes(w4, req4)

	if w4.Code != http.StatusOK {
		t.Fatalf("不同 IP 添加期望 200, 实际: %d, body: %s", w4.Code, w4.Body.String())
	}
	t.Logf("✔ 不同 IP 或端口正常添加通过！")
}

// 4. 验证心跳上报时以 IP 和端口为唯一标识维护节点，杜绝生成多余同地址节点
func TestNodeUniqueByIPAndPort_HeartbeatUnification(t *testing.T) {
	srv, st, _, cleanup := setupTestNodeUniqueServer(t)
	defer cleanup()

	// 预先注册一个节点，其 ID 为 worker_initial_id
	initNode := &model.Node{
		ID:         "worker_initial_id",
		Name:       "node-alpha",
		IP:         "10.0.0.88",
		Port:       9001,
		Role:       "worker",
		Status:     "online",
		DiskDevice: "/dev/sdb",
		JoinedAt:   time.Now(),
	}
	_ = st.SaveNode(initNode)

	// 模拟 Worker 上报心跳，上报中包含不同的临时 Node ID，但 IP 和 Port 保持 10.0.0.88:9001
	hbNode := model.Node{
		ID:       "worker_random_reported_id_12345",
		IP:       "10.0.0.88",
		Port:     9001,
		Role:     "worker",
		Resource: model.SystemResource{CPUPercent: 12.5},
	}
	hbBody, _ := json.Marshal(hbNode)
	hbReq := httptest.NewRequest(http.MethodPost, "/api/cluster/heartbeat", bytes.NewReader(hbBody))
	hbReq.Header.Set("X-Cluster-Token", "test-cluster-token")
	wHb := httptest.NewRecorder()

	srv.handleClusterHeartbeat(wHb, hbReq)

	if wHb.Code != http.StatusOK {
		t.Fatalf("心跳期望 200, 实际: %d", wHb.Code)
	}

	// 检查存储中的所有节点，验证总节点数仍然只有 1 个，未发生重复创建
	nodes, err := st.ListNodes()
	if err != nil {
		t.Fatalf("ListNodes failed: %v", err)
	}
	var workerCount int
	for _, n := range nodes {
		if n.Role == "worker" {
			workerCount++
			if n.IP != "10.0.0.88" || n.Port != 9001 {
				t.Fatalf("意外节点地址: %s:%d", n.IP, n.Port)
			}
		}
	}
	if workerCount != 1 {
		t.Fatalf("心跳应以 IP+Port 唯一复用节点，期望 1 个 worker, 实际: %d", workerCount)
	}
	t.Logf("✔ 心跳统一以 IP+Port 标识节点测试通过！")
}
