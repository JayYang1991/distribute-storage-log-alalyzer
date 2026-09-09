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
	cfg        *config.Config
	server     *http.Server
	nodeID     string
	activeTask int
	mu         sync.Mutex
}

func NewAgent(cfg *config.Config) *Agent {
	nodeID := fmt.Sprintf("worker_%s_%d", cfg.NodeName, cfg.Port)
	return &Agent{
		cfg:    cfg,
		nodeID: nodeID,
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
	mux.HandleFunc("/api/worker/storage/files", a.handleStorageFiles)

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
	defer destFile.Close()

	if _, err := io.Copy(destFile, file); err != nil {
		http.Error(w, fmt.Sprintf("写入文件失败: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("[Worker Storage] 日志包已持久化存放在本节点硬盘: %s", destPath)

	// 2. 本地解包与文件树提取
	files, totalLines, err := ExtractArchive(destPath, extractDir)
	if err != nil {
		http.Error(w, fmt.Sprintf("解包失败: %v", err), http.StatusInternalServerError)
		return
	}

	// 3. 执行规则匹配诊断
	engine := rules.NewEngine(ruleList)
	report, err := engine.DiagnoseDirectory(archiveID, userID, header.Filename, extractDir)
	if err != nil {
		http.Error(w, fmt.Sprintf("诊断失败: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"files":        files,
		"total_lines":  totalLines,
		"extract_path": extractDir,
		"report":       report,
	})
}

// handleStorageFileContent 读取保存在本节点硬盘上的文件内容
func (a *Agent) handleStorageFileContent(w http.ResponseWriter, r *http.Request) {
	relPath := r.URL.Query().Get("path")
	extractPath := r.URL.Query().Get("extract_path")
	if relPath == "" || extractPath == "" {
		http.Error(w, "缺少必要参数 path 或 extract_path", http.StatusBadRequest)
		return
	}

	fullPath := filepath.Join(extractPath, relPath)
	if !strings.HasPrefix(filepath.Clean(fullPath), filepath.Clean(extractPath)) {
		http.Error(w, "非法访问路径", http.StatusForbidden)
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
		if rel == "." {
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

	resp, err := SearchLogs(req.ExtractDir, &req.Query)
	if err != nil {
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
			a.sendHeartbeat()
		}
	}
}

func (a *Agent) sendHeartbeat() {
	res := a.collectSystemResource()
	a.mu.Lock()
	tasks := a.activeTask
	a.mu.Unlock()

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
			if resp.StatusCode == http.StatusOK {
				// 心跳成功送达当前活跃的管理节点
				return
			}
		}
	}
	log.Printf("[Worker] 向管理节点 (%s) 发送心跳均未能成功响应", a.cfg.ManagerURL)
}

// collectSystemResource 纯 Go 读取 Linux /proc 系统硬件及状态指标
func (a *Agent) collectSystemResource() model.SystemResource {
	res := model.SystemResource{
		OS:   runtime.GOOS,
		Arch: runtime.GOARCH,
	}

	// 磁盘空间
	var stat syscall.Statfs_t
	if err := syscall.Statfs(a.cfg.DataDir, &stat); err == nil {
		res.DiskFreeMB = int64(stat.Bavail * uint64(stat.Bsize) / (1024 * 1024))
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
