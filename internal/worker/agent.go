package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"dist-log-analyzer/internal/config"
	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/rules"
)

// Agent Worker 计算节点守护服务
type Agent struct {
	cfg              *config.Config
	server           *http.Server
	nodeID           string
	activeTask       int
	mu               sync.Mutex
	alarmCooldown    map[string]time.Time
	alarmMu          sync.Mutex
	isDecommissioned bool
}

func NewAgent(cfg *config.Config) *Agent {
	nodeID := fmt.Sprintf("worker_%s_%d", cfg.NodeName, cfg.Port)
	return &Agent{
		cfg:           cfg,
		nodeID:        nodeID,
		alarmCooldown: make(map[string]time.Time),
	}
}

func (a *Agent) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/worker/health", a.handleHealth)
	mux.HandleFunc("/api/worker/tasks/analyze", a.handleAnalyzeTask)
	mux.HandleFunc("/api/worker/tasks/search", a.handleSearchTask)

	// 新增：定向存储与日志查看专用接口
	mux.HandleFunc("/api/worker/storage/upload", a.handleStorageUpload)
	mux.HandleFunc("/api/worker/storage/file-content", a.handleStorageFileContent)
	mux.HandleFunc("/api/worker/storage/download-file", a.handleStorageDownloadFile)
	mux.HandleFunc("/api/worker/storage/download-archive", a.handleStorageDownloadArchive)
	mux.HandleFunc("/api/worker/storage/clean-archive", a.handleStorageCleanArchive)
	mux.HandleFunc("/api/worker/storage/files", a.handleStorageFiles)
	mux.HandleFunc("/api/worker/decommission", a.handleDecommission)

	addr := fmt.Sprintf("%s:%d", a.cfg.ListenHost, a.cfg.Port)
	a.server = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	log.Printf("[Worker Agent] 启动监听: %s (NodeID: %s, 磁盘存储目录: %s)", addr, a.nodeID, a.cfg.DataDir)

	// 后台定期向 Manager 注册并发送心跳
	go a.heartbeatLoop(ctx)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = a.server.Shutdown(shutdownCtx)
	}()

	if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (a *Agent) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   "ok",
		"node_id":  a.nodeID,
		"role":     "worker",
		"data_dir": a.cfg.DataDir,
		"time":     time.Now(),
	})
}

// handleStorageUpload 接收定向上传的日志压缩包并在本机硬盘上落地、解包并完成故障匹配
func (a *Agent) handleStorageUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 限制单次上传 2GB
	if err := r.ParseMultipartForm(2048 << 20); err != nil {
		http.Error(w, fmt.Sprintf("解析上传文件失败: %v", err), http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "未找到上传文件", http.StatusBadRequest)
		return
	}
	defer file.Close()

	archiveID := r.FormValue("archive_id")
	username := r.FormValue("username")
	userID := r.FormValue("user_id")
	rulesJSON := r.FormValue("rules")

	if archiveID == "" || username == "" {
		http.Error(w, "缺少必要的 archive_id 或 username 参数", http.StatusBadRequest)
		return
	}

	var ruleList []*model.Rule
	if rulesJSON != "" {
		_ = json.Unmarshal([]byte(rulesJSON), &ruleList)
	}

	a.mu.Lock()
	a.activeTask++
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		a.activeTask--
		a.mu.Unlock()
	}()

	// 1. 保存到本机已格式化/挂载的数据目录
	archiveDir := filepath.Join(a.cfg.DataDir, "users", username, "archives")
	extractDir := filepath.Join(a.cfg.DataDir, "users", username, "extracted", archiveID)
	_ = os.MkdirAll(archiveDir, 0755)

	destPath := filepath.Join(archiveDir, header.Filename)
	destFile, err := os.Create(destPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("创建本地目标存储文件失败: %v", err), http.StatusInternalServerError)
		return
	}

	bw := bufio.NewWriterSize(destFile, 2*1024*1024)
	bufPtr := extractBufPool.Get().(*[]byte)
	_, copyErr := io.CopyBuffer(bw, file, *bufPtr)
	extractBufPool.Put(bufPtr)
	_ = bw.Flush()
	destFile.Close()

	if copyErr != nil {
		a.ReportAlarm(model.AlarmTypeDiskReadOnly, model.SeverityCritical, "日志写入磁盘发生 I/O 异常", fmt.Sprintf("节点 %s 写入文件 %s 失败: %v", a.cfg.NodeName, destPath, copyErr))
		http.Error(w, fmt.Sprintf("写入文件失败: %v", copyErr), http.StatusInternalServerError)
		return
	}

	log.Printf("[Worker Storage] 日志包已持久化存放在本节点硬盘: %s，立即响应管理节点并后台启动异步解包与诊断", destPath)

	// 2. 后台异步执行大文件解包、行数统计与规则诊断
	go a.asyncProcessArchive(archiveID, userID, header.Filename, destPath, extractDir, ruleList)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":       "extracting",
		"archive_id":   archiveID,
		"extract_path": extractDir,
	})
}

