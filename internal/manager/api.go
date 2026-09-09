package manager

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"dist-log-analyzer/internal/config"
	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/rules"
	"dist-log-analyzer/internal/store"
	"dist-log-analyzer/internal/worker"

	"golang.org/x/crypto/bcrypt"
)

type Server struct {
	cfg       *config.Config
	store     *store.Store
	scheduler *Scheduler
	ha        *HAManager
	sessions  sync.Map // token -> username
	server    *http.Server
	staticFS  http.FileSystem
}

func NewServer(cfg *config.Config, s *store.Store, staticFS http.FileSystem) *Server {
	srv := &Server{
		cfg:       cfg,
		store:     s,
		scheduler: NewScheduler(s),
		ha:        NewHAManager(cfg, s),
		staticFS:  staticFS,
	}
	// 初始化内置规则（如果数据库规则为空）
	ruleList, _ := s.ListRules()
	if len(ruleList) == 0 {
		for _, r := range rules.DefaultPresets() {
			_ = s.SaveRule(r)
		}
	}
	return srv
}

func (s *Server) Start(ctx context.Context) error {
	// 启动 HA 状态机与数据同步
	s.ha.Start(ctx)

	// 启动业务计算节点心跳存活与自动故障告警监控协程
	go s.startNodeHealthAndAlarmMonitor(ctx)

	mux := http.NewServeMux()

	// 静态文件与前端
	if s.staticFS != nil {
		mux.Handle("/", http.FileServer(s.staticFS))
	}

	// 高可用 HA 路由
	mux.HandleFunc("/api/ha/status", s.handleHAStatus)
	mux.HandleFunc("/api/ha/config", s.handleHAConfig)
	mux.HandleFunc("/api/ha/heartbeat", s.handleHAHeartbeat)
	mux.HandleFunc("/api/ha/snapshot", s.handleHASnapshot)
	mux.HandleFunc("/api/ha/switchover", s.handleHASwitchover)
	mux.HandleFunc("/api/ha/promote", s.handleHAPromote)
	mux.HandleFunc("/api/ha/demote", s.handleHADemote)

	// 告警管理路由
	mux.HandleFunc("/api/alarms", s.handleAlarms)
	mux.HandleFunc("/api/alarms/report", s.handleAlarmReport)
	mux.HandleFunc("/api/alarms/summary", s.handleAlarmSummary)
	mux.HandleFunc("/api/alarms/clear-resolved", s.handleClearResolvedAlarms)
	mux.HandleFunc("/api/alarms/", s.handleAlarmItem)

	// 认证与用户管理
	mux.HandleFunc("/api/auth/login", s.handleLogin)
	mux.HandleFunc("/api/auth/logout", s.handleLogout)
	mux.HandleFunc("/api/auth/me", s.handleAuthMe)
	mux.HandleFunc("/api/users", s.handleUsers)
	mux.HandleFunc("/api/users/", s.handleUserItem)

	// 集群与节点管理
	mux.HandleFunc("/api/nodes", s.handleNodes)
	mux.HandleFunc("/api/nodes/", s.handleNodeItem)
	mux.HandleFunc("/api/nodes/deploy", s.handleDeployWorker)
	mux.HandleFunc("/api/nodes/detect-disks", s.handleDetectDisks)
	mux.HandleFunc("/api/cluster/heartbeat", s.handleClusterHeartbeat)
	mux.HandleFunc("/api/cluster/binary", s.handleDownloadBinary)
	mux.HandleFunc("/api/agent/install.sh", s.handleAgentInstallScript)

	// 日志归档管理
	mux.HandleFunc("/api/archives", s.handleArchives)
	mux.HandleFunc("/api/archives/upload", s.handleUploadArchive)
	mux.HandleFunc("/api/archives/", s.handleArchiveItem)

	// 检索
	mux.HandleFunc("/api/search", s.handleSearch)

	// 规则与诊断报告
	mux.HandleFunc("/api/rules", s.handleRules)
	mux.HandleFunc("/api/rules/", s.handleRuleItem)
	mux.HandleFunc("/api/rules/reset-defaults", s.handleResetDefaultRules)
	mux.HandleFunc("/api/reports/", s.handleReports)

	addr := fmt.Sprintf("%s:%d", s.cfg.ListenHost, s.cfg.Port)
	s.server = &http.Server{
		Addr:    addr,
		Handler: s.corsMiddleware(mux),
	}

	log.Printf("[Manager] 管理组件控制台已启动: http://%s (广播地址: %s)", addr, s.cfg.AdvertiseIP)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.server.Shutdown(shutdownCtx)
	}()

	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// ================= 中间件与认证辅助 =================

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Cluster-Token")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authenticate(r *http.Request) (*model.User, error) {
	token := r.Header.Get("Authorization")
	token = strings.TrimPrefix(token, "Bearer ")
	if token == "" {
		if cookie, err := r.Cookie("session_token"); err == nil {
			token = cookie.Value
		}
	}
	if token == "" {
		return nil, fmt.Errorf("未登录或 Token 缺失")
	}

	val, ok := s.sessions.Load(token)
	if !ok {
		return nil, fmt.Errorf("登录会话已过期，请重新登录")
	}

	username := val.(string)
	user, err := s.store.GetUserByUsername(username)
	if err != nil {
		return nil, fmt.Errorf("用户不存在")
	}
	return user, nil
}

