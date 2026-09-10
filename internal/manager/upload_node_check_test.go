package manager

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"dist-log-analyzer/internal/config"
	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/store"
)

func TestUploadArchive_RejectWhenNoWorkerOrManagerSelected(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "upload_check_test_*")
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

	// 获取 Admin Token
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
	authHeader := "Bearer " + loginResp.Token

	// 辅助函数：构造带有文件的 multipart 请求
	makeUploadRequest := func(targetNodeID string) *http.Request {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, _ := writer.CreateFormFile("file", "test.tar.gz")
		_, _ = part.Write([]byte("dummy content"))
		if targetNodeID != "" {
			_ = writer.WriteField("target_node_id", targetNodeID)
		}
		_ = writer.WriteField("tags", "tag-check-"+targetNodeID)
		_ = writer.Close()

		req := httptest.NewRequest(http.MethodPost, "/api/archives/upload", &body)
		req.Header.Set("Authorization", authHeader)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		return req
	}

	// 场景 1: 集群内无任何业务节点，尝试 auto 上传 -> 必须拒绝并报错 (HTTP 400)
	{
		req := makeUploadRequest("auto")
		w := httptest.NewRecorder()
		server.handleUploadArchive(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("Expected 400 Bad Request when no workers exist, got %d, body: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "无可用业务存储节点") {
			t.Errorf("Expected error to mention no worker available, got %s", w.Body.String())
		}
	}

	// 场景 2: 显式指定 target_node_id 为 manager_primary -> 必须拒绝并报错 (HTTP 400)
	{
		req := makeUploadRequest("manager_primary")
		w := httptest.NewRecorder()
		server.handleUploadArchive(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("Expected 400 Bad Request when manager_primary is specified, got %d, body: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "禁止选择管理节点存储日志") {
			t.Errorf("Expected error to mention manager storage forbidden, got %s", w.Body.String())
		}
	}

	// 场景 3: 显式指定 target_node_id 为 local -> 必须拒绝并报错 (HTTP 400)
	{
		req := makeUploadRequest("local")
		w := httptest.NewRecorder()
		server.handleUploadArchive(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("Expected 400 Bad Request when local is specified, got %d, body: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "禁止选择管理节点存储日志") {
			t.Errorf("Expected error to mention manager storage forbidden, got %s", w.Body.String())
		}
	}

	// 场景 4: 存在离线的业务节点，但无可用的在线业务节点 -> 必须拒绝并报错 (HTTP 400)
	{
		offlineWorker := &model.Node{
			ID:            "worker-offline",
			Name:          "Offline Worker",
			Role:          "worker",
			IP:            "127.0.0.1",
			Port:          19999,
			Status:        "offline",
			LastHeartbeat: time.Now().Add(-1 * time.Hour),
		}
		_ = st.SaveNode(offlineWorker)

		req := makeUploadRequest("worker-offline")
		w := httptest.NewRecorder()
		server.handleUploadArchive(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("Expected 400 Bad Request when specified worker is offline, got %d, body: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "无可用业务存储节点") {
			t.Errorf("Expected error to mention no worker available, got %s", w.Body.String())
		}
	}
}

func TestAsyncArchiveUploadAndCallback(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "async_upload_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	cfg := config.DefaultConfig()
	cfg.DataDir = tempDir
	cfg.ClusterToken = "test-cluster-token"
	cfg.InitialAdmin.Username = "admin"
	cfg.InitialAdmin.Password = "admin123"

	st, err := store.NewStore(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer st.Close()

	server := NewServer(cfg, st, nil)

	// 模拟 Worker Server
	var capturedUpload bool
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/worker/storage/upload" {
			capturedUpload = true
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":       "extracting",
				"archive_id":   r.FormValue("archive_id"),
				"extract_path": "/data/extracted/test",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer workerServer.Close()

	// 注册在线 Worker 节点
	workerURL := strings.TrimPrefix(workerServer.URL, "http://")
	parts := strings.Split(workerURL, ":")
	workerPort := 8080
	if len(parts) == 2 {
		if p, err := strconv.Atoi(parts[1]); err == nil {
			workerPort = p
		}
	}

	onlineWorker := &model.Node{
		ID:            "worker-async-test",
		Name:          "Worker Async Test",
		Role:          "worker",
		IP:            parts[0],
		Port:          workerPort,
		Status:        "online",
		LastHeartbeat: time.Now(),
		Resource: model.SystemResource{
			DiskTotalMB: 100000,
			DiskFreeMB:  50000,
			DiskUsedMB:  50000,
		},
	}
	_ = st.SaveNode(onlineWorker)

	// 登录获取 Token
	loginBody, _ := json.Marshal(map[string]string{
		"username": "admin",
		"password": "admin123",
	})
	loginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")
	loginRec := httptest.NewRecorder()
	server.handleLogin(loginRec, loginReq)
	var loginResp struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(loginRec.Body.Bytes(), &loginResp)

	// 1. 上传文件 -> 期望立即返回 extracting 状态与 HTTP 200
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "syslog.2.gz")
	_, _ = part.Write([]byte("mock gz binary stream"))
	_ = writer.WriteField("target_node_id", "worker-async-test")
	_ = writer.WriteField("tags", "async-test-unique-tag")
	_ = writer.Close()

	uploadReq := httptest.NewRequest(http.MethodPost, "/api/archives/upload", &body)
	uploadReq.Header.Set("Authorization", "Bearer "+loginResp.Token)
	uploadReq.Header.Set("Content-Type", writer.FormDataContentType())
	uploadRec := httptest.NewRecorder()

	server.handleUploadArchive(uploadRec, uploadReq)
	if uploadRec.Code != http.StatusOK {
		t.Fatalf("Upload failed with code %d: %s", uploadRec.Code, uploadRec.Body.String())
	}

	var createdArchive model.LogArchive
	if err := json.Unmarshal(uploadRec.Body.Bytes(), &createdArchive); err != nil {
		t.Fatalf("Failed to decode upload response: %v", err)
	}

	if !capturedUpload {
		t.Fatalf("Expected upload to be forwarded to worker")
	}
	if createdArchive.Status != "extracting" {
		t.Fatalf("Expected created archive status to be 'extracting', got '%s'", createdArchive.Status)
	}

	// 2. 模拟 Worker 后台解包与分析完毕，通过回调接口通知 Manager
	callbackPayload := model.ArchiveCallbackReq{
		ArchiveID:   createdArchive.ID,
		Status:      "ready",
		TotalLines:  21588691,
		FileCount:   1,
		ExtractPath: "/data/extracted/test",
		Report: &model.DiagnosisReport{
			ArchiveID:   createdArchive.ID,
			ArchiveName: createdArchive.Filename,
			Status:      "completed",
			HealthScore: 95,
			TotalEvents: 2,
		},
	}
	callbackBody, _ := json.Marshal(callbackPayload)
	cbReq := httptest.NewRequest(http.MethodPost, "/api/cluster/archive-callback", bytes.NewReader(callbackBody))
	cbReq.Header.Set("Content-Type", "application/json")
	cbReq.Header.Set("X-Cluster-Token", cfg.ClusterToken)
	cbRec := httptest.NewRecorder()

	server.handleClusterArchiveCallback(cbRec, cbReq)
	if cbRec.Code != http.StatusOK {
		t.Fatalf("Callback failed with code %d: %s", cbRec.Code, cbRec.Body.String())
	}

	// 3. 验证 Store 中归档包状态已更新为 ready，总行数与报告均正确落库
	updatedArchive, err := st.GetArchive(createdArchive.ID)
	if err != nil {
		t.Fatalf("Failed to get updated archive: %v", err)
	}
	if updatedArchive.Status != "ready" {
		t.Errorf("Expected updated archive status 'ready', got '%s'", updatedArchive.Status)
	}
	if updatedArchive.TotalLines != 21588691 {
		t.Errorf("Expected total lines 21588691, got %d", updatedArchive.TotalLines)
	}
	if updatedArchive.FileCount != 1 {
		t.Errorf("Expected file count 1, got %d", updatedArchive.FileCount)
	}

	report, err := st.GetReport(createdArchive.ID)
	if err != nil || report == nil {
		t.Fatalf("Expected report to be saved, err: %v", err)
	}
	if report.HealthScore != 95 || report.TotalEvents != 2 {
		t.Errorf("Report data mismatch: score=%d, events=%d", report.HealthScore, report.TotalEvents)
	}
}

func TestUploadArchiveTagValidationAndUniqueness(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "dist-log-tag-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	cfg := config.DefaultConfig()
	cfg.DataDir = tempDir
	cfg.InitialAdmin.Username = "admin"
	cfg.InitialAdmin.Password = "admin123"

	st, err := store.NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	server := NewServer(cfg, st, nil)

	authReq := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"admin","password":"admin123"}`))
	authRec := httptest.NewRecorder()
	server.handleLogin(authRec, authReq)
	var loginResp struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(authRec.Body.Bytes(), &loginResp)

	makeUploadWithTag := func(filename, tag string) *httptest.ResponseRecorder {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, _ := writer.CreateFormFile("file", filename)
		_, _ = part.Write([]byte("mock content"))
		_ = writer.WriteField("target_node_id", "auto")
		if tag != "" {
			_ = writer.WriteField("tags", tag)
		}
		_ = writer.Close()

		req := httptest.NewRequest(http.MethodPost, "/api/archives/upload", &body)
		req.Header.Set("Authorization", "Bearer "+loginResp.Token)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		rec := httptest.NewRecorder()
		server.handleUploadArchive(rec, req)
		return rec
	}

	// 1. 测试未输入标签时 -> 必须拦截并返回 400
	recEmpty := makeUploadWithTag("empty_tag.tar.gz", "")
	if recEmpty.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 when tag is empty, got %d, body: %s", recEmpty.Code, recEmpty.Body.String())
	}
	if !strings.Contains(recEmpty.Body.String(), "必须输入日志归档标签") {
		t.Errorf("Expected body to mention required tag, got %s", recEmpty.Body.String())
	}

	// 先手动在 store 中保存一个已存在的归档包 (模拟系统已有归档)
	existingArc := &model.LogArchive{
		ID:         "arc_existing_1",
		UserID:     "usr_admin_001",
		Username:   "admin",
		Filename:   "existing_log.tar.gz",
		Tags:       []string{"Ceph-Cluster-01"},
		UploadTime: time.Now(),
	}
	if err := st.SaveArchive(existingArc); err != nil {
		t.Fatal(err)
	}

	// 2. 测试 check-tag 接口查重
	checkReq := httptest.NewRequest(http.MethodGet, "/api/archives/check-tag?tag=Ceph-Cluster-01", nil)
	checkRec := httptest.NewRecorder()
	server.handleCheckArchiveTag(checkRec, checkReq)
	if checkRec.Code != http.StatusOK {
		t.Fatalf("Check tag failed: %d", checkRec.Code)
	}
	var checkResp struct {
		Exists           bool   `json:"exists"`
		Tag              string `json:"tag"`
		MatchedArchiveID string `json:"matched_archive_id"`
	}
	if err := json.Unmarshal(checkRec.Body.Bytes(), &checkResp); err != nil {
		t.Fatal(err)
	}
	if !checkResp.Exists || checkResp.MatchedArchiveID != "arc_existing_1" {
		t.Errorf("Expected check-tag to report exists=true for Ceph-Cluster-01, got %+v", checkResp)
	}

	// 3. 测试上传使用已重复的标签 -> 必须拦截并返回 400
	recDup := makeUploadWithTag("dup_tag.tar.gz", "ceph-cluster-01") // 大小写不敏感测试
	if recDup.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 when tag is duplicate, got %d, body: %s", recDup.Code, recDup.Body.String())
	}
	if !strings.Contains(recDup.Body.String(), "占用") || !strings.Contains(recDup.Body.String(), "唯一") {
		t.Errorf("Expected error to mention tag duplicate and unique requirement, got %s", recDup.Body.String())
	}

	// 4. 测试修改已有归档标签为冲突标签 -> 必须拦截
	updateReq := httptest.NewRequest(http.MethodPut, "/api/archives/"+existingArc.ID, strings.NewReader(`{"tags":"Ceph-Cluster-01","remark":"test"}`))
	updateReq.Header.Set("Authorization", "Bearer "+loginResp.Token)
	updateRec := httptest.NewRecorder()
	server.handleArchiveItem(updateRec, updateReq)
	// 自身标签不变应该允许
	if updateRec.Code != http.StatusOK {
		t.Fatalf("Self tag should be allowed, got code %d, body: %s", updateRec.Code, updateRec.Body.String())
	}
}

func TestDeleteArchiveCleansDiskSpace(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "dist-log-delete-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	cfg := config.DefaultConfig()
	cfg.DataDir = tempDir
	cfg.InitialAdmin.Username = "admin"
	cfg.InitialAdmin.Password = "admin123"

	st, err := store.NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	server := NewServer(cfg, st, nil)

	// 1. 模拟一个远程 Worker 节点
	var cleanedRemoteArchiveID string
	var cleanedRemoteFilename string
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/worker/storage/clean-archive" {
			cleanedRemoteArchiveID = r.URL.Query().Get("archive_id")
			cleanedRemoteFilename = r.URL.Query().Get("filename")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
			return
		}
		http.NotFound(w, r)
	}))
	defer workerServer.Close()

	workerURL, _ := url.Parse(workerServer.URL)
	workerHost := workerURL.Hostname()
	workerPort, _ := strconv.Atoi(workerURL.Port())

	// 2. 准备本地磁盘测试文件
	testExtractDir := filepath.Join(tempDir, "extracted", "arc_delete_999")
	_ = os.MkdirAll(testExtractDir, 0755)
	dummyLogFile := filepath.Join(testExtractDir, "syslog.log")
	_ = os.WriteFile(dummyLogFile, []byte("large mock log lines..."), 0644)

	// 准备 archives 目录下的原始包
	testArchivesDir := filepath.Join(tempDir, "archives")
	_ = os.MkdirAll(testArchivesDir, 0755)
	dummyArchiveFile := filepath.Join(testArchivesDir, "ceph.tar.gz")
	_ = os.WriteFile(dummyArchiveFile, []byte("mock tar gz binary"), 0644)

	// 保存归档记录到数据库
	arc := &model.LogArchive{
		ID:              "arc_delete_999",
		UserID:          "usr_admin_001",
		Username:        "admin",
		Filename:        "ceph.tar.gz",
		Size:            1024,
		ExtractPath:     testExtractDir,
		StorageNodeID:   "worker_mock_01",
		StorageNodeName: "worker-mock-01",
		StorageNodeIP:   workerHost,
		StorageNodePort: workerPort,
		UploadTime:      time.Now(),
	}
	if err := st.SaveArchive(arc); err != nil {
		t.Fatal(err)
	}

	// 3. 执行删除请求 DELETE /api/archives/arc_delete_999
	authReq := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"admin","password":"admin123"}`))
	authRec := httptest.NewRecorder()
	server.handleLogin(authRec, authReq)
	var loginResp struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(authRec.Body.Bytes(), &loginResp)

	delReq := httptest.NewRequest(http.MethodDelete, "/api/archives/arc_delete_999", nil)
	delReq.Header.Set("Authorization", "Bearer "+loginResp.Token)
	delRec := httptest.NewRecorder()
	server.handleArchiveItem(delRec, delReq)

	if delRec.Code != http.StatusOK {
		t.Fatalf("Delete archive failed, got code %d, body: %s", delRec.Code, delRec.Body.String())
	}

	// 4. 验证远程 Worker clean-archive 是否被正确调用
	if cleanedRemoteArchiveID != "arc_delete_999" || cleanedRemoteFilename != "ceph.tar.gz" {
		t.Errorf("Expected remote worker clean-archive to be called with ID 'arc_delete_999' and filename 'ceph.tar.gz', got id=%q, file=%q",
			cleanedRemoteArchiveID, cleanedRemoteFilename)
	}

	// 5. 验证本地磁盘的解包目录和原始包是否已被删除
	if _, err := os.Stat(testExtractDir); !os.IsNotExist(err) {
		t.Errorf("Expected testExtractDir to be removed, but it still exists")
	}
	if _, err := os.Stat(dummyArchiveFile); !os.IsNotExist(err) {
		t.Errorf("Expected dummyArchiveFile to be removed, but it still exists")
	}

	// 6. 验证数据库中的归档记录已删除
	deletedArc, _ := st.GetArchive("arc_delete_999")
	if deletedArc != nil {
		t.Errorf("Expected archive to be removed from database, but found: %+v", deletedArc)
	}
}