// asyncProcessArchive 业务节点后台异步解包与规则匹配诊断
func (a *Agent) asyncProcessArchive(archiveID, userID, filename, destPath, extractDir string, ruleList []*model.Rule) {
	a.mu.Lock()
	a.activeTask++
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		a.activeTask--
		a.mu.Unlock()
	}()

	log.Printf("[Worker Storage] 后台开始异步解包: %s -> %s", filename, extractDir)
	files, totalLines, err := ExtractArchive(destPath, extractDir)
	if err != nil {
		errMsg := err.Error()
		if strings.Contains(strings.ToLower(errMsg), "no space left on device") {
			errMsg = fmt.Sprintf("存储磁盘空间不足 (no space left on device): %v", err)
		}
		a.ReportAlarm(model.AlarmTypeTaskFailed, model.SeverityWarning, "日志归档解压缩失败", fmt.Sprintf("节点 %s 解压缩文件 %s 发生异常: %v", a.cfg.NodeName, filename, errMsg))
		a.reportArchiveCallback(&model.ArchiveCallbackReq{
			ArchiveID:   archiveID,
			Status:      "failed",
			ErrorMsg:    fmt.Sprintf("解包失败: %v", errMsg),
			ExtractPath: extractDir,
		})
		return
	}

	log.Printf("[Worker Storage] 异步解包完成: %s (总行数: %d, 文件数: %d)，开始规则匹配诊断...", filename, totalLines, len(files))

	// 异步预热构建稀疏行号索引与分块布隆索引，彻底消除后续全文件随机跨行翻页与初次搜索时的索引等待
	go func(targetFiles []*model.LogFileItem, baseDir string) {
		for _, f := range targetFiles {
			if f.IsDirectory {
				continue
			}
			fullPath := filepath.Join(baseDir, f.RelativePath)
			_, _ = GetOrBuildLineIndex(fullPath)
			_, _ = GetOrBuildBloomIndex(fullPath)
		}
	}(files, extractDir)
	engine := rules.NewEngine(ruleList)
	report, err := engine.DiagnoseDirectory(archiveID, userID, filename, extractDir)
	if err != nil {
		log.Printf("[Worker Storage] 规则诊断发生异常: %v", err)
	}

	a.reportArchiveCallback(&model.ArchiveCallbackReq{
		ArchiveID:   archiveID,
		Status:      "ready",
		Files:       files,
		FileCount:   len(files),
		TotalLines:  totalLines,
		ExtractPath: extractDir,
		Report:      report,
	})
	log.Printf("[Worker Storage] 日志包 %s 异步解包与诊断全部完成并已上报 Manager", filename)
}

// reportArchiveCallback 向管理节点上报异步解包与诊断完成状态
func (a *Agent) reportArchiveCallback(reqPayload *model.ArchiveCallbackReq) {
	data, err := json.Marshal(reqPayload)
	if err != nil {
		log.Printf("[Worker Callback] 序列化回调数据失败: %v", err)
		return
	}

	mgrURLs := strings.Split(a.cfg.ManagerURL, ",")
	client := &http.Client{Timeout: 30 * time.Second}

	for _, rawURL := range mgrURLs {
		cleanURL := strings.TrimSpace(rawURL)
		if cleanURL == "" {
			continue
		}
		targetURL := fmt.Sprintf("%s/api/cluster/archive-callback", strings.TrimRight(cleanURL, "/"))
		req, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(data))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Cluster-Token", a.cfg.ClusterToken)

		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				log.Printf("[Worker Callback] 成功向管理节点 (%s) 上报归档包 %s 状态: %s", cleanURL, reqPayload.ArchiveID, reqPayload.Status)
				return
			}
		}
	}
	log.Printf("[Worker Callback] 警告: 未能向任何 Manager 成功上报归档包 %s 状态", reqPayload.ArchiveID)
}

var contentBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 1024*1024)
		return &b
	},
}

// handleStorageFileContent 读取保存在本节点硬盘上的文件内容
func (a *Agent) handleStorageFileContent(w http.ResponseWriter, r *http.Request) {
	relPath := r.URL.Query().Get("path")
	extractPath := r.URL.Query().Get("extract_path")
	if relPath == "" || extractPath == "" {
		http.Error(w, "缺少必要参数 path 或 extract_path", http.StatusBadRequest)
		return
	}
	if model.IsInternalIndexFile(relPath) {
		http.Error(w, "系统内部索引文件禁止直接浏览", http.StatusForbidden)
		return
	}

	fullPath := filepath.Join(extractPath, relPath)
	if !strings.HasPrefix(filepath.Clean(fullPath), filepath.Clean(extractPath)) {
		http.Error(w, "非法访问路径", http.StatusForbidden)
		return
	}

	startLine, _ := strconv.Atoi(r.URL.Query().Get("start_line"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	lines, hasMore, totalLines, err := ReadFileLinesWithIndex(fullPath, startLine, limit)
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
}

// handleStorageDownloadFile 直接下载业务节点硬盘上解压目录中的指定日志文件
func (a *Agent) handleStorageDownloadFile(w http.ResponseWriter, r *http.Request) {
	relPath := r.URL.Query().Get("path")
	extractPath := r.URL.Query().Get("extract_path")
	if relPath == "" || extractPath == "" {
		http.Error(w, "缺少必要参数 path 或 extract_path", http.StatusBadRequest)
		return
	}
	if model.IsInternalIndexFile(relPath) {
		http.Error(w, "系统内部索引文件禁止下载", http.StatusForbidden)
		return
	}

	cleanExtract := filepath.Clean(extractPath)
	fullPath := filepath.Join(cleanExtract, relPath)
	if !strings.HasPrefix(filepath.Clean(fullPath), cleanExtract) {
		http.Error(w, "非法访问路径", http.StatusForbidden)
		return
	}

	f, err := os.Open(fullPath)
	if err != nil {
		http.Error(w, "无法读取指定文件: "+err.Error(), http.StatusNotFound)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.Error(w, "目标不是有效的文件", http.StatusBadRequest)
		return
	}

	fileName := filepath.Base(fullPath)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, fileName))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	http.ServeContent(w, r, fileName, info.ModTime(), f)
}

// handleStorageDownloadArchive 下载业务节点硬盘上暂存的原日志归档压缩包
func (a *Agent) handleStorageDownloadArchive(w http.ResponseWriter, r *http.Request) {
	username := r.URL.Query().Get("username")
	filename := r.URL.Query().Get("filename")
	if username == "" || filename == "" {
		http.Error(w, "缺少必要参数 username 或 filename", http.StatusBadRequest)
		return
	}

	archiveDir := filepath.Join(a.cfg.DataDir, "users", username, "archives")
	fullPath := filepath.Join(archiveDir, filename)
	if !strings.HasPrefix(filepath.Clean(fullPath), filepath.Clean(archiveDir)) {
		http.Error(w, "非法访问路径", http.StatusForbidden)
		return
	}

	f, err := os.Open(fullPath)
	if err != nil {
		http.Error(w, "原始归档文件不存在: "+err.Error(), http.StatusNotFound)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.Error(w, "目标不是有效的文件", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	http.ServeContent(w, r, filename, info.ModTime(), f)
}

// handleStorageCleanArchive 清理业务节点硬盘上的日志归档文件及解压目录 (用于节点移除时的彻底清理或迁移后的空间释放)
func (a *Agent) handleStorageCleanArchive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	username := r.URL.Query().Get("username")
	archiveID := r.URL.Query().Get("archive_id")
	filename := r.URL.Query().Get("filename")
	all := r.URL.Query().Get("all") == "true"

	if all {
		// 清理整个数据目录下的 users
		usersDir := filepath.Join(a.cfg.DataDir, "users")
		_ = os.RemoveAll(usersDir)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "message": "all user data cleared"})
		return
	}

	if username != "" {
		if filename != "" {
			archivePath := filepath.Join(a.cfg.DataDir, "users", username, "archives", filename)
			_ = os.Remove(archivePath)
		}
		if archiveID != "" {
			extractDir := filepath.Join(a.cfg.DataDir, "users", username, "extracted", archiveID)
			_ = os.RemoveAll(extractDir)
		}
	}

	// 支持直接指定 extract_path 清理
	extractPath := r.URL.Query().Get("extract_path")
	if extractPath != "" {
		cleanExtract := filepath.Clean(extractPath)
		_ = os.RemoveAll(cleanExtract)
		if filename != "" {
			siblingArchive := filepath.Join(filepath.Dir(filepath.Dir(cleanExtract)), "archives", filename)
			_ = os.Remove(siblingArchive)
		}
	}

	log.Printf("[Worker Storage] 磁盘清理执行完毕: archive_id=%s, filename=%s, username=%s", archiveID, filename, username)

	// 异步立即触发一次心跳上报，更新 Manager 侧该节点的最新磁盘用量统计
	go a.sendHeartbeat()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "message": "磁盘空间已释放"})
}

