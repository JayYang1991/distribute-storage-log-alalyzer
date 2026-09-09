package manager

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
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