// ================= 认证接口 =================

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	user, err := s.store.GetUserByUsername(req.Username)
	if err != nil || user == nil {
		http.Error(w, "用户名或密码错误", http.StatusUnauthorized)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)); err != nil {
		http.Error(w, "用户名或密码错误", http.StatusUnauthorized)
		return
	}

	if user.Status != "active" {
		http.Error(w, "该用户已被禁用", http.StatusForbidden)
		return
	}

	// 生成 Session Token
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)
	s.sessions.Store(token, user.Username)

	// 更新使用量
	if usage, err := s.store.CalculateUserStorageUsage(user.Username); err == nil {
		user.UsedStorageBytes = usage
	}

	sanitized := *user
	sanitized.PasswordHash = ""

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"token": token,
		"user":  sanitized,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token != "" {
		s.sessions.Delete(token)
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	user, err := s.authenticate(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if usage, err := s.store.CalculateUserStorageUsage(user.Username); err == nil {
		user.UsedStorageBytes = usage
	}
	sanitized := *user
	sanitized.PasswordHash = ""
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sanitized)
}

// ================= 用户管理接口 =================

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	currentUser, err := s.authenticate(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if currentUser.Role != model.RoleAdmin {
		http.Error(w, "仅管理员可访问用户管理", http.StatusForbidden)
		return
	}

	switch r.Method {
	case http.MethodGet:
		users, err := s.store.ListUsers()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, u := range users {
			u.PasswordHash = ""
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(users)

	case http.MethodPost:
		var req struct {
			Username        string `json:"username"`
			Password        string `json:"password"`
			Role            string `json:"role"`
			SpaceQuotaBytes int64  `json:"space_quota_bytes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Username == "" || req.Password == "" {
			http.Error(w, "用户名和密码不能为空", http.StatusBadRequest)
			return
		}
		if req.Role == "" {
			req.Role = model.RoleUser
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		newUser := &model.User{
			ID:              fmt.Sprintf("usr_%d", time.Now().UnixNano()),
			Username:        req.Username,
			PasswordHash:    string(hash),
			Role:            req.Role,
			SpaceQuotaBytes: req.SpaceQuotaBytes,
			Status:          "active",
			CreatedAt:       time.Now(),
		}
		if err := s.store.SaveUser(newUser); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = s.store.EnsureUserDirectories(req.Username)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(newUser)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleUserItem(w http.ResponseWriter, r *http.Request) {
	currentUser, err := s.authenticate(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if currentUser.Role != model.RoleAdmin {
		http.Error(w, "权限不足", http.StatusForbidden)
		return
	}

	targetUsername := strings.TrimPrefix(r.URL.Path, "/api/users/")
	if targetUsername == "" {
		http.Error(w, "Username missing", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodPut:
		var req struct {
			Password        string `json:"password"`
			Role            string `json:"role"`
			SpaceQuotaBytes *int64 `json:"space_quota_bytes"`
			Status          string `json:"status"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		user, err := s.store.GetUserByUsername(targetUsername)
		if err != nil {
			http.Error(w, "用户未找到", http.StatusNotFound)
			return
		}
		if req.Password != "" {
			hash, _ := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
			user.PasswordHash = string(hash)
		}
		if req.Role != "" {
			user.Role = req.Role
		}
		if req.SpaceQuotaBytes != nil {
			user.SpaceQuotaBytes = *req.SpaceQuotaBytes
		}
		if req.Status != "" {
			user.Status = req.Status
		}
		_ = s.store.SaveUser(user)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(user)

	case http.MethodDelete:
		if err := s.store.DeleteUser(targetUsername); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ================= 集群与节点管理接口 =================

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	_, err := s.authenticate(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	nodes, err := s.store.ListNodes()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 检查心跳超时（15秒）
	now := time.Now()
	for _, n := range nodes {
		if n.Role == "worker" && n.Status == "online" && now.Sub(n.LastHeartbeat) > 15*time.Second {
			n.Status = "offline"
			_ = s.store.SaveNode(n)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(nodes)
}

func (s *Server) handleNodeItem(w http.ResponseWriter, r *http.Request) {
	currentUser, err := s.authenticate(r)
	if err != nil || currentUser.Role != model.RoleAdmin {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/api/nodes/")
	if r.Method == http.MethodDelete {
		_ = s.store.DeleteNode(id)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// handleDetectDisks 远程探测目标主机的物理磁盘
func (s *Server) handleDetectDisks(w http.ResponseWriter, r *http.Request) {
	currentUser, err := s.authenticate(r)
	if err != nil || currentUser.Role != model.RoleAdmin {
		http.Error(w, "只有管理员可探测远程磁盘", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var opts SSHDeployOptions
	if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if opts.Host == "" || opts.Username == "" {
		http.Error(w, "目标主机 IP 与 SSH 用户名不能为空", http.StatusBadRequest)
		return
	}

	disks, err := DetectRemoteDisks(opts)
	if err != nil {
		http.Error(w, fmt.Sprintf("探测远程磁盘失败: %v", err), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(disks)
}

// handleDeployWorker Web 界面上一键 SSH 远程部署安装业务组件
func (s *Server) handleDeployWorker(w http.ResponseWriter, r *http.Request) {
	currentUser, err := s.authenticate(r)
	if err != nil || currentUser.Role != model.RoleAdmin {
		http.Error(w, "只有管理员可执行远程部署", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var opts SSHDeployOptions
	if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if opts.Host == "" || opts.Username == "" {
		http.Error(w, "目标主机 IP 与 SSH 用户名不能为空", http.StatusBadRequest)
		return
	}

	// 自动补充 Manager 接入地址与集群凭据
	if opts.ManagerURL == "" {
		opts.ManagerURL = fmt.Sprintf("http://%s:%d", s.cfg.AdvertiseIP, s.cfg.Port)
	}
	opts.ClusterToken = s.cfg.ClusterToken

	// 创建临时节点记录
	nodeID := fmt.Sprintf("node_%s", strings.ReplaceAll(opts.Host, ".", "_"))
	node := &model.Node{
		ID:            nodeID,
		Name:          opts.NodeName,
		IP:            opts.Host,
		Port:          opts.WorkerPort,
		Role:          "worker",
		Status:        "installing",
		DiskDevice:    opts.DiskDevice,
		MountPoint:    opts.MountPoint,
		FSType:        opts.FSType,
		JoinedAt:      time.Now(),
		LastHeartbeat: time.Now(),
	}
	_ = s.store.SaveNode(node)

	// 异步执行远程 SSH 部署
	go func() {
		var logBuf bytes.Buffer
		mw := io.MultiWriter(os.Stdout, &logBuf)
		deployErr := DeployWorkerViaSSH(opts, mw)

		node.InstallLog = logBuf.String()
		if deployErr != nil {
			node.Status = "failed"
			log.Printf("[Manager] 节点 %s 远程一键部署失败: %v", opts.Host, deployErr)
		} else {
			node.Status = "online"
			node.LastHeartbeat = time.Now()
			log.Printf("[Manager] 节点 %s 远程一键部署成功并已接入！", opts.Host)
		}
		_ = s.store.SaveNode(node)
	}()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "远程安装任务已下发，正在进行 SSH 自动化推包与部署",
		"node_id": nodeID,
	})
}

// handleClusterHeartbeat Worker 心跳接收
func (s *Server) handleClusterHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := r.Header.Get("X-Cluster-Token")
	if token != s.cfg.ClusterToken {
		http.Error(w, "集群通信 Token 验证失败", http.StatusUnauthorized)
		return
	}

	var node model.Node
	if err := json.NewDecoder(r.Body).Decode(&node); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	node.LastHeartbeat = time.Now()
	node.Status = "online"
	_ = s.store.SaveNode(&node)

	// 自动消警：节点重新上报心跳，自动解除此前的离线失联告警
	_, _ = s.store.ResolveAlarm(node.ID, model.AlarmTypeNodeOffline)

	w.WriteHeader(http.StatusOK)
}

// handleDownloadBinary 供 Worker 节点远程下载的二进制
func (s *Server) handleDownloadBinary(w http.ResponseWriter, r *http.Request) {
	execPath, err := os.Executable()
	if err != nil {
		http.Error(w, "Binary not found", http.StatusNotFound)
		return
	}
	http.ServeFile(w, r, execPath)
}

// handleAgentInstallScript 返回一键安装业务组件的 Shell 脚本
func (s *Server) handleAgentInstallScript(w http.ResponseWriter, r *http.Request) {
	mgrURL := fmt.Sprintf("http://%s:%d", s.cfg.AdvertiseIP, s.cfg.Port)
	script := fmt.Sprintf(`#!/usr/bin/env bash
set -e
echo "=========================================="
echo "  分布式存储日志分析系统 - 业务组件一键接入"
echo "=========================================="
INSTALL_DIR="/opt/dist-log-worker"
mkdir -p "$INSTALL_DIR/bin" "$INSTALL_DIR/data" "$INSTALL_DIR/logs"

echo "[1/3] 正在下载业务组件可执行程序..."
curl -sSL -o "$INSTALL_DIR/bin/dist-log-analyzer" "%s/api/cluster/binary" || wget -qO "$INSTALL_DIR/bin/dist-log-analyzer" "%s/api/cluster/binary"
chmod +x "$INSTALL_DIR/bin/dist-log-analyzer"

echo "[2/3] 配置后台常驻服务并启动..."
pkill -f "$INSTALL_DIR/bin/dist-log-analyzer worker" || true

nohup "$INSTALL_DIR/bin/dist-log-analyzer" worker \
  --port=8081 \
  --manager-url="%s" \
  --cluster-token="%s" \
  --advertise-ip="$(hostname -I | awk '{print $1}')" \
  --data-dir="$INSTALL_DIR/data" > "$INSTALL_DIR/logs/worker.log" 2>&1 &

sleep 2
echo "[3/3] 验证启动状态..."
if pgrep -f "$INSTALL_DIR/bin/dist-log-analyzer worker" > /dev/null; then
    echo ">>> 业务组件安装成功！已顺利向管理节点注册上线！"
else
    echo ">>> 启动可能出现异常，请查看日志: $INSTALL_DIR/logs/worker.log"
fi
`, mgrURL, mgrURL, mgrURL, s.cfg.ClusterToken)

	w.Header().Set("Content-Type", "text/x-shellscript")
	_, _ = w.Write([]byte(script))
}

// ================= 日志归档与文件管理 =================

func (s *Server) handleArchives(w http.ResponseWriter, r *http.Request) {
	currentUser, err := s.authenticate(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	isAdmin := currentUser.Role == model.RoleAdmin
	archives, err := s.store.ListArchives(currentUser.Username, isAdmin)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(archives)
}

type workerUploadResult struct {
	Files       []*model.LogFileItem   `json:"files"`
	TotalLines  int64                  `json:"total_lines"`
	ExtractPath string                 `json:"extract_path"`
	Report      *model.DiagnosisReport `json:"report"`
}

func (s *Server) forwardUploadToWorker(node *model.Node, file io.Reader, filename string, archiveID, username, userID string, rulesList []*model.Rule) (*workerUploadResult, error) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(part, file); err != nil {
		return nil, err
	}

	_ = w.WriteField("archive_id", archiveID)
	_ = w.WriteField("username", username)
	_ = w.WriteField("user_id", userID)

	rulesData, _ := json.Marshal(rulesList)
	_ = w.WriteField("rules", string(rulesData))
	_ = w.Close()

	url := fmt.Sprintf("http://%s:%d/api/worker/storage/upload", node.IP, node.Port)
	req, err := http.NewRequest(http.MethodPost, url, &b)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("worker 存储失败 (%d): %s", resp.StatusCode, string(body))
	}

	var res workerUploadResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (s *Server) handleUploadArchive(w http.ResponseWriter, r *http.Request) {
	currentUser, err := s.authenticate(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 允许最大 2GB 日志包上传
	err = r.ParseMultipartForm(2048 << 20)
	if err != nil {
		http.Error(w, fmt.Sprintf("上传文件解析失败: %v", err), http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "请选择需要上传的日志文件压缩包", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// 存储配额校验
	if err := s.store.CheckUserQuota(currentUser.Username, header.Size); err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}

	targetNodeID := r.FormValue("target_node_id")
	archiveID := fmt.Sprintf("arc_%d", time.Now().UnixNano())

	// 检查目标存储节点
	var targetWorker *model.Node
	if targetNodeID != "" && targetNodeID != "manager_primary" && targetNodeID != "local" {
		targetWorker, _ = s.store.GetNode(targetNodeID)
	}

	// 如果选定的业务节点在线，直接存储并分发至该业务节点已挂载的存储硬盘
	if targetWorker != nil && targetWorker.Role == "worker" && targetWorker.Status == "online" {
		ruleList, _ := s.store.ListRules()
		log.Printf("[Manager] 用户 %s 选定将日志包 %s 存放在业务节点 %s (%s:%d)",
			currentUser.Username, header.Filename, targetWorker.Name, targetWorker.IP, targetWorker.Port)

		wRes, err := s.forwardUploadToWorker(targetWorker, file, header.Filename, archiveID, currentUser.Username, currentUser.ID, ruleList)
		if err != nil {
			http.Error(w, fmt.Sprintf("上传至目标业务节点存储失败: %v", err), http.StatusInternalServerError)
			return
		}

		archive := &model.LogArchive{
			ID:              archiveID,
			UserID:          currentUser.ID,
			Username:        currentUser.Username,
			Filename:        header.Filename,
			Size:            header.Size,
			Format:          detectArchiveFormat(header.Filename),
			Status:          "ready",
			FileCount:       len(wRes.Files),
			TotalLines:      wRes.TotalLines,
			ExtractPath:     wRes.ExtractPath,
			StorageNodeID:   targetWorker.ID,
			StorageNodeName: targetWorker.Name,
			StorageNodeIP:   targetWorker.IP,
			StorageNodePort: targetWorker.Port,
			AssignedWorker:  targetWorker.Name,
			UploadTime:      time.Now(),
			FinishTime:      time.Now(),
		}

		_ = s.store.SaveArchive(archive)
		if wRes.Report != nil {
			_ = s.store.SaveReport(wRes.Report)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(archive)
		return
	}

	// 默认保存在管理节点本地
	archiveDir := s.store.GetUserArchiveDir(currentUser.Username)
	_ = os.MkdirAll(archiveDir, 0755)

	destFilePath := filepath.Join(archiveDir, fmt.Sprintf("%s_%s", archiveID, header.Filename))
	destFile, err := os.Create(destFilePath)
	if err != nil {
		http.Error(w, fmt.Sprintf("创建存储文件失败: %v", err), http.StatusInternalServerError)
		return
	}
	defer destFile.Close()

	if _, err := io.Copy(destFile, file); err != nil {
		http.Error(w, fmt.Sprintf("保存文件失败: %v", err), http.StatusInternalServerError)
		return
	}

	archive := &model.LogArchive{
		ID:              archiveID,
		UserID:          currentUser.ID,
		Username:        currentUser.Username,
		Filename:        fmt.Sprintf("%s_%s", archiveID, header.Filename),
		Size:            header.Size,
		Format:          detectArchiveFormat(header.Filename),
		Status:          "uploading",
		StorageNodeID:   "manager_primary",
		StorageNodeName: "管理节点本地存储",
		UploadTime:      time.Now(),
	}

	_ = s.store.SaveArchive(archive)
	s.scheduler.DispatchAnalyzeTask(archive)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(archive)
}

func (s *Server) handleArchiveItem(w http.ResponseWriter, r *http.Request) {
	currentUser, err := s.authenticate(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/archives/")
	parts := strings.Split(path, "/")
	archiveID := parts[0]

	archive, err := s.store.GetArchive(archiveID)
	if err != nil {
		http.Error(w, "归档包不存在", http.StatusNotFound)
		return
	}

	isAdmin := currentUser.Role == model.RoleAdmin
	if !isAdmin && archive.Username != currentUser.Username {
		http.Error(w, "无权访问此日志包", http.StatusForbidden)
		return
	}

	// 获取文件树
	if len(parts) == 2 && parts[1] == "files" {
		if archive.ExtractPath == "" {
			http.Error(w, "日志仍在解包分析中，请稍后刷新", http.StatusBadRequest)
			return
		}

		// 若日志存放在远程业务节点，代理从该业务节点获取
		if archive.StorageNodeIP != "" && archive.StorageNodePort > 0 && archive.StorageNodeID != "manager_primary" {
			url := fmt.Sprintf("http://%s:%d/api/worker/storage/files?archive_id=%s&extract_path=%s",
				archive.StorageNodeIP, archive.StorageNodePort, archive.ID, archive.ExtractPath)
			resp, err := http.Get(url)
			if err == nil && resp.StatusCode == http.StatusOK {
				defer resp.Body.Close()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.Copy(w, resp.Body)
				return
			}
		}

		// 本地读取
		var files []*model.LogFileItem
		_ = filepath.Walk(archive.ExtractPath, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(archive.ExtractPath, p)
			if rel == "." {
				return nil
			}
			files = append(files, &model.LogFileItem{
				ArchiveID:    archive.ID,
				RelativePath: rel,
				Size:         info.Size(),
				ModTime:      info.ModTime(),
				IsDirectory:  info.IsDir(),
			})
			return nil
		})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(files)
		return
	}

	// 查看具体文件内容 (带分页行支持)
	if len(parts) == 2 && parts[1] == "file-content" {
		relPath := r.URL.Query().Get("path")
		if relPath == "" {
			http.Error(w, "缺少文件相对路径参数 path", http.StatusBadRequest)
			return
		}

		// 若日志存放在远程业务节点，代理从该节点获取
		if archive.StorageNodeIP != "" && archive.StorageNodePort > 0 && archive.StorageNodeID != "manager_primary" {
			url := fmt.Sprintf("http://%s:%d/api/worker/storage/file-content?path=%s&extract_path=%s&start_line=%s&limit=%s",
				archive.StorageNodeIP, archive.StorageNodePort, relPath, archive.ExtractPath,
				r.URL.Query().Get("start_line"), r.URL.Query().Get("limit"))
			resp, err := http.Get(url)
			if err == nil && resp.StatusCode == http.StatusOK {
				defer resp.Body.Close()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.Copy(w, resp.Body)
				return
			}
		}

		fullPath := filepath.Join(archive.ExtractPath, relPath)
		if !strings.HasPrefix(filepath.Clean(fullPath), filepath.Clean(archive.ExtractPath)) {
			http.Error(w, "非法文件路径", http.StatusForbidden)
			return
		}

		f, err := os.Open(fullPath)
		if err != nil {
			http.Error(w, "无法读取文件", http.StatusNotFound)
			return
		}
		defer f.Close()

		startLine, _ := strconv.Atoi(r.URL.Query().Get("start_line"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 500
		}
		if startLine <= 0 {
			startLine = 1
		}

		var lines []string
		scanner := bufio.NewScanner(f)
		buf := make([]byte, 1024*1024)
		scanner.Buffer(buf, 10*1024*1024)

		current := 0
		for scanner.Scan() {
			current++
			if current < startLine {
				continue
			}
			lines = append(lines, scanner.Text())
			if len(lines) >= limit {
				break
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"file_path":  relPath,
			"start_line": startLine,
			"line_count": len(lines),
			"lines":      lines,
		})
		return
	}

	// 删除归档包
	if r.Method == http.MethodDelete {
		if err := s.store.DeleteArchive(archiveID, currentUser.Username, isAdmin); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	// 单包元数据
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(archive)
}

// ================= 日志检索中心 =================

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	currentUser, err := s.authenticate(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var q model.SearchQuery
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if q.ArchiveID == "" {
		http.Error(w, "请选择要检索的日志包", http.StatusBadRequest)
		return
	}

	archive, err := s.store.GetArchive(q.ArchiveID)
	if err != nil {
		http.Error(w, "归档包不存在", http.StatusNotFound)
		return
	}

	if currentUser.Role != model.RoleAdmin && archive.Username != currentUser.Username {
		http.Error(w, "无权访问此归档日志", http.StatusForbidden)
		return
	}

	if archive.ExtractPath == "" || archive.Status != "ready" {
		http.Error(w, "该日志包尚未就绪，解包分析中", http.StatusBadRequest)
		return
	}

	// 若存放在指定的远程业务节点，向该节点下发检索任务
	if archive.StorageNodeIP != "" && archive.StorageNodePort > 0 && archive.StorageNodeID != "manager_primary" {
		url := fmt.Sprintf("http://%s:%d/api/worker/tasks/search", archive.StorageNodeIP, archive.StorageNodePort)
		taskReq := map[string]interface{}{
			"extract_dir": archive.ExtractPath,
			"query":       q,
		}
		data, _ := json.Marshal(taskReq)
		resp, err := http.Post(url, "application/json", bytes.NewReader(data))
		if err == nil && resp.StatusCode == http.StatusOK {
			defer resp.Body.Close()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.Copy(w, resp.Body)
			return
		}
	}

	// 本地执行检索
	resp, err := worker.SearchLogs(archive.ExtractPath, &q)
	if err != nil {
		http.Error(w, fmt.Sprintf("检索失败: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// ================= 规则管理与诊断报告 =================

func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	currentUser, err := s.authenticate(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodGet:
		ruleList, err := s.store.ListRules()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ruleList)

	case http.MethodPost:
		if currentUser.Role != model.RoleAdmin {
			http.Error(w, "仅管理员可配置规则", http.StatusForbidden)
			return
		}
		var rule model.Rule
		if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if rule.Name == "" || rule.Pattern == "" {
			http.Error(w, "规则名称与匹配表达式不能为空", http.StatusBadRequest)
			return
		}
		rule.ID = fmt.Sprintf("rule_custom_%d", time.Now().UnixNano())
		rule.CreatedAt = time.Now()
		rule.UpdatedAt = time.Now()
		if err := s.store.SaveRule(&rule); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rule)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleRuleItem(w http.ResponseWriter, r *http.Request) {
	currentUser, err := s.authenticate(r)
	if err != nil || currentUser.Role != model.RoleAdmin {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	ruleID := strings.TrimPrefix(r.URL.Path, "/api/rules/")

	switch r.Method {
	case http.MethodPut:
		var req model.Rule
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rule, err := s.store.GetRule(ruleID)
		if err != nil {
			http.Error(w, "Rule not found", http.StatusNotFound)
			return
		}
		rule.Name = req.Name
		rule.StorageType = req.StorageType
		rule.Severity = req.Severity
		rule.Pattern = req.Pattern
		rule.IsRegex = req.IsRegex
		rule.Description = req.Description
		rule.Suggestion = req.Suggestion
		rule.Enabled = req.Enabled
		_ = s.store.SaveRule(rule)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rule)

	case http.MethodDelete:
		_ = s.store.DeleteRule(ruleID)
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleResetDefaultRules(w http.ResponseWriter, r *http.Request) {
	currentUser, err := s.authenticate(r)
	if err != nil || currentUser.Role != model.RoleAdmin {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	for _, rule := range rules.DefaultPresets() {
		_ = s.store.SaveRule(rule)
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleReports(w http.ResponseWriter, r *http.Request) {
	currentUser, err := s.authenticate(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	archiveID := strings.TrimPrefix(r.URL.Path, "/api/reports/")
	archive, err := s.store.GetArchive(archiveID)
	if err != nil {
		http.Error(w, "归档不存在", http.StatusNotFound)
		return
	}

	if currentUser.Role != model.RoleAdmin && archive.Username != currentUser.Username {
		http.Error(w, "无权访问此报告", http.StatusForbidden)
		return
	}

	report, err := s.store.GetReport(archiveID)
	if err != nil {
		http.Error(w, "诊断报告生成中或不存在", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(report)
}

func detectArchiveFormat(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"):
		return "tar.gz"
	case strings.HasSuffix(lower, ".tgz"):
		return "tgz"
	case strings.HasSuffix(lower, ".zip"):
		return "zip"
	case strings.HasSuffix(lower, ".tar.bz2"):
		return "tar.bz2"
	case strings.HasSuffix(lower, ".tar"):
		return "tar"
	case strings.HasSuffix(lower, ".gz"):
		return "gz"
	case strings.HasSuffix(lower, ".bz2"):
		return "bz2"
	default:
		return "log"
	}
}

// ================= 高可用 HA API 处理函数 =================

func (s *Server) handleHAStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	status := s.ha.GetStatus()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func (s *Server) handleHAConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.ha.GetConfig()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cfg)
	case http.MethodPost:
		user, err := s.authenticate(r)
		if err != nil || user == nil || user.Role != model.RoleAdmin {
			http.Error(w, "需要管理员权限", http.StatusForbidden)
			return
		}
		var req config.HAConfig
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "请求格式错误: "+err.Error(), http.StatusBadRequest)
			return
		}

		// 持久化到 store
		if err := s.store.SaveHAConfig(&req); err != nil {
			http.Error(w, "持久化保存高可用配置失败: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// 动态热更新到运行中的 HAManager
		if err := s.ha.UpdateConfig(req); err != nil {
			http.Error(w, "动态更新高可用配置失败: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"message": "高可用与网络自检配置已保存并立即热生效",
			"config":  s.ha.GetConfig(),
			"status":  s.ha.GetStatus(),
		})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleHAHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 验证集群通信 Token
	token := r.Header.Get("X-Cluster-Token")
	if token != "" && token != s.cfg.ClusterToken {
		http.Error(w, "Unauthorized cluster token", http.StatusUnauthorized)
		return
	}

	res := map[string]interface{}{
		"role":      s.ha.role,
		"mode":      s.ha.mode,
		"node_name": s.cfg.NodeName,
		"timestamp": time.Now().Unix(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func (s *Server) handleHASnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := r.Header.Get("X-Cluster-Token")
	if token != s.cfg.ClusterToken {
		http.Error(w, "Unauthorized cluster token", http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=\"analyzer.db.snapshot\"")

	written, err := s.store.ExportSnapshot(w)
	if err != nil {
		log.Printf("[HA Snapshot] 导出快照流失败: %v (已写入 %d 字节)", err, written)
	} else {
		log.Printf("[HA Snapshot] 成功向备节点输出数据库快照流 (大小: %d 字节)", written)
	}
}

func (s *Server) handleHASwitchover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, err := s.authenticate(r)
	if err != nil || user == nil || user.Role != model.RoleAdmin {
		http.Error(w, "需要管理员权限", http.StatusForbidden)
		return
	}

	if err := s.ha.Switchover(); err != nil {
		http.Error(w, fmt.Sprintf("主备平滑倒换失败: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "主备平滑倒换成功",
		"status":  s.ha.GetStatus(),
	})
}

func (s *Server) handleHAPromote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := r.Header.Get("X-Cluster-Token")
	if token != s.cfg.ClusterToken {
		http.Error(w, "Unauthorized cluster token", http.StatusUnauthorized)
		return
	}

	if err := s.ha.PromoteToActive("收到对端平滑倒换请求"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func (s *Server) handleHADemote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := r.Header.Get("X-Cluster-Token")
	if token != s.cfg.ClusterToken {
		http.Error(w, "Unauthorized cluster token", http.StatusUnauthorized)
		return
	}

	if err := s.ha.DemoteToStandby("收到对端平滑倒换请求"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// ================= 告警系统 API 与节点心跳存活后台巡检 =================

// startNodeHealthAndAlarmMonitor 后台定时巡检 Worker 节点心跳，感知失联并自动触发/消警
func (s *Server) startNodeHealthAndAlarmMonitor(ctx context.Context) {
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// 仅 Active 管理节点执行集群节点心跳超期告警检测
			if !s.ha.IsActive() {
				continue
			}
			nodes, err := s.store.ListNodes()
			if err != nil {
				continue
			}
			now := time.Now()
			for _, node := range nodes {
				if node.Role == "worker" {
					if node.Status == "online" && now.Sub(node.LastHeartbeat) > 12*time.Second {
						node.Status = "offline"
						_ = s.store.SaveNode(node)

						// 触发/聚合严重告警
						_, _ = s.store.CreateOrAggregateAlarm(&model.Alarm{
							NodeID:    node.ID,
							NodeName:  node.Name,
							NodeIP:    node.IP,
							Component: "worker",
							AlarmType: model.AlarmTypeNodeOffline,
							Severity:  model.SeverityCritical,
							Title:     fmt.Sprintf("业务计算节点 [%s] 离线失联", node.Name),
							Message:   fmt.Sprintf("计算节点 %s (%s:%d) 超过 12 秒未发送心跳，可能已遭遇网络中断、服务器宕机或服务崩溃", node.Name, node.IP, node.Port),
						})
						log.Printf("[告警监控] 检测到计算节点 [%s] 离线失联，已自动生成 CRITICAL 严重告警！", node.Name)
					}
				}
			}
		}
	}
}

// handleAlarmReport 接收业务组件主动上报的异常告警
func (s *Server) handleAlarmReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := r.Header.Get("X-Cluster-Token")
	if token != s.cfg.ClusterToken {
		http.Error(w, "集群通信凭证无效", http.StatusUnauthorized)
		return
	}

	var req model.AlarmReportReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求格式错误: "+err.Error(), http.StatusBadRequest)
		return
	}

	nodeName := req.NodeID
	nodeIP := r.RemoteAddr
	if n, err := s.store.GetNode(req.NodeID); err == nil && n != nil {
		nodeName = n.Name
		nodeIP = n.IP
	}

	alarm := &model.Alarm{
		NodeID:    req.NodeID,
		NodeName:  nodeName,
		NodeIP:    nodeIP,
		Component: "worker",
		AlarmType: req.AlarmType,
		Severity:  req.Severity,
		Title:     req.Title,
		Message:   req.Message,
	}

	created, err := s.store.CreateOrAggregateAlarm(alarm)
	if err != nil {
		http.Error(w, "保存告警失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("[告警中心] 收到业务组件 [%s] 异常告警: %s (级别: %s, 频次: %d)",
		nodeName, req.Title, req.Severity, created.Count)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
		"alarm":  created,
	})
}

// handleAlarms 获取告警列表
func (s *Server) handleAlarms(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, err := s.authenticate(r)
	if err != nil || user == nil {
		http.Error(w, "请先登录", http.StatusUnauthorized)
		return
	}

	statusFilter := r.URL.Query().Get("status")
	list, err := s.store.ListAlarms(statusFilter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []*model.Alarm{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

// handleAlarmSummary 获取告警全局指标统计 (未恢复数及各级别统计)
func (s *Server) handleAlarmSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sum := s.store.GetAlarmSummary()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sum)
}

// handleClearResolvedAlarms 一键清空已恢复告警
func (s *Server) handleClearResolvedAlarms(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, err := s.authenticate(r)
	if err != nil || user == nil || user.Role != model.RoleAdmin {
		http.Error(w, "需要管理员权限", http.StatusForbidden)
		return
	}
	count, err := s.store.ClearResolvedAlarms()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"message": fmt.Sprintf("已成功清理 %d 条已恢复的历史告警", count),
		"count":   count,
	})
}

// handleAlarmItem 单条告警的操作 (确认、标记解决、删除)
func (s *Server) handleAlarmItem(w http.ResponseWriter, r *http.Request) {
	user, err := s.authenticate(r)
	if err != nil || user == nil || user.Role != model.RoleAdmin {
		http.Error(w, "需要管理员权限", http.StatusForbidden)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/alarms/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "缺少告警 ID", http.StatusBadRequest)
		return
	}
	alarmID := parts[0]

	if len(parts) == 2 {
		action := parts[1]
		if action == "ack" && r.Method == http.MethodPost {
			if err := s.store.AcknowledgeAlarm(alarmID); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "告警已标记为已确认"})
			return
		} else if action == "resolve" && r.Method == http.MethodPost {
			if err := s.store.ManualResolveAlarm(alarmID); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "告警已标记为已解除"})
			return
		}
	}

	if r.Method == http.MethodDelete {
		if err := s.store.DeleteAlarm(alarmID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "告警已删除"})
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