// handleStorageFiles 读取指定解压目录的文件树
func (a *Agent) handleStorageFiles(w http.ResponseWriter, r *http.Request) {
	archiveID := r.URL.Query().Get("archive_id")
	extractPath := r.URL.Query().Get("extract_path")
	if extractPath == "" {
		http.Error(w, "缺少 extract_path", http.StatusBadRequest)
		return
	}

	var files []*model.LogFileItem
	_ = filepath.Walk(extractPath, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(extractPath, p)
		if rel == "." || model.IsInternalIndexFile(rel) {
			return nil
		}
		files = append(files, &model.LogFileItem{
			ArchiveID:    archiveID,
			RelativePath: rel,
			Size:         info.Size(),
			ModTime:      info.ModTime(),
			IsDirectory:  info.IsDir(),
		})
		return nil
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(files)
}

// AnalyzeTaskRequest 分布式诊断分析请求
type AnalyzeTaskRequest struct {
	ArchiveID   string        `json:"archive_id"`
	UserID      string        `json:"user_id"`
	ArchivePath string        `json:"archive_path"`
	ExtractDir  string        `json:"extract_dir"`
	Rules       []*model.Rule `json:"rules"`
}

func (a *Agent) handleAnalyzeTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req AnalyzeTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	a.mu.Lock()
	a.activeTask++
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		a.activeTask--
		a.mu.Unlock()
	}()

	log.Printf("[Worker] 开始解压并分析归档包: %s -> %s", req.ArchivePath, req.ExtractDir)
	files, totalLines, err := ExtractArchive(req.ArchivePath, req.ExtractDir)
	if err != nil {
		http.Error(w, fmt.Sprintf("解压失败: %v", err), http.StatusInternalServerError)
		return
	}

	// 异步预热构建稀疏行号索引与分块布隆索引
	go func(targetFiles []*model.LogFileItem, baseDir string) {
		for _, f := range targetFiles {
			if f.IsDirectory {
				continue
			}
			fullPath := filepath.Join(baseDir, f.RelativePath)
			_, _ = GetOrBuildLineIndex(fullPath)
			_, _ = GetOrBuildBloomIndex(fullPath)
		}
	}(files, req.ExtractDir)

	// 规则诊断
	engine := rules.NewEngine(req.Rules)
	report, err := engine.DiagnoseDirectory(req.ArchiveID, req.UserID, req.ArchivePath, req.ExtractDir)
	if err != nil {
		http.Error(w, fmt.Sprintf("规则分析失败: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"files":       files,
		"total_lines": totalLines,
		"report":      report,
	})
}

