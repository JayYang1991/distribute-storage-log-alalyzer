package manager

import (
	"fmt"
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

func TestDownloadSpecifiedLogFile_LocalAndSecurity(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "download_test_*")
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

	// 创建测试管理员用户与会话 Token
	adminUser, err := st.GetUserByUsername("admin")
	if err != nil {
		t.Fatalf("获取 admin 用户失败: %v", err)
	}
	testToken := "test-session-token-download-001"
	srv.sessions.Store(testToken, adminUser.Username)

	// 构造测试解压目录与日志文件
	extractDir := filepath.Join(tmpDir, "extracted", "arc-test-01")
	logSubDir := filepath.Join(extractDir, "ceph", "cluster")
	if err := os.MkdirAll(logSubDir, 0755); err != nil {
		t.Fatalf("创建解包目录失败: %v", err)
	}

	testLogContent := "2026-09-09 10:20:00 [INF] OSD.0 is UP and active+clean\n2026-09-09 10:20:01 [WRN] slow request blocked 32 ops\n"
	testLogPath := filepath.Join(logSubDir, "ceph-osd.0.log")
	if err := os.WriteFile(testLogPath, []byte(testLogContent), 0644); err != nil {
		t.Fatalf("写入测试日志文件失败: %v", err)
	}

	// 写入元数据归档记录
	archive := &model.LogArchive{
		ID:          "arc-test-01",
		Filename:    "ceph_cluster_logs.tar.gz",
		Username:    adminUser.Username,
		UserID:      adminUser.ID,
		Format:      "tar.gz",
		Status:      "ready",
		ExtractPath: extractDir,
		UploadTime:  time.Now(),
	}
	if err := st.SaveArchive(archive); err != nil {
		t.Fatalf("保存归档包元数据失败: %v", err)
	}

	// 1. 验证通过 Query Token 正常下载指定日志文件
	relPath := "ceph/cluster/ceph-osd.0.log"
	reqURL := fmt.Sprintf("/api/archives/%s/download-file?path=%s&token=%s", archive.ID, url.QueryEscape(relPath), testToken)
	req := httptest.NewRequest(http.MethodGet, reqURL, nil)
	w := httptest.NewRecorder()

	srv.handleArchiveItem(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("下载指定日志文件预期 200 OK，实际状态码: %d, body: %s", w.Code, w.Body.String())
	}

	cd := w.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "ceph-osd.0.log") {
		t.Errorf("Content-Disposition 预期包含文件名 ceph-osd.0.log，实际: %s", cd)
	}
	if w.Body.String() != testLogContent {
		t.Errorf("下载文件内容与预期不一致: \n预期:\n%s\n实际:\n%s", testLogContent, w.Body.String())
	}

	// 2. 验证路径穿越高危请求被安全拦截 (403 Forbidden)
	badReqURL := fmt.Sprintf("/api/archives/%s/download-file?path=%s&token=%s", archive.ID, url.QueryEscape("../../etc/passwd"), testToken)
	badReq := httptest.NewRequest(http.MethodGet, badReqURL, nil)
	wBad := httptest.NewRecorder()

	srv.handleArchiveItem(wBad, badReq)
	if wBad.Code != http.StatusForbidden {
		t.Errorf("路径穿越攻击预期应返回 403 Forbidden，实际状态码: %d", wBad.Code)
	}

	// 3. 验证缺少 path 参数返回 400 Bad Request
	noPathURL := fmt.Sprintf("/api/archives/%s/download-file?token=%s", archive.ID, testToken)
	noPathReq := httptest.NewRequest(http.MethodGet, noPathURL, nil)
	wNoPath := httptest.NewRecorder()

	srv.handleArchiveItem(wNoPath, noPathReq)
	if wNoPath.Code != http.StatusBadRequest {
		t.Errorf("缺少 path 参数预期返回 400，实际状态码: %d", wNoPath.Code)
	}
}

