package manager

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/rules"
	"dist-log-analyzer/internal/store"
	"dist-log-analyzer/internal/worker"
)

// Scheduler 分布式任务调度中心
type Scheduler struct {
	store *store.Store
	mu    sync.RWMutex
}

func NewScheduler(s *store.Store) *Scheduler {
	return &Scheduler{store: s}
}

// DispatchAnalyzeTask 分发归档包解压与规则匹配任务
func (sc *Scheduler) DispatchAnalyzeTask(archive *model.LogArchive) {
	go func() {
		archive.Status = "extracting"
		_ = sc.store.SaveArchive(archive)

		// 选取最适合的 Worker 节点
		workerNode := sc.pickAvailableWorker()

		ruleList, _ := sc.store.ListRules()
		archivePath := filepath.Join(sc.store.GetUserArchiveDir(archive.Username), archive.Filename)
		extractDir := sc.store.GetUserExtractDir(archive.Username, archive.ID)

		if workerNode != nil {
			// 分发给远程 Worker
			log.Printf("[Scheduler] 将任务 %s 分派给业务节点 %s (%s:%d)", archive.ID, workerNode.Name, workerNode.IP, workerNode.Port)
			archive.AssignedWorker = workerNode.Name
			_ = sc.store.SaveArchive(archive)

			err := sc.callRemoteWorkerAnalyze(workerNode, archive, archivePath, extractDir, ruleList)
			if err == nil {
				return
			}
			log.Printf("[Scheduler] 远程 Worker 任务执行异常: %v，降级由 Manager 本地引擎处理", err)
		}

		// 本地降级处理或单机模式
		sc.runLocalAnalyze(archive, archivePath, extractDir, ruleList)
	}()
}

func (sc *Scheduler) runLocalAnalyze(archive *model.LogArchive, archivePath, extractDir string, ruleList []*model.Rule) {
	log.Printf("[Scheduler] 本地引擎开始解压分析: %s", archive.Filename)

	files, totalLines, err := worker.ExtractArchive(archivePath, extractDir)
	if err != nil {
		archive.Status = "failed"
		errMsg := err.Error()
		if strings.Contains(strings.ToLower(errMsg), "no space left on device") {
			errMsg = fmt.Sprintf("存储磁盘空间不足 (no space left on device): %v", err)
		}
		archive.ErrorMsg = fmt.Sprintf("解压失败: %s", errMsg)
		_ = sc.store.SaveArchive(archive)
		_ = os.RemoveAll(extractDir)
		return
	}

	archive.Status = "analyzing"
	archive.FileCount = len(files)
	archive.TotalLines = totalLines
	archive.ExtractPath = extractDir
	_ = sc.store.SaveArchive(archive)

	// 执行规则匹配诊断
	engine := rules.NewEngine(ruleList)
	report, err := engine.DiagnoseDirectory(archive.ID, archive.UserID, archive.Filename, extractDir)
	if err != nil {
		archive.Status = "failed"
		archive.ErrorMsg = fmt.Sprintf("故障规则匹配失败: %v", err)
		_ = sc.store.SaveArchive(archive)
		return
	}

	_ = sc.store.SaveReport(report)

	archive.Status = "ready"
	archive.FinishTime = time.Now()
	_ = sc.store.SaveArchive(archive)
	log.Printf("[Scheduler] 日志包 %s 处理完成！检出 %d 处故障事件，健康得分: %d", archive.Filename, report.TotalEvents, report.HealthScore)
}