func (a *Agent) handleSearchTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ExtractDir string            `json:"extract_dir"`
		Query      model.SearchQuery `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	resp, err := SearchLogsContext(r.Context(), req.ExtractDir, &req.Query)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (a *Agent) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// 启动先立即注册一次
	a.sendHeartbeat()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.mu.Lock()
			decom := a.isDecommissioned
			a.mu.Unlock()
			if decom {
				return
			}
			a.sendHeartbeat()
		}
	}
}

func (a *Agent) sendHeartbeat() {
	a.mu.Lock()
	if a.isDecommissioned {
		a.mu.Unlock()
		return
	}
	tasks := a.activeTask
	a.mu.Unlock()

	res := a.collectSystemResource()

	// 业务组件健康自检与主动上报告警 (磁盘满、只读挂载、内存超高)
	a.checkSelfHealth(res)

	node := &model.Node{
		ID:            a.nodeID,
		Name:          a.cfg.NodeName,
		IP:            a.cfg.AdvertiseIP,
		Port:          a.cfg.Port,
		Role:          "worker",
		Status:        "online",
		Resource:      res,
		MountPoint:    a.cfg.DataDir,
		ActiveTasks:   tasks,
		LastHeartbeat: time.Now(),
	}

	data, _ := json.Marshal(node)

	// 支持多个以逗号分隔的 Manager URL (主备高可用架构)
	mgrURLs := strings.Split(a.cfg.ManagerURL, ",")
	client := &http.Client{Timeout: 3 * time.Second}

	for _, rawURL := range mgrURLs {
		cleanURL := strings.TrimSpace(rawURL)
		if cleanURL == "" {
			continue
		}
		targetURL := fmt.Sprintf("%s/api/cluster/heartbeat", strings.TrimRight(cleanURL, "/"))
		req, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(data))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Cluster-Token", a.cfg.ClusterToken)

		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusGone {
				log.Printf("[Worker] 收到管理节点 410 Gone (该节点已从集群移除注销)，执行自动下线停止...")
				go a.executeDecommission()
				return
			}
			if resp.StatusCode == http.StatusOK {
				// 心跳成功送达当前活跃的管理节点
				return
			}
		}
	}
	log.Printf("[Worker] 向管理节点 (%s) 发送心跳均未能成功响应", a.cfg.ManagerURL)
}

func (a *Agent) handleDecommission(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := r.Header.Get("X-Cluster-Token")
	if token != a.cfg.ClusterToken {
		http.Error(w, "Token 验证失败", http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"message": "Worker 已收到下线注销指令，正在执行清理并停止服务",
	})

	go a.executeDecommission()
}

func (a *Agent) executeDecommission() {
	a.mu.Lock()
	if a.isDecommissioned {
		a.mu.Unlock()
		return
	}
	a.isDecommissioned = true
	a.mu.Unlock()

	log.Printf("[Worker Agent] 收到集群注销指令，正在停用服务与注销守护进程...")

	// 1. 如果存在由 Manager 一键部署注册的 Systemd 服务，主动停止并禁用服务文件，防止 Systemd 自动拉起
	stopServiceCmd := fmt.Sprintf(`
		for svc in /etc/systemd/system/dist-log-worker-*.service /etc/systemd/system/dist-log-worker.service; do
			if [ -f "$svc" ]; then
				sname=$(basename "$svc")
				if grep -q "port=%d" "$svc" 2>/dev/null || grep -q "%s" "$svc" 2>/dev/null; then
					sudo -n systemctl stop "$sname" 2>/dev/null || systemctl stop "$sname" 2>/dev/null || true
					sudo -n systemctl disable "$sname" 2>/dev/null || systemctl disable "$sname" 2>/dev/null || true
					sudo -n rm -f "$svc" 2>/dev/null || rm -f "$svc" 2>/dev/null || true
					sudo -n systemctl daemon-reload 2>/dev/null || systemctl daemon-reload 2>/dev/null || true
				fi
			fi
		done
	`, a.cfg.Port, a.cfg.DataDir)
	_ = exec.Command("sh", "-c", stopServiceCmd).Run()

	// 2. 异步优雅停止 HTTP 服务并退出
	go func() {
		time.Sleep(500 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if a.server != nil {
			_ = a.server.Shutdown(ctx)
		}
		os.Exit(0)
	}()
}

// ReportAlarm 业务组件向管理节点主动上报告警事件 (支持多 Manager 故障自动转移与防风暴消抖)
func (a *Agent) ReportAlarm(alarmType, severity, title, message string) {
	a.alarmMu.Lock()
	lastTime, exists := a.alarmCooldown[alarmType]
	if exists && time.Since(lastTime) < 30*time.Second {
		a.alarmMu.Unlock()
		return // 30秒内同类告警限频消抖
	}
	a.alarmCooldown[alarmType] = time.Now()
	a.alarmMu.Unlock()

	reqPayload := model.AlarmReportReq{
		NodeID:    a.nodeID,
		AlarmType: alarmType,
		Severity:  severity,
		Title:     title,
		Message:   message,
	}
	data, _ := json.Marshal(reqPayload)

	mgrURLs := strings.Split(a.cfg.ManagerURL, ",")
	client := &http.Client{Timeout: 3 * time.Second}

	for _, rawURL := range mgrURLs {
		cleanURL := strings.TrimSpace(rawURL)
		if cleanURL == "" {
			continue
		}
		targetURL := fmt.Sprintf("%s/api/alarms/report", strings.TrimRight(cleanURL, "/"))
		req, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(data))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Cluster-Token", a.cfg.ClusterToken)

		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				log.Printf("[Worker Alarm] 成功上报告警 [%s] 到管理节点 (%s): %s", alarmType, cleanURL, title)
				return
			}
		}
	}
	log.Printf("[Worker Alarm] 上报告警 [%s] 到管理节点失败: %s", alarmType, title)
}

// checkSelfHealth 检查业务组件健康状态，发现异常及时上报告警
func (a *Agent) checkSelfHealth(res model.SystemResource) {
	// 1. 磁盘空间严重不足检测
	if res.DiskFreeMB > 0 && res.DiskFreeMB < 1024 {
		a.ReportAlarm(
			model.AlarmTypeDiskFull,
			model.SeverityCritical,
			fmt.Sprintf("业务存储磁盘空间严重不足 (剩余 %d MB)", res.DiskFreeMB),
			fmt.Sprintf("计算节点 %s 存储目录 %s 可用空间仅剩 %d MB (< 1GB)，可能导致日志写入及解压失败，请尽快扩容或清理", a.cfg.NodeName, a.cfg.DataDir, res.DiskFreeMB),
		)
	}

	// 2. 存储挂载目录只读与 I/O 异常检测
	testFile := filepath.Join(a.cfg.DataDir, ".write_health_test.tmp")
	if err := os.WriteFile(testFile, []byte("ok"), 0644); err != nil {
		a.ReportAlarm(
			model.AlarmTypeDiskReadOnly,
			model.SeverityCritical,
			"业务存储文件系统发生只读或 I/O 写入故障",
			fmt.Sprintf("计算节点 %s 存储目录 %s 无法写入文件: %v，可能硬盘损坏或被内核置为只读模式", a.cfg.NodeName, a.cfg.DataDir, err),
		)
	} else {
		_ = os.Remove(testFile)
	}

	// 3. 内存超高使用率检测
	if res.MemTotalMB > 0 {
		usageRatio := float64(res.MemUsedMB) / float64(res.MemTotalMB)
		if usageRatio > 0.92 {
			a.ReportAlarm(
				model.AlarmTypeHighMemory,
				model.SeverityWarning,
				fmt.Sprintf("业务节点内存占用过高 (%.1f%%)", usageRatio*100),
				fmt.Sprintf("计算节点 %s 当前内存已使用 %d MB / %d MB (%.1f%%)，面临 OOM 风险", a.cfg.NodeName, res.MemUsedMB, res.MemTotalMB, usageRatio*100),
			)
		}
	}
}

// collectSystemResource 纯 Go 读取 Linux /proc 系统硬件及状态指标
func (a *Agent) collectSystemResource() model.SystemResource {
	res := model.SystemResource{
		OS:   runtime.GOOS,
		Arch: runtime.GOARCH,
	}

	// 磁盘空间 (采集总容量、已用容量、可用容量与使用率)
	var stat syscall.Statfs_t
	if err := syscall.Statfs(a.cfg.DataDir, &stat); err == nil {
		totalMB := int64(stat.Blocks * uint64(stat.Bsize) / (1024 * 1024))
		freeMB := int64(stat.Bavail * uint64(stat.Bsize) / (1024 * 1024))
		usedMB := totalMB - freeMB
		if usedMB < 0 {
			usedMB = 0
		}
		res.DiskTotalMB = totalMB
		res.DiskUsedMB = usedMB
		res.DiskFreeMB = freeMB
		if totalMB > 0 {
			res.DiskUsedPercent = float64(usedMB) / float64(totalMB) * 100
		}
	}

	// 内存读取 /proc/meminfo
	if memData, err := os.ReadFile("/proc/meminfo"); err == nil {
		lines := strings.Split(string(memData), "\n")
		var totalKB, freeKB, buffersKB, cachedKB int64
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				val, _ := strconv.ParseInt(fields[1], 10, 64)
				switch fields[0] {
				case "MemTotal:":
					totalKB = val
				case "MemFree:":
					freeKB = val
				case "Buffers:":
					buffersKB = val
				case "Cached:":
					cachedKB = val
				}
			}
		}
		res.MemTotalMB = totalKB / 1024
		usedKB := totalKB - freeKB - buffersKB - cachedKB
		if usedKB < 0 {
			usedKB = 0
		}
		res.MemUsedMB = usedKB / 1024
	}

	// CPU 活跃度估算
	res.CPUPercent = float64(runtime.NumGoroutine())
	return res
}