func TestDownloadSpecifiedLogFile_RemoteWorkerProxy(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "download_proxy_test_*")
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
	testToken := "test-session-token-proxy-002"
	srv.sessions.Store(testToken, adminUser.Username)

	// 模拟远程业务节点 Worker 的下载服务
	workerFileContent := "2026-09-09 10:25:00 [Worker Storage Log] Distributed Node File OK\n"
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/worker/storage/download-file" {
			path := r.URL.Query().Get("path")
			fileName := filepath.Base(path)
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, fileName))
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.Itoa(len(workerFileContent)))
			_, _ = w.Write([]byte(workerFileContent))
			return
		}
		http.NotFound(w, r)
	}))
	defer workerServer.Close()

	// 解析 workerServer 的 IP 和端口
	workerURL, _ := url.Parse(workerServer.URL)
	workerHost := workerURL.Hostname()
	workerPort, _ := strconv.Atoi(workerURL.Port())

	// 构造存放在远程业务节点的归档包记录
	archive := &model.LogArchive{
		ID:              "arc-remote-worker-01",
		Filename:        "remote_sys_logs.tar.gz",
		Username:        adminUser.Username,
		UserID:          adminUser.ID,
		Format:          "tar.gz",
		Status:          "ready",
		ExtractPath:     "/data/dist-log-storage/users/admin/extracted/arc-remote-worker-01",
		StorageNodeID:   "worker-ssd-node",
		StorageNodeName: "worker-ssd-node",
		StorageNodeIP:   workerHost,
		StorageNodePort: workerPort,
		UploadTime:      time.Now(),
	}
	_ = st.SaveArchive(archive)

	// 通过 Manager 发起下载请求，Manager 应透明代理至 Worker 并流式返回
	relPath := "sys/messages.log"
	reqURL := fmt.Sprintf("/api/archives/%s/download-file?path=%s&token=%s", archive.ID, url.QueryEscape(relPath), testToken)
	req := httptest.NewRequest(http.MethodGet, reqURL, nil)
	w := httptest.NewRecorder()

	srv.handleArchiveItem(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("远程代理下载预期 200 OK，实际: %d, body: %s", w.Code, w.Body.String())
	}

	cd := w.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "messages.log") {
		t.Errorf("Content-Disposition 预期包含 messages.log，实际: %s", cd)
	}

	if w.Body.String() != workerFileContent {
		t.Errorf("代理下载内容与业务节点返回内容不一致: \n预期:\n%s\n实际:\n%s", workerFileContent, w.Body.String())
	}
}

func TestDownloadArchivePackage(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "download_archive_test_*")
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
	testToken := "test-session-token-archive-003"
	srv.sessions.Store(testToken, adminUser.Username)

	// 创建模拟原始压缩包
	archiveDir := st.GetUserArchiveDir(adminUser.Username)
	_ = os.MkdirAll(archiveDir, 0755)
	rawPackageContent := "MOCK_TAR_GZ_COMPRESSED_DATA_STREAM"
	rawFileName := "sample_storage_audit.tar.gz"
	_ = os.WriteFile(filepath.Join(archiveDir, rawFileName), []byte(rawPackageContent), 0644)

	archive := &model.LogArchive{
		ID:          "arc-sample-03",
		Filename:    rawFileName,
		Username:    adminUser.Username,
		UserID:      adminUser.ID,
		Format:      "tar.gz",
		Status:      "ready",
		UploadTime:  time.Now(),
	}
	_ = st.SaveArchive(archive)

	// 发起整包下载请求
	reqURL := fmt.Sprintf("/api/archives/%s/download?token=%s", archive.ID, testToken)
	req := httptest.NewRequest(http.MethodGet, reqURL, nil)
	w := httptest.NewRecorder()

	srv.handleArchiveItem(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("整包下载预期 200 OK，实际: %d, body: %s", w.Code, w.Body.String())
	}

	cd := w.Header().Get("Content-Disposition")
	if !strings.Contains(cd, rawFileName) {
		t.Errorf("Content-Disposition 预期包含 %s，实际: %s", rawFileName, cd)
	}

	if w.Body.String() != rawPackageContent {
		t.Errorf("下载整包内容与实际文件不一致")
	}
}