func (sc *Scheduler) callRemoteWorkerAnalyze(node *model.Node, archive *model.LogArchive, archivePath, extractDir string, ruleList []*model.Rule) error {
	reqBody := worker.AnalyzeTaskRequest{
		ArchiveID:   archive.ID,
		UserID:      archive.UserID,
		ArchivePath: archivePath,
		ExtractDir:  extractDir,
		Rules:       ruleList,
	}
	data, _ := json.Marshal(reqBody)
	url := fmt.Sprintf("http://%s:%d/api/worker/tasks/analyze", node.IP, node.Port)

	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("worker returned status %d: %s", resp.StatusCode, string(b))
	}

	var res struct {
		Files      []*model.LogFileItem   `json:"files"`
		TotalLines int64                  `json:"total_lines"`
		Report     *model.DiagnosisReport `json:"report"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return err
	}

	if res.Report != nil {
		_ = sc.store.SaveReport(res.Report)
	}

	archive.Status = "ready"
	archive.FileCount = len(res.Files)
	archive.TotalLines = res.TotalLines
	archive.ExtractPath = extractDir
	archive.FinishTime = time.Now()
	_ = sc.store.SaveArchive(archive)
	return nil
}

func (sc *Scheduler) pickAvailableWorker() *model.Node {
	return sc.PickLowestUsageWorker(0)
}

// PickLowestUsageWorker 依据存储容量挑选最优业务节点：优先使用已使用容量低的节点存放日志
func (sc *Scheduler) PickLowestUsageWorker(requiredBytes int64) *model.Node {
	nodes, err := sc.store.ListNodes()
	if err != nil || len(nodes) == 0 {
		return nil
	}

	now := time.Now()
	var candidates []*model.Node

	for _, n := range nodes {
		if n.Role != "worker" || n.Status != "online" {
			continue
		}
		// 超过 15 秒无心跳视为离线
		if now.Sub(n.LastHeartbeat) > 15*time.Second {
			continue
		}

		// 检查剩余磁盘空间是否足够
		if requiredBytes > 0 && n.Resource.DiskFreeMB > 0 {
			reqMB := requiredBytes / (1024 * 1024)
			if n.Resource.DiskFreeMB <= reqMB {
				continue // 剩余容量不足以容纳该日志包
			}
		}

		// 检查节点是否存在磁盘只读故障告警
		if a, _ := sc.store.FindActiveAlarm(n.ID, model.AlarmTypeDiskReadOnly); a != nil {
			continue // 排除文件系统只读故障节点
		}

		candidates = append(candidates, n)
	}

	if len(candidates) == 0 {
		return nil
	}

	// 核心排序策略：优先选用已使用容量最低的节点存放
	best := candidates[0]
	for i := 1; i < len(candidates); i++ {
		cur := candidates[i]
		if isWorkerCapacityLower(cur, best) {
			best = cur
		}
	}
	return best
}

// isWorkerCapacityLower 判定 a 节点的已用容量是否低于 b 节点
func isWorkerCapacityLower(a, b *model.Node) bool {
	// 1. 若双方均上报了物理磁盘已用容量 (MB)，优先比较物理磁盘已用空间 (已用空间越小越优先)
	if a.Resource.DiskUsedMB > 0 && b.Resource.DiskUsedMB > 0 {
		if a.Resource.DiskUsedMB != b.Resource.DiskUsedMB {
			return a.Resource.DiskUsedMB < b.Resource.DiskUsedMB
		}
		// 若已用 MB 相同，比较磁盘使用率百分比
		if a.Resource.DiskUsedPercent != b.Resource.DiskUsedPercent {
			return a.Resource.DiskUsedPercent < b.Resource.DiskUsedPercent
		}
	}

	// 2. 比较节点已存储的日志包总大小 (StorageUsedBytes)
	if a.StorageUsedBytes != b.StorageUsedBytes {
		return a.StorageUsedBytes < b.StorageUsedBytes
	}

	// 3. 比较磁盘可用容量 (可用空间越大多越优先)
	if a.Resource.DiskFreeMB != b.Resource.DiskFreeMB {
		return a.Resource.DiskFreeMB > b.Resource.DiskFreeMB
	}

	// 4. 若上述均一致，比较当前活跃任务数
	return a.ActiveTasks < b.ActiveTasks
}
