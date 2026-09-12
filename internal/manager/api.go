package manager

import (
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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dist-log-analyzer/internal/config"
	"dist-log-analyzer/internal/indexer"
	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/rules"
	"dist-log-analyzer/internal/store"
	"dist-log-analyzer/internal/worker"

	"golang.org/x/crypto/bcrypt"
)

type Server struct {
	cfg                  *config.Config
	store                *store.Store
	scheduler            *Scheduler
	ha                   *HAManager
	sessions             sync.Map // token -> username
	server               *http.Server
	staticFS             http.FileSystem
	httpClient           *http.Client
	searchRequestsTotal  uint64
	searchLatencySumMs   uint64
}

func NewServer(cfg *config.Config, s *store.Store, staticFS http.FileSystem) *Server {
	transport := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 50,
		IdleConnTimeout:     90 * time.Second,
	}
	srv := &Server{
		cfg:       cfg,
		store:     s,
		scheduler: NewScheduler(s),
		ha:        NewHAManager(cfg, s),
		staticFS:  staticFS,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Minute,
		},
	}
	// 初始化或增量补充内置通用分布式系统预设规则
	for _, r := range rules.DefaultPresets() {
		if existing, _ := s.GetRule(r.ID); existing == nil {
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

	// 静态前端文件托管
	if s.staticFS != nil {
		fileServer := http.FileServer(s.staticFS)
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Path
			if strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".css") || strings.HasSuffix(p, ".svg") || strings.HasSuffix(p, ".png") || strings.HasSuffix(p, ".woff2") {
				w.Header().Set("Cache-Control", "public, max-age=86400")
			} else if strings.HasSuffix(p, ".html") || p == "/" {
				w.Header().Set("Cache-Control", "no-cache")
			}
			fileServer.ServeHTTP(w, r)
		})
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
	mux.HandleFunc("/api/nodes/maintenance", s.handleNodeMaintenance)
	mux.HandleFunc("/api/nodes/", s.handleNodeItem)
	mux.HandleFunc("/api/nodes/deploy", s.handleDeployWorker)
	mux.HandleFunc("/api/nodes/detect-disks", s.handleDetectDisks)
	mux.HandleFunc("/api/cluster/heartbeat", s.handleClusterHeartbeat)
	mux.HandleFunc("/api/cluster/archive-callback", s.handleClusterArchiveCallback)
	mux.HandleFunc("/api/cluster/binary", s.handleDownloadBinary)
	mux.HandleFunc("/api/agent/install.sh", s.handleAgentInstallScript)
	mux.HandleFunc("/api/system/backup", s.handleSystemBackup)
	mux.HandleFunc("/api/system/upgrade", s.handleSystemUpgrade)

	// 日志归档管理
	mux.HandleFunc("/api/archives", s.handleArchives)
	mux.HandleFunc("/api/archives/check-tag", s.handleCheckArchiveTag)
	mux.HandleFunc("/api/archives/upload", s.handleUploadArchive)
	mux.HandleFunc("/api/archives/pin", s.handleArchivePin)
	mux.HandleFunc("/api/archives/", s.handleArchiveItem)
	mux.HandleFunc("/api/settings/retention", s.handleRetentionSettings)

	// 检索
	mux.HandleFunc("/api/search", s.handleSearch)

	// 规则与诊断报告
	mux.HandleFunc("/api/rules", s.handleRules)
	mux.HandleFunc("/api/rules/reset", s.handleResetDefaultRules)
	mux.HandleFunc("/api/rules/", s.handleRuleItem)
	mux.HandleFunc("/api/reports/", s.handleReports)

	// 通用分布式日志分析增强：基准差分对比与模板聚类
	mux.HandleFunc("/api/analysis/diff", s.handleAnalysisDiff)
	mux.HandleFunc("/api/analysis/templates", s.handleAnalysisTemplates)

	// 运维监控度量与微服务标准探针
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/metrics", s.handleMetrics)

	addr := fmt.Sprintf("%s:%d", s.cfg.ListenHost, s.cfg.Port)
	s.server = &http.Server{
		Addr:    addr,
		Handler: s.corsMiddleware(mux),
	}

	log.Printf("[Manager] 管理组件控制台已启动: http://%s (广播地址: %s)", addr, s.cfg.AdvertiseIP)

	// 启动后台日志生命周期与磁盘容量自愈巡检协程
	go s.StartRetentionLoop(ctx)

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
		token = r.URL.Query().Get("token")
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
	currentUser, err := s.authenticate(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodGet:
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

	case http.MethodPost:
		if currentUser.Role != model.RoleAdmin {
			http.Error(w, "只有管理员可添加业务组件节点", http.StatusForbidden)
			return
		}

		var req struct {
			Name       string `json:"name"`
			IP         string `json:"ip"`
			Port       int    `json:"port"`
			MountPoint string `json:"mount_point"`
			DiskDevice string `json:"disk_device"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		req.IP = strings.TrimSpace(req.IP)
		if req.IP == "" || req.Port <= 0 {
			http.Error(w, "业务组件节点 IP 地址与端口不能为空且端口必须大于0", http.StatusBadRequest)
			return
		}

		// 核心唯一性防呆校验：以 IP 和端口作为唯一标识，禁止重复添加！
		if existing, err := s.store.GetNodeByAddr(req.IP, req.Port); err == nil && existing != nil {
			if existing.Status != "failed" {
				http.Error(w, fmt.Sprintf("分布式业务组件节点添加失败：节点 [%s:%d] 已存在于集群中 (名称: %s, 状态: %s)，禁止重复添加！", req.IP, req.Port, existing.Name, existing.Status), http.StatusBadRequest)
				return
			}
		}

		if req.Name == "" {
			req.Name = fmt.Sprintf("worker-%s-%d", strings.ReplaceAll(req.IP, ".", "-"), req.Port)
		}
		nodeID := fmt.Sprintf("worker_%s_%d", strings.ReplaceAll(req.IP, ".", "_"), req.Port)
		_ = s.store.ClearDecommissionedNode(nodeID)

		newNode := &model.Node{
			ID:            nodeID,
			Name:          req.Name,
			IP:            req.IP,
			Port:          req.Port,
			Role:          "worker",
			Status:        "online",
			MountPoint:    req.MountPoint,
			DiskDevice:    req.DiskDevice,
			JoinedAt:      time.Now(),
			LastHeartbeat: time.Now(),
		}
		if err := s.store.SaveNode(newNode); err != nil {
			http.Error(w, fmt.Sprintf("保存业务组件节点失败: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(newNode)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

type RemoveNodeRequest struct {
	Action       string `json:"action"`         // "migrate" | "retain" | "delete"
	TargetNodeID string `json:"target_node_id"` // 当 action == "migrate" 时的目标节点 ID
}

func (s *Server) handleNodeItem(w http.ResponseWriter, r *http.Request) {
	currentUser, err := s.authenticate(r)
	if err != nil || currentUser.Role != model.RoleAdmin {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/nodes/")
	parts := strings.Split(path, "/")
	id := parts[0]

	// 支持 POST /api/nodes/{id}/remove
	if len(parts) >= 2 && parts[1] == "remove" && r.Method == http.MethodPost {
		var req RemoveNodeRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.handleRemoveNode(w, r, id, req)
		return
	}

	if r.Method == http.MethodDelete {
		originNode, _ := s.store.GetNode(id)
		_ = s.store.DecommissionNode(id)
		_, _ = s.store.ResolveAlarm(id, model.AlarmTypeNodeOffline)
		if originNode != nil && originNode.IP != "" && originNode.Port > 0 {
			go func(ip string, port int, token string) {
				decomURL := fmt.Sprintf("http://%s:%d/api/worker/decommission", ip, port)
				req, err := http.NewRequest(http.MethodPost, decomURL, nil)
				if err == nil {
					req.Header.Set("X-Cluster-Token", token)
					client := &http.Client{Timeout: 3 * time.Second}
					_, _ = client.Do(req)
				}
			}(originNode.IP, originNode.Port, s.cfg.ClusterToken)
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

func (s *Server) handleRemoveNode(w http.ResponseWriter, r *http.Request, nodeID string, req RemoveNodeRequest) {
	originNode, err := s.store.GetNode(nodeID)
	if err != nil {
		http.Error(w, "目标节点不存在", http.StatusNotFound)
		return
	}

	archives, _ := s.store.ListArchivesByNode(nodeID)
	action := req.Action
	if action == "" {
		action = "retain" // 默认保留数据
	}

	migratedCount := 0

	switch action {
	case "migrate":
		targetNodeID := req.TargetNodeID
		if targetNodeID == "" || targetNodeID == nodeID {
			http.Error(w, "迁移数据必须指定有效的接收目标节点", http.StatusBadRequest)
			return
		}

		if targetNodeID == "manager_primary" || targetNodeID == "local" {
			http.Error(w, "禁止将数据迁移至管理节点系统盘，日志只能保存在业务存储节点上以避免系统盘被占满", http.StatusBadRequest)
			return
		}

		targetWorker, err := s.store.GetNode(targetNodeID)
		if err != nil || targetWorker == nil || targetWorker.Role != "worker" || targetWorker.Status != "online" {
			http.Error(w, "指定的目标接收业务节点不存在或已下线", http.StatusBadRequest)
			return
		}

		rulesList, _ := s.store.ListRules()

		for _, arc := range archives {
			// 1. 获取原节点上的压缩包文件流
			var reader io.ReadCloser
			if originNode.Role == "manager" || originNode.IP == "" {
				localPath := filepath.Join(s.store.GetUserArchiveDir(arc.Username), arc.Filename)
				f, err := os.Open(localPath)
				if err != nil {
					continue
				}
				reader = f
			} else {
				dlURL := fmt.Sprintf("http://%s:%d/api/worker/storage/download-archive?username=%s&filename=%s",
					originNode.IP, originNode.Port, url.QueryEscape(arc.Username), url.QueryEscape(arc.Filename))
				resp, err := http.Get(dlURL)
				if err != nil || resp.StatusCode != http.StatusOK {
					if resp != nil {
						resp.Body.Close()
					}
					continue
				}
				reader = resp.Body
			}

			upRes, err := s.forwardUploadToWorker(targetWorker, reader, arc.Filename, arc.ID, arc.Username, arc.UserID, rulesList)
			reader.Close()
			if err == nil && upRes != nil {
				arc.StorageNodeID = targetWorker.ID
				arc.StorageNodeName = targetWorker.Name
				arc.StorageNodeIP = targetWorker.IP
				arc.StorageNodePort = targetWorker.Port
				arc.ExtractPath = upRes.ExtractPath
				if len(upRes.Files) > 0 {
					arc.FileCount = len(upRes.Files)
				}
				if upRes.TotalLines > 0 {
					arc.TotalLines = upRes.TotalLines
				}
				_ = s.store.SaveArchive(arc)
				migratedCount++

				// 通知原 Worker 释放该日志包磁盘空间
				if originNode.IP != "" && originNode.Port > 0 {
					cleanURL := fmt.Sprintf("http://%s:%d/api/worker/storage/clean-archive?username=%s&archive_id=%s&filename=%s",
						originNode.IP, originNode.Port, url.QueryEscape(arc.Username), url.QueryEscape(arc.ID), url.QueryEscape(arc.Filename))
					cleanReq, _ := http.NewRequest(http.MethodPost, cleanURL, nil)
					client := &http.Client{Timeout: 5 * time.Second}
					_, _ = client.Do(cleanReq)
				}
			}
		}

		_ = s.store.DecommissionNode(nodeID)
		_, _ = s.store.ResolveAlarm(nodeID, model.AlarmTypeNodeOffline)
		if originNode.IP != "" && originNode.Port > 0 {
			go func(ip string, port int, token string) {
				decomURL := fmt.Sprintf("http://%s:%d/api/worker/decommission", ip, port)
				req, err := http.NewRequest(http.MethodPost, decomURL, nil)
				if err == nil {
					req.Header.Set("X-Cluster-Token", token)
					client := &http.Client{Timeout: 3 * time.Second}
					_, _ = client.Do(req)
				}
			}(originNode.IP, originNode.Port, s.cfg.ClusterToken)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":         "ok",
			"message":        fmt.Sprintf("业务节点已成功移除，已将 %d 个日志归档包迁移至目标节点", migratedCount),
			"migrated_count": migratedCount,
		})
		return

	case "delete":
		for _, arc := range archives {
			if originNode.IP != "" && originNode.Port > 0 {
				cleanURL := fmt.Sprintf("http://%s:%d/api/worker/storage/clean-archive?username=%s&archive_id=%s&filename=%s",
					originNode.IP, originNode.Port, url.QueryEscape(arc.Username), url.QueryEscape(arc.ID), url.QueryEscape(arc.Filename))
				cleanReq, _ := http.NewRequest(http.MethodPost, cleanURL, nil)
				client := &http.Client{Timeout: 5 * time.Second}
				_, _ = client.Do(cleanReq)
			}
			_ = s.store.DeleteArchive(arc.ID, arc.Username, true)
		}
		_ = s.store.DecommissionNode(nodeID)
		_, _ = s.store.ResolveAlarm(nodeID, model.AlarmTypeNodeOffline)
		if originNode.IP != "" && originNode.Port > 0 {
			go func(ip string, port int, token string) {
				decomURL := fmt.Sprintf("http://%s:%d/api/worker/decommission", ip, port)
				req, err := http.NewRequest(http.MethodPost, decomURL, nil)
				if err == nil {
					req.Header.Set("X-Cluster-Token", token)
					client := &http.Client{Timeout: 3 * time.Second}
					_, _ = client.Do(req)
				}
			}(originNode.IP, originNode.Port, s.cfg.ClusterToken)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "ok",
			"message": fmt.Sprintf("业务节点已成功移除，其关联的 %d 个日志归档包数据已彻底清理", len(archives)),
		})
		return

	case "retain":
		fallthrough
	default:
		_ = s.store.DecommissionNode(nodeID)
		_, _ = s.store.ResolveAlarm(nodeID, model.AlarmTypeNodeOffline)
		if originNode.IP != "" && originNode.Port > 0 {
			go func(ip string, port int, token string) {
				decomURL := fmt.Sprintf("http://%s:%d/api/worker/decommission", ip, port)
				req, err := http.NewRequest(http.MethodPost, decomURL, nil)
				if err == nil {
					req.Header.Set("X-Cluster-Token", token)
					client := &http.Client{Timeout: 3 * time.Second}
					_, _ = client.Do(req)
				}
			}(originNode.IP, originNode.Port, s.cfg.ClusterToken)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "ok",
			"message": fmt.Sprintf("业务节点已成功从集群注销，其物理存储的 %d 个日志归档包数据已完整保留", len(archives)),
		})
		return
	}
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
	if disks == nil {
		disks = []model.DiskInfo{}
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

	opts.Host = strings.TrimSpace(opts.Host)
	if opts.WorkerPort <= 0 {
		opts.WorkerPort = 8081
	}

	disks := opts.GetDiskList()
	if len(disks) == 0 {
		http.Error(w, "安全策略限制：严禁使用系统盘存放日志！请至少选择一块独立的物理存储盘", http.StatusBadRequest)
		return
	}

	// 核心唯一性防呆校验：以 IP 和端口作为唯一标识，检查所有待部署实例的端口是否重复，禁止重复添加！
	for idx := range disks {
		targetPort := opts.WorkerPort + idx
		if existing, err := s.store.GetNodeByAddr(opts.Host, targetPort); err == nil && existing != nil {
			if existing.Status != "failed" {
				http.Error(w, fmt.Sprintf("分布式业务组件节点添加失败：节点 [%s:%d] 已存在于集群中 (名称: %s, 状态: %s)，禁止重复添加！", opts.Host, targetPort, existing.Name, existing.Status), http.StatusBadRequest)
				return
			}
		}
	}

	// 自动补充 Manager 接入地址与集群凭据
	if opts.ManagerURL == "" {
		opts.ManagerURL = fmt.Sprintf("http://%s:%d", s.cfg.AdvertiseIP, s.cfg.Port)
	}
	opts.ClusterToken = s.cfg.ClusterToken

	// 创建临时节点记录
	nodeID := fmt.Sprintf("node_%s", strings.ReplaceAll(opts.Host, ".", "_"))

	// 部署前清理可能存在的历史注销/下线记录，允许节点重新加入集群
	_ = s.store.ClearDecommissionedNode(nodeID)
	for idx, d := range disks {
		diskName := filepath.Base(d)
		subNodeID := fmt.Sprintf("worker_%s-%s_%d", opts.NodeName, diskName, opts.WorkerPort+idx)
		_ = s.store.ClearDecommissionedNode(subNodeID)
	}

	node := &model.Node{
		ID:            nodeID,
		Name:          opts.NodeName,
		IP:            opts.Host,
		Port:          opts.WorkerPort,
		Role:          "worker",
		Status:        "installing",
		DiskDevice:    strings.Join(disks, ", "),
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
			_ = s.store.SaveNode(node)
		} else {
			// 如果部署成功且创建了具体的磁盘子 Worker，清理临时的部署占位节点记录
			_ = s.store.DeleteNode(node.ID)
			log.Printf("[Manager] 节点 %s 远程一键部署成功，各磁盘独立 Worker 实例已接管服务", opts.Host)
		}
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

	node.IP = strings.TrimSpace(node.IP)

	// 核心唯一性保障：以 IP 和端口作为唯一标识，统一管理节点记录
	existing, err := s.store.GetNodeByAddr(node.IP, node.Port)
	if err == nil && existing != nil {
		if s.store.IsNodeDecommissioned(existing.ID) || s.store.IsNodeDecommissioned(node.ID) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusGone) // 410 Gone
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":  "decommissioned",
				"action":  "shutdown",
				"message": "该节点已从集群移除注销，拒绝心跳",
			})
			return
		}
		// 继承并统一节点 ID 与元数据，杜绝同一 IP:Port 在集群中出现重复节点
		node.ID = existing.ID
		if node.Name == "" {
			node.Name = existing.Name
		}
		if node.DiskDevice == "" {
			node.DiskDevice = existing.DiskDevice
		}
		if node.JoinedAt.IsZero() {
			node.JoinedAt = existing.JoinedAt
		}
	} else {
		if s.store.IsNodeDecommissioned(node.ID) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusGone) // 410 Gone
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":  "decommissioned",
				"action":  "shutdown",
				"message": "该节点已从集群移除注销，拒绝心跳",
			})
			return
		}
		if node.ID == "" {
			node.ID = fmt.Sprintf("worker_%s_%d", strings.ReplaceAll(node.IP, ".", "_"), node.Port)
		}
		node.JoinedAt = time.Now()
	}

	node.LastHeartbeat = time.Now()
	if existing != nil && existing.Status == "maintenance" {
		node.Status = "maintenance"
	} else {
		node.Status = "online"
	}
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

INSTALL_LOG="/tmp/dist-log-worker-install.log"
mkdir -p "$(dirname "$INSTALL_LOG")" 2>/dev/null || true
{
    echo "=========================================="
    echo "  业务组件一键接入安装日志"
    echo "  启动时间: $(date '+%%Y-%%m-%%d %%H:%%M:%%S')"
    echo "  主机名称: $(hostname 2>/dev/null || echo 'unknown')"
    echo "  执行用户: $(whoami) (UID: $(id -u))"
    echo "=========================================="
} >> "$INSTALL_LOG"

exec > >(tee -a "$INSTALL_LOG") 2>&1

on_agent_install_error() {
    local exit_code=$?
    local line_no=$1
    local cmd_str=$2
    echo ""
    echo "❌ [安装异常终止] 脚本在第 $line_no 行执行失败: $cmd_str (退出码: $exit_code)"
    echo "  ▶ 诊断日志已保存至: $INSTALL_LOG"
    if [ -d "/opt/dist-log-worker/logs" ]; then
        cp -f "$INSTALL_LOG" "/opt/dist-log-worker/logs/install.log" 2>/dev/null || true
    fi
    exit "$exit_code"
}
trap 'on_agent_install_error ${LINENO} "$BASH_COMMAND"' ERR

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
    cp -f "$INSTALL_LOG" "$INSTALL_DIR/logs/install.log" 2>/dev/null || true
    echo ">>> 完整安装日志已归档至: $INSTALL_DIR/logs/install.log"
else
    echo ">>> 启动可能出现异常，请查看日志: $INSTALL_DIR/logs/worker.log 或 $INSTALL_LOG"
    exit 1
fi
`, mgrURL, mgrURL, mgrURL, s.cfg.ClusterToken)

	w.Header().Set("Content-Type", "text/x-shellscript")
	_, _ = w.Write([]byte(script))
}

// ================= 日志归档与文件管理 =================

func parseTags(tagsStr string) []string {
	tagsStr = strings.TrimSpace(tagsStr)
	if tagsStr == "" {
		return nil
	}
	var jsonTags []string
	if err := json.Unmarshal([]byte(tagsStr), &jsonTags); err == nil {
		var res []string
		for _, t := range jsonTags {
			t = strings.TrimSpace(t)
			if t != "" {
				res = append(res, t)
			}
		}
		return res
	}

	cleaned := strings.ReplaceAll(tagsStr, "，", ",")
	cleaned = strings.ReplaceAll(cleaned, "；", ",")
	cleaned = strings.ReplaceAll(cleaned, ";", ",")
	var res []string
	seen := make(map[string]bool)
	for _, part := range strings.Split(cleaned, ",") {
		part = strings.TrimSpace(part)
		if part != "" && !seen[part] {
			seen[part] = true
			res = append(res, part)
		}
	}
	return res
}

// findDuplicateTag 检查给定的标签列表中是否有任何一个与现有归档冲突（不区分大小写，可排除指定的归档自身）
func (s *Server) findDuplicateTag(tags []string, excludeArchiveID string) (duplicateTag string, matchedArchive *model.LogArchive) {
	if len(tags) == 0 {
		return "", nil
	}
	existingArchives, err := s.store.ListArchives("", true)
	if err != nil {
		return "", nil
	}
	for _, candidate := range tags {
		cLower := strings.ToLower(strings.TrimSpace(candidate))
		if cLower == "" {
			continue
		}
		for _, a := range existingArchives {
			if excludeArchiveID != "" && a.ID == excludeArchiveID {
				continue
			}
			for _, existTag := range a.Tags {
				if strings.ToLower(strings.TrimSpace(existTag)) == cLower {
					return candidate, a
				}
			}
		}
	}
	return "", nil
}

func (s *Server) handleCheckArchiveTag(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tag := strings.TrimSpace(r.URL.Query().Get("tag"))
	excludeID := strings.TrimSpace(r.URL.Query().Get("exclude_id"))

	w.Header().Set("Content-Type", "application/json")
	if tag == "" {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"exists": false,
			"tag":    "",
		})
		return
	}

	tags := parseTags(tag)
	if len(tags) == 0 {
		tags = []string{tag}
	}

	dupTag, matchedArc := s.findDuplicateTag(tags, excludeID)
	if dupTag != "" && matchedArc != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"exists":             true,
			"tag":                dupTag,
			"matched_archive_id": matchedArc.ID,
			"matched_filename":   matchedArc.Filename,
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"exists": false,
		"tag":    tag,
	})
}


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

	tagFilter := strings.TrimSpace(r.URL.Query().Get("tag"))
	keyword := strings.TrimSpace(r.URL.Query().Get("keyword"))

	if tagFilter != "" || keyword != "" {
		tagLower := strings.ToLower(tagFilter)
		kwLower := strings.ToLower(keyword)
		var filtered []*model.LogArchive
		for _, a := range archives {
			matchTag := true
			if tagLower != "" {
				matchTag = false
				for _, t := range a.Tags {
					if strings.ToLower(t) == tagLower || strings.Contains(strings.ToLower(t), tagLower) {
						matchTag = true
						break
					}
				}
			}

			matchKw := true
			if kwLower != "" {
				matchKw = strings.Contains(strings.ToLower(a.Filename), kwLower) ||
					strings.Contains(strings.ToLower(a.Remark), kwLower)
				if !matchKw {
					for _, t := range a.Tags {
						if strings.Contains(strings.ToLower(t), kwLower) {
							matchKw = true
							break
						}
					}
				}
			}

			if matchTag && matchKw {
				filtered = append(filtered, a)
			}
		}
		archives = filtered
	}

	if archives == nil {
		archives = make([]*model.LogArchive, 0)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(archives)
}

type workerUploadResult struct {
	Status      string                 `json:"status"`
	ArchiveID   string                 `json:"archive_id"`
	Files       []*model.LogFileItem   `json:"files,omitempty"`
	TotalLines  int64                  `json:"total_lines,omitempty"`
	ExtractPath string                 `json:"extract_path"`
	Report      *model.DiagnosisReport `json:"report,omitempty"`
}

func (s *Server) forwardUploadToWorker(node *model.Node, file io.Reader, filename string, archiveID, username, userID string, rulesList []*model.Rule) (*workerUploadResult, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	rulesData, _ := json.Marshal(rulesList)

	go func() {
		var copyErr error
		defer func() {
			if copyErr != nil {
				_ = pw.CloseWithError(copyErr)
			} else {
				_ = pw.Close()
			}
		}()

		if err := mw.WriteField("archive_id", archiveID); err != nil {
			copyErr = err
			return
		}
		if err := mw.WriteField("username", username); err != nil {
			copyErr = err
			return
		}
		if err := mw.WriteField("user_id", userID); err != nil {
			copyErr = err
			return
		}
		if err := mw.WriteField("rules", string(rulesData)); err != nil {
			copyErr = err
			return
		}

		part, err := mw.CreateFormFile("file", filename)
		if err != nil {
			copyErr = err
			return
		}

		// 64KB 流式传输缓冲区，避免大文件占用堆内存
		buf := make([]byte, 64*1024)
		if _, copyErr = io.CopyBuffer(part, file, buf); copyErr != nil {
			return
		}
		copyErr = mw.Close()
	}()

	url := fmt.Sprintf("http://%s:%d/api/worker/storage/upload", node.IP, node.Port)
	req, err := http.NewRequest(http.MethodPost, url, pr)
	if err != nil {
		_ = pr.Close()
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := s.httpClient.Do(req)
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

func (s *Server) handleClusterArchiveCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := r.Header.Get("X-Cluster-Token")
	if token != s.cfg.ClusterToken {
		http.Error(w, "集群通信 Token 验证失败", http.StatusUnauthorized)
		return
	}

	var req model.ArchiveCallbackReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	archive, err := s.store.GetArchive(req.ArchiveID)
	if err != nil || archive == nil {
		http.Error(w, "归档包记录未找到", http.StatusNotFound)
		return
	}

	archive.Status = req.Status
	archive.ErrorMsg = req.ErrorMsg
	if req.Status == "ready" {
		if req.FileCount > 0 {
			archive.FileCount = req.FileCount
		}
		if req.TotalLines > 0 {
			archive.TotalLines = req.TotalLines
		}
		archive.FinishTime = time.Now()
		if req.ExtractPath != "" {
			archive.ExtractPath = req.ExtractPath
		}
	}

	if err := s.store.SaveArchive(archive); err != nil {
		http.Error(w, fmt.Sprintf("保存归档状态失败: %v", err), http.StatusInternalServerError)
		return
	}

	if req.Report != nil {
		_ = s.store.SaveReport(req.Report)
	}

	log.Printf("[Manager Callback] 归档包 %s 状态已成功更新为 %s (总行数: %d, 文件数: %d)",
		archive.ID, archive.Status, archive.TotalLines, archive.FileCount)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
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
	tagsStr := r.FormValue("tags")
	remark := r.FormValue("remark")
	tags := parseTags(tagsStr)

	// 1. 标签必填且不可为空 (标签为唯一标识)
	if len(tags) == 0 {
		http.Error(w, "必须输入日志归档标签，标签为归档唯一标识", http.StatusBadRequest)
		return
	}

	// 2. 标签唯一性检查：不可与系统中任何已有归档标签重复
	if dupTag, matchedArc := s.findDuplicateTag(tags, ""); dupTag != "" {
		http.Error(w, fmt.Sprintf("标签 “%s” 已被归档包 [%s] 占用，归档标签必须保持全系统唯一", dupTag, matchedArc.Filename), http.StatusBadRequest)
		return
	}

	archiveID := fmt.Sprintf("arc_%d", time.Now().UnixNano())

	// 检查目标存储节点：日志只能保存在业务存储上，严禁写入管理节点系统盘
	if targetNodeID == "manager_primary" || targetNodeID == "local" {
		http.Error(w, "禁止选择管理节点存储日志！日志包只能保存在业务存储节点上，避免管理节点系统盘被占满。", http.StatusBadRequest)
		return
	}

	var targetWorker *model.Node
	if targetNodeID == "auto" || targetNodeID == "" {
		// 自动智能调度：优先使用已使用容量最低的业务节点存放
		targetWorker = s.scheduler.PickLowestUsageWorker(header.Size)
		if targetWorker != nil {
			log.Printf("[Manager] 智能容量调度生效：选定已用容量最低的业务节点 %s (已用: %d MB, 剩余: %d MB) 存放日志包 %s",
				targetWorker.Name, targetWorker.Resource.DiskUsedMB, targetWorker.Resource.DiskFreeMB, header.Filename)
		}
	} else {
		targetWorker, _ = s.store.GetNode(targetNodeID)
		// 如果用户指定的节点不在线，自动重新回退到容量最低的在线节点
		if targetWorker == nil || targetWorker.Role != "worker" || targetWorker.Status != "online" {
			log.Printf("[Manager] 用户指定的业务节点不可用，自动重定向至已用容量最低的在线业务节点")
			targetWorker = s.scheduler.PickLowestUsageWorker(header.Size)
		}
	}

	// 若无可用的在线业务节点，直接拒绝并报错，避免占用管理节点系统盘
	if targetWorker == nil || targetWorker.Role != "worker" || targetWorker.Status != "online" {
		http.Error(w, "当前无可用业务存储节点（无在线业务节点或业务节点剩余磁盘容量不足），日志只能保存在业务存储节点上，禁止存入管理节点系统盘以防占满系统盘。请先接入或启动业务存储节点。", http.StatusBadRequest)
		return
	}

	// 直接存储并分发至选定的业务节点存储硬盘
	ruleList, _ := s.store.ListRules()
	log.Printf("[Manager] 用户 %s 选定将日志包 %s 存放在业务节点 %s (%s:%d)",
		currentUser.Username, header.Filename, targetWorker.Name, targetWorker.IP, targetWorker.Port)

	wRes, err := s.forwardUploadToWorker(targetWorker, file, header.Filename, archiveID, currentUser.Username, currentUser.ID, ruleList)
	if err != nil {
		http.Error(w, fmt.Sprintf("上传至目标业务节点存储失败: %v", err), http.StatusInternalServerError)
		return
	}

	status := "ready"
	if wRes.Status != "" {
		status = wRes.Status
	}

	archive := &model.LogArchive{
		ID:              archiveID,
		UserID:          currentUser.ID,
		Username:        currentUser.Username,
		Filename:        header.Filename,
		Size:            header.Size,
		Format:          detectArchiveFormat(header.Filename),
		Status:          status,
		FileCount:       len(wRes.Files),
		TotalLines:      wRes.TotalLines,
		ExtractPath:     wRes.ExtractPath,
		StorageNodeID:   targetWorker.ID,
		StorageNodeName: targetWorker.Name,
		StorageNodeIP:   targetWorker.IP,
		StorageNodePort: targetWorker.Port,
		AssignedWorker:  targetWorker.Name,
		Tags:            tags,
		Remark:          strings.TrimSpace(remark),
		UploadTime:      time.Now(),
		FinishTime:      time.Now(),
	}

	_ = s.store.SaveArchive(archive)
	if wRes.Report != nil {
		_ = s.store.SaveReport(wRes.Report)
	}

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

	// 获取内部子压缩包/独立子节点列表 (用于同包内基准差分比对)
	if len(parts) == 2 && parts[1] == "sub-archives" {
		if archive.ExtractPath == "" {
			http.Error(w, "日志包解包尚未完成，请稍后重试", http.StatusBadRequest)
			return
		}
		subs, err := DetectSubArchives(archive.ExtractPath)
		if err != nil {
			subs = []model.SubArchiveItem{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(subs)
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
				archive.StorageNodeIP, archive.StorageNodePort, archive.ID, url.QueryEscape(archive.ExtractPath))
			resp, err := s.httpClient.Get(url)
			if err == nil {
				if resp.StatusCode == http.StatusOK {
					var remoteFiles []*model.LogFileItem
					if err := json.NewDecoder(resp.Body).Decode(&remoteFiles); err == nil {
						resp.Body.Close()
						filtered := make([]*model.LogFileItem, 0, len(remoteFiles))
						for _, f := range remoteFiles {
							if !model.IsInternalIndexFile(f.RelativePath) {
								filtered = append(filtered, f)
							}
						}
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(filtered)
						return
					}
				}
				resp.Body.Close()
			}
		}

		// 本地读取
		var files []*model.LogFileItem
		_ = filepath.Walk(archive.ExtractPath, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(archive.ExtractPath, p)
			if rel == "." || model.IsInternalIndexFile(rel) {
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

	// 按需懒加载目录节点 (单层直接子项读取，彻底避免一次性遍历海量文件导致网络和页面假死)
	if len(parts) == 2 && parts[1] == "tree-nodes" {
		if archive.ExtractPath == "" {
			http.Error(w, "日志仍在解包分析中，请稍后刷新", http.StatusBadRequest)
			return
		}

		subDir := r.URL.Query().Get("dir")

		// 若日志存放在远程业务节点，代理从该业务节点获取
		if archive.StorageNodeIP != "" && archive.StorageNodePort > 0 && archive.StorageNodeID != "manager_primary" {
			reqURL := fmt.Sprintf("http://%s:%d/api/worker/storage/tree-nodes?archive_id=%s&extract_path=%s&dir=%s",
				archive.StorageNodeIP, archive.StorageNodePort, archive.ID, url.QueryEscape(archive.ExtractPath), url.QueryEscape(subDir))
			resp, err := s.httpClient.Get(reqURL)
			if err == nil {
				if resp.StatusCode == http.StatusOK {
					var remoteNodes []*model.TreeNodeItem
					if err := json.NewDecoder(resp.Body).Decode(&remoteNodes); err == nil {
						resp.Body.Close()
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(remoteNodes)
						return
					}
				}
				resp.Body.Close()
			}
		}

		targetDir := archive.ExtractPath
		if subDir != "" {
			targetDir = filepath.Join(archive.ExtractPath, subDir)
		}
		if !model.IsSafeSubpath(archive.ExtractPath, targetDir) {
			http.Error(w, "非法访问路径", http.StatusForbidden)
			return
		}

		cleanTarget := filepath.Clean(targetDir)
		entries, err := os.ReadDir(cleanTarget)
		if err != nil {
			if os.IsNotExist(err) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode([]*model.TreeNodeItem{})
				return
			}
			http.Error(w, fmt.Sprintf("无法读取目录: %v", err), http.StatusInternalServerError)
			return
		}

		nodes := make([]*model.TreeNodeItem, 0, len(entries))
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}
			fullChildPath := filepath.Join(cleanTarget, name)
			rel, rErr := filepath.Rel(archive.ExtractPath, fullChildPath)
			if rErr != nil || model.IsInternalIndexFile(rel) {
				continue
			}

			info, iErr := entry.Info()
			if iErr != nil {
				continue
			}

			isDir := entry.IsDir()
			childCount := 0
			if isDir {
				if subEntries, err := os.ReadDir(fullChildPath); err == nil {
					for _, se := range subEntries {
						if !strings.HasPrefix(se.Name(), ".") && !model.IsInternalIndexFile(se.Name()) {
							childCount++
						}
					}
				}
			}

			nodes = append(nodes, &model.TreeNodeItem{
				ArchiveID:    archive.ID,
				Name:         name,
				RelativePath: rel,
				Size:         info.Size(),
				ModTime:      info.ModTime(),
				IsDirectory:  isDir,
				ChildCount:   childCount,
			})
		}

		sort.Slice(nodes, func(i, j int) bool {
			if nodes[i].IsDirectory != nodes[j].IsDirectory {
				return nodes[i].IsDirectory
			}
			return nodes[i].Name < nodes[j].Name
		})

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(nodes)
		return
	}

	// 查看具体文件内容 (带分页行支持)
	if len(parts) == 2 && parts[1] == "file-content" {
		relPath := r.URL.Query().Get("path")
		if relPath == "" {
			http.Error(w, "缺少文件相对路径参数 path", http.StatusBadRequest)
			return
		}
		if model.IsInternalIndexFile(relPath) {
			http.Error(w, "系统内部索引文件禁止直接浏览", http.StatusForbidden)
			return
		}

		// 若日志存放在远程业务节点，代理从该节点获取
		if archive.StorageNodeIP != "" && archive.StorageNodePort > 0 && archive.StorageNodeID != "manager_primary" {
			url := fmt.Sprintf("http://%s:%d/api/worker/storage/file-content?path=%s&extract_path=%s&start_line=%s&limit=%s",
				archive.StorageNodeIP, archive.StorageNodePort, url.QueryEscape(relPath), url.QueryEscape(archive.ExtractPath),
				r.URL.Query().Get("start_line"), r.URL.Query().Get("limit"))
			resp, err := s.httpClient.Get(url)
			if err == nil {
				if resp.StatusCode == http.StatusOK {
					defer resp.Body.Close()
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.Copy(w, resp.Body)
					return
				}
				resp.Body.Close()
			}
		}

		fullPath := filepath.Join(archive.ExtractPath, relPath)
		if !model.IsSafeSubpath(archive.ExtractPath, fullPath) {
			http.Error(w, "非法文件路径", http.StatusForbidden)
			return
		}

		startLine, _ := strconv.Atoi(r.URL.Query().Get("start_line"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

		lines, hasMore, totalLines, err := worker.ReadFileLinesWithIndex(fullPath, startLine, limit)
		if err != nil {
			http.Error(w, fmt.Sprintf("无法读取文件: %v", err), http.StatusInternalServerError)
			return
		}

		respMap := map[string]interface{}{
			"file_path":  relPath,
			"start_line": startLine,
			"line_count": len(lines),
			"lines":      lines,
			"has_more":   hasMore,
		}
		if totalLines > 0 {
			respMap["total_lines"] = totalLines
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(respMap)
		return
	}

	// 下载指定的日志文件
	if len(parts) == 2 && (parts[1] == "download-file" || parts[1] == "file-download") {
		relPath := r.URL.Query().Get("path")
		if relPath == "" {
			http.Error(w, "缺少文件相对路径参数 path", http.StatusBadRequest)
			return
		}
		if model.IsInternalIndexFile(relPath) {
			http.Error(w, "系统内部索引文件禁止下载", http.StatusForbidden)
			return
		}

		// 若日志存放在远程业务节点，代理从该业务节点下载
		if archive.StorageNodeIP != "" && archive.StorageNodePort > 0 && archive.StorageNodeID != "manager_primary" {
			remoteURL := fmt.Sprintf("http://%s:%d/api/worker/storage/download-file?path=%s&extract_path=%s",
				archive.StorageNodeIP, archive.StorageNodePort, url.QueryEscape(relPath), url.QueryEscape(archive.ExtractPath))
			resp, err := s.httpClient.Get(remoteURL)
			if err == nil {
				if resp.StatusCode == http.StatusOK {
					defer resp.Body.Close()
					fileName := filepath.Base(relPath)
					if cd := resp.Header.Get("Content-Disposition"); cd != "" {
						w.Header().Set("Content-Disposition", cd)
					} else {
						w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, fileName))
					}
					if ct := resp.Header.Get("Content-Type"); ct != "" {
						w.Header().Set("Content-Type", ct)
					} else {
						w.Header().Set("Content-Type", "application/octet-stream")
					}
					if cl := resp.Header.Get("Content-Length"); cl != "" {
						w.Header().Set("Content-Length", cl)
					}
					_, _ = io.Copy(w, resp.Body)
					return
				}
				resp.Body.Close()
			}
		}

		// 本地读取解压目录下的指定日志文件
		fullPath := filepath.Join(archive.ExtractPath, relPath)
		if !model.IsSafeSubpath(archive.ExtractPath, fullPath) {
			http.Error(w, "非法文件路径", http.StatusForbidden)
			return
		}

		f, err := os.Open(fullPath)
		if err != nil {
			http.Error(w, "无法读取文件: "+err.Error(), http.StatusNotFound)
			return
		}
		defer f.Close()

		info, err := f.Stat()
		if err != nil || info.IsDir() {
			http.Error(w, "目标不是有效文件", http.StatusBadRequest)
			return
		}

		fileName := filepath.Base(fullPath)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, fileName))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		http.ServeContent(w, r, fileName, info.ModTime(), f)
		return
	}

	// 下载原始日志归档压缩包
	if len(parts) == 2 && (parts[1] == "download" || parts[1] == "download-archive") {
		// 若日志存放在远程业务节点，代理从该业务节点下载
		if archive.StorageNodeIP != "" && archive.StorageNodePort > 0 && archive.StorageNodeID != "manager_primary" {
			remoteURL := fmt.Sprintf("http://%s:%d/api/worker/storage/download-archive?username=%s&filename=%s",
				archive.StorageNodeIP, archive.StorageNodePort, url.QueryEscape(archive.Username), url.QueryEscape(archive.Filename))
			resp, err := http.Get(remoteURL)
			if err == nil {
				if resp.StatusCode == http.StatusOK {
					defer resp.Body.Close()
					if cd := resp.Header.Get("Content-Disposition"); cd != "" {
						w.Header().Set("Content-Disposition", cd)
					} else {
						w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, archive.Filename))
					}
					if ct := resp.Header.Get("Content-Type"); ct != "" {
						w.Header().Set("Content-Type", ct)
					} else {
						w.Header().Set("Content-Type", "application/octet-stream")
					}
					if cl := resp.Header.Get("Content-Length"); cl != "" {
						w.Header().Set("Content-Length", cl)
					}
					_, _ = io.Copy(w, resp.Body)
					return
				}
				resp.Body.Close()
			}
		}

		archiveDir := s.store.GetUserArchiveDir(archive.Username)
		fullPath := filepath.Join(archiveDir, archive.Filename)
		f, err := os.Open(fullPath)
		if err != nil {
			http.Error(w, "归档压缩包不存在或无法打开: "+err.Error(), http.StatusNotFound)
			return
		}
		defer f.Close()

		info, err := f.Stat()
		if err != nil || info.IsDir() {
			http.Error(w, "无法读取归档文件", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, archive.Filename))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		http.ServeContent(w, r, archive.Filename, info.ModTime(), f)
		return
	}

	// 更新归档包标签与备注信息
	if len(parts) == 1 && (r.Method == http.MethodPut || r.Method == http.MethodPost) {
		var req struct {
			Tags   interface{} `json:"tags"`
			Remark string      `json:"remark"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch t := req.Tags.(type) {
		case string:
			archive.Tags = parseTags(t)
		case []interface{}:
			var rawList []string
			for _, item := range t {
				if s, ok := item.(string); ok {
					rawList = append(rawList, s)
				}
			}
			data, _ := json.Marshal(rawList)
			archive.Tags = parseTags(string(data))
		}

		if len(archive.Tags) == 0 {
			http.Error(w, "日志归档标签不能为空，标签为归档唯一标识", http.StatusBadRequest)
			return
		}
		if dupTag, matchedArc := s.findDuplicateTag(archive.Tags, archive.ID); dupTag != "" {
			http.Error(w, fmt.Sprintf("标签 “%s” 已被归档包 [%s] 占用，归档标签必须保持全系统唯一", dupTag, matchedArc.Filename), http.StatusBadRequest)
			return
		}

		archive.Remark = strings.TrimSpace(req.Remark)
		if err := s.store.SaveArchive(archive); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(archive)
		return
	}

	// 重新触发解包与分析调度
	if len(parts) == 2 && parts[1] == "retry" && (r.Method == http.MethodPost || r.Method == http.MethodPut) {
		archive.Status = "extracting"
		archive.ErrorMsg = ""
		_ = s.store.SaveArchive(archive)
		s.scheduler.DispatchAnalyzeTask(archive)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "ok",
			"message": "已重新触发解包与分析调度",
		})
		return
	}

	// 删除归档包
	if r.Method == http.MethodDelete {
		if !isAdmin && archive.Username != currentUser.Username {
			http.Error(w, "无权删除该日志归档包", http.StatusForbidden)
			return
		}
		if archive.Pinned {
			http.Error(w, "该日志包处于保护锁定状态，请先解除锁定后再执行删除", http.StatusBadRequest)
			return
		}

		if err := s.DeleteArchiveStorageAndMeta(archive); err != nil {
			http.Error(w, fmt.Sprintf("清理失败: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "ok",
			"message": "日志包及占用磁盘空间已彻底清理",
		})
		return
	}

	// 单包元数据
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(archive)
}

// ================= 日志检索中心 =================

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	defer func() {
		s.RecordSearchMetrics(time.Since(startTime))
	}()

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
		req, reqErr := http.NewRequestWithContext(r.Context(), "POST", url, bytes.NewReader(data))
		if reqErr == nil {
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				if resp.StatusCode == http.StatusOK {
					defer resp.Body.Close()
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.Copy(w, resp.Body)
					return
				}
				resp.Body.Close()
			}
		}
	}

	// 本地执行检索
	resp, err := worker.SearchLogsContext(r.Context(), archive.ExtractPath, &q)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
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
		rule.FilePathPattern = req.FilePathPattern
		rule.Description = req.Description
		rule.Suggestion = req.Suggestion
		rule.Enabled = req.Enabled
		rule.UpdatedAt = time.Now()
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
	case strings.HasSuffix(lower, ".7z"):
		return "7z"
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

	// 1. 获取当前最新 TxID
	txID, err := s.store.CurrentTxID()
	if err == nil {
		etag := fmt.Sprintf(`W/"tx-%d"`, txID)
		w.Header().Set("ETag", etag)
		w.Header().Set("X-DB-TxID", strconv.Itoa(txID))

		// 检查条件请求头 If-None-Match 或 X-Last-TxID
		clientETag := r.Header.Get("If-None-Match")
		clientTxIDStr := r.Header.Get("X-Last-TxID")
		if clientETag == etag || (clientTxIDStr != "" && clientTxIDStr == strconv.Itoa(txID)) {
			// 数据无任何变动，直接返回 304 Not Modified，节省全量网络流与备机 DB 重载开销
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=\"analyzer.db.snapshot\"")

	written, err := s.store.ExportSnapshot(w)
	if err != nil {
		log.Printf("[HA Snapshot] 导出快照流失败: %v (已写入 %d 字节)", err, written)
	} else {
		log.Printf("[HA Snapshot] 成功向备节点输出数据库快照流 (大小: %d 字节, TxID: %d)", written, txID)
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

// handleArchivePin 切换日志包锁定保护状态 (锁定后禁止自动生命周期清理和防误删)
func (s *Server) handleArchivePin(w http.ResponseWriter, r *http.Request) {
	currentUser, err := s.authenticate(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ArchiveID string `json:"archive_id"`
		Pinned    bool   `json:"pinned"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	archive, err := s.store.GetArchive(req.ArchiveID)
	if err != nil {
		http.Error(w, "归档包不存在", http.StatusNotFound)
		return
	}
	if currentUser.Role != model.RoleAdmin && archive.Username != currentUser.Username {
		http.Error(w, "无权修改该日志包的保护状态", http.StatusForbidden)
		return
	}

	updated, err := s.store.SetArchivePinned(req.ArchiveID, req.Pinned)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(updated)
}

// handleRetentionSettings 读取与保存日志生命周期与磁盘高水位自愈配置
func (s *Server) handleRetentionSettings(w http.ResponseWriter, r *http.Request) {
	currentUser, err := s.authenticate(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	if r.Method == http.MethodGet {
		cfg, err := s.store.GetRetentionConfig()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cfg)
		return
	}

	if r.Method == http.MethodPut || r.Method == http.MethodPost {
		if currentUser.Role != model.RoleAdmin {
			http.Error(w, "仅管理员可修改容量与生命周期配置", http.StatusForbidden)
			return
		}
		var newCfg model.RetentionConfig
		if err := json.NewDecoder(r.Body).Decode(&newCfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if newCfg.RetentionDays < 0 {
			newCfg.RetentionDays = 0
		}
		if newCfg.HighWatermarkPercent <= 0 || newCfg.HighWatermarkPercent > 100 {
			newCfg.HighWatermarkPercent = 85
		}
		if newCfg.EmergencyWatermarkPercent <= 0 || newCfg.EmergencyWatermarkPercent > 100 {
			newCfg.EmergencyWatermarkPercent = 92
		}
		if newCfg.TargetWatermarkPercent <= 0 || newCfg.TargetWatermarkPercent >= newCfg.EmergencyWatermarkPercent {
			newCfg.TargetWatermarkPercent = 75
		}

		var cleanTags []string
		tagSeen := make(map[string]bool)
		for _, t := range newCfg.ExemptTags {
			t = strings.TrimSpace(t)
			if t != "" && !tagSeen[t] {
				tagSeen[t] = true
				cleanTags = append(cleanTags, t)
			}
		}
		newCfg.ExemptTags = cleanTags

		if err := s.store.SaveRetentionConfig(&newCfg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(newCfg)
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// handleSystemBackup 在线无锁热快照导出备份
func (s *Server) handleSystemBackup(w http.ResponseWriter, r *http.Request) {
	// 支持从 Authorization Header 或 URL query ?token=... 鉴权
	token := r.URL.Query().Get("token")
	var currentUser *model.User
	var err error
	if token != "" {
		if u, ok := s.sessions.Load(token); ok {
			currentUser, err = s.store.GetUserByUsername(u.(string))
		}
	}
	if currentUser == nil {
		currentUser, err = s.authenticate(r)
	}
	if err != nil || currentUser == nil || currentUser.Role != model.RoleAdmin {
		http.Error(w, "仅系统管理员可执行数据库热备份导出", http.StatusUnauthorized)
		return
	}

	filename := fmt.Sprintf("analyzer-backup-%s.db", time.Now().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))

	if err := s.store.BackupSnapshot(w); err != nil {
		log.Printf("[Backup] 导出数据库热快照异常: %v", err)
		http.Error(w, fmt.Sprintf("备份失败: %v", err), http.StatusInternalServerError)
		return
	}
}

// handleNodeMaintenance 设置节点维护模式 (Drain / Maintenance Mode)
func (s *Server) handleNodeMaintenance(w http.ResponseWriter, r *http.Request) {
	currentUser, err := s.authenticate(r)
	if err != nil || currentUser.Role != model.RoleAdmin {
		http.Error(w, "仅管理员可调整节点维护状态", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		NodeID      string `json:"node_id"`
		Maintenance bool   `json:"maintenance"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	node, err := s.store.GetNode(req.NodeID)
	if err != nil || node == nil {
		http.Error(w, "目标节点不存在", http.StatusNotFound)
		return
	}

	if req.Maintenance {
		node.Status = "maintenance"
		log.Printf("[Maintenance] 管理员 %s 将节点 %s (%s:%d) 置为维护模式 (Drain: 暂停新日志调度分配)",
			currentUser.Username, node.Name, node.IP, node.Port)
	} else {
		node.Status = "online"
		log.Printf("[Maintenance] 管理员 %s 将节点 %s (%s:%d) 恢复上线 (恢复新日志调度分配)",
			currentUser.Username, node.Name, node.IP, node.Port)
	}

	if err := s.store.SaveNode(node); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(node)
}

// handleAnalysisDiff 触发双日志包横向基准差分对比
func (s *Server) handleAnalysisDiff(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticate(r); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ArchiveIDA string `json:"archive_id_a"`
		SubPathA   string `json:"sub_path_a"`
		ArchiveIDB string `json:"archive_id_b"`
		SubPathB   string `json:"sub_path_b"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.ArchiveIDA == "" || req.ArchiveIDB == "" {
		http.Error(w, "archive_id_a 和 archive_id_b 均不能为空", http.StatusBadRequest)
		return
	}
	report, err := s.CompareArchiveScopes(req.ArchiveIDA, req.SubPathA, req.ArchiveIDB, req.SubPathB)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(report)
}

// handleAnalysisTemplates 提取指定归档包的通用 Drain 模式聚类模板
func (s *Server) handleAnalysisTemplates(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticate(r); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	archiveID := r.URL.Query().Get("archive_id")
	if archiveID == "" {
		http.Error(w, "缺少 archive_id 参数", http.StatusBadRequest)
		return
	}
	arc, err := s.store.GetArchive(archiveID)
	if err != nil || arc == nil {
		http.Error(w, "归档包不存在", http.StatusNotFound)
		return
	}
	miner := indexer.NewDrainMiner(0.55, 4)
	mineArchiveSamples(arc.ExtractPath, miner, 2000)
	templates := miner.GetTemplates()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(templates)
}

// handleSystemUpgrade 管理员上传新版安装包执行一键平滑升级
func (s *Server) handleSystemUpgrade(w http.ResponseWriter, r *http.Request) {
	currentUser, err := s.authenticate(r)
	if err != nil || currentUser == nil || currentUser.Role != model.RoleAdmin {
		http.Error(w, "仅系统管理员可执行在线平滑升级", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseMultipartForm(300 << 20); err != nil {
		http.Error(w, "解析上传表单失败: "+err.Error(), http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("package")
	if err != nil {
		file, header, err = r.FormFile("file")
	}
	if err != nil {
		http.Error(w, "请提供要升级的安装包文件 (package)", http.StatusBadRequest)
		return
	}
	defer file.Close()

	tempDir, err := os.MkdirTemp("", "upgrade_pkg_*")
	if err != nil {
		http.Error(w, "创建升级临时环境失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	targetBin, err := extractUpgradeBinary(tempDir, header.Filename, file)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		http.Error(w, "升级包校验失败: "+err.Error(), http.StatusBadRequest)
		return
	}

	currentBin := getSystemInstallBinPath()
	binBak := currentBin + ".bak"
	binDir := filepath.Dir(currentBin)
	_ = os.MkdirAll(binDir, 0755)
	tmpBin := filepath.Join(binDir, fmt.Sprintf(".dist-log-analyzer.upgrade.%d", time.Now().UnixNano()))

	// 先将目标二进制写入同目录临时文件
	if err := copyFile(targetBin, tmpBin); err != nil {
		_ = os.Remove(tmpBin)
		_ = os.RemoveAll(tempDir)
		http.Error(w, "写入目标程序目录失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = os.Chmod(tmpBin, 0755)

	// 备份当前旧版本至 .bak (通过 rename 或 copy)
	if _, err := os.Stat(currentBin); err == nil {
		_ = os.Rename(currentBin, binBak)
	}

	// 原子覆盖为新版本 (同文件系统 rename 彻底避免 Linux text file busy 问题)
	if err := os.Rename(tmpBin, currentBin); err != nil {
		if _, statErr := os.Stat(binBak); statErr == nil {
			_ = os.Rename(binBak, currentBin)
		}
		_ = os.Remove(tmpBin)
		_ = os.RemoveAll(tempDir)
		http.Error(w, "原子替换目标程序文件失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = os.Chmod(currentBin, 0755)

	log.Printf("[System Upgrade] 管理员 %s 成功上传并更新了程序版本 (来源: %s, 目标: %s)，准备重启服务生效",
		currentUser.Username, header.Filename, currentBin)

	// 立即向浏览器客户端返回成功状态，告知客户端已进入升级与重连等待流程
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "upgrading",
		"message":     "新版本安装包校验与替换完成，系统正在平滑重启生效...",
		"package":     header.Filename,
		"backup_file": binBak,
	})

	// 延迟 800ms 触发滚动重启，确保当前 HTTP 响应已完整交付客户端网络栈
	go func() {
		time.Sleep(800 * time.Millisecond)
		_ = os.RemoveAll(tempDir)
		if _, err := exec.LookPath("systemctl"); err == nil {
			out, _ := exec.Command("systemctl", "list-units", "--type=service", "--state=running", "--no-pager").Output()
			restartedAny := false
			for _, line := range strings.Split(string(out), "\n") {
				if strings.Contains(line, "dist-log-manager") || strings.Contains(line, "dist-log-worker") {
					parts := strings.Fields(line)
					if len(parts) > 0 {
						_ = exec.Command("systemctl", "restart", parts[0]).Run()
						restartedAny = true
					}
				}
			}
			if !restartedAny {
				_ = exec.Command("systemctl", "restart", "dist-log-manager").Run()
			}
		}
	}()
}

func extractUpgradeBinary(tempDir, filename string, reader io.Reader) (string, error) {
	pkgPath := filepath.Join(tempDir, filename)
	outFile, err := os.OpenFile(pkgPath, os.O_CREATE|os.O_WRONLY, 0755)
	if err != nil {
		return "", fmt.Errorf("创建升级临时文件失败: %v", err)
	}
	if _, err := io.Copy(outFile, reader); err != nil {
		_ = outFile.Close()
		return "", fmt.Errorf("写入升级包失败: %v", err)
	}
	_ = outFile.Close()

	var targetBin string
	if strings.HasSuffix(filename, ".tar.gz") || strings.HasSuffix(filename, ".tgz") {
		cmd := exec.Command("tar", "-zxvf", pkgPath, "-C", tempDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("解包安装包失败: %v, 输出: %s", err, string(out))
		}
		// 递归遍历解压目录，定位 dist-log-analyzer 二进制程序
		_ = filepath.WalkDir(tempDir, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr == nil && !d.IsDir() && d.Name() == "dist-log-analyzer" {
				targetBin = path
			}
			return nil
		})
	} else {
		targetBin = pkgPath
	}

	if targetBin == "" {
		return "", fmt.Errorf("升级包内未找到有效的主程序 (dist-log-analyzer)")
	}

	_ = os.Chmod(targetBin, 0755)

	// 执行架构兼容性与执行探针自检
	testCmd := exec.Command(targetBin, "--help")
	if out, err := testCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("新版本程序架构兼容性校验失败: %v, 输出: %s", err, string(out))
	}

	return targetBin, nil
}

func getSystemInstallBinPath() string {
	candidates := []string{
		"/opt/dist-log-analyzer/bin/dist-log-analyzer",
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".dist-log-analyzer", "bin", "dist-log-analyzer"))
	}
	if currentExec, err := os.Executable(); err == nil {
		candidates = append([]string{currentExec}, candidates...)
	}

	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "/opt/dist-log-analyzer/bin/dist-log-analyzer"
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	_ = os.MkdirAll(filepath.Dir(dst), 0755)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

