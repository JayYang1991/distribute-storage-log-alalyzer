package manager

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
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
		archive.ErrorMsg = fmt.Sprintf("解压失败: %v", err)
		_ = sc.store.SaveArchive(archive)
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
	nodes, err := sc.store.ListNodes()
	if err != nil || len(nodes) == 0 {
		return nil
	}

	now := time.Now()
	var best *model.Node

	for _, n := range nodes {
		if n.Role != "worker" || n.Status != "online" {
			continue
		}
		// 超过 15 秒无心跳视为离线
		if now.Sub(n.LastHeartbeat) > 15*time.Second {
			n.Status = "offline"
			_ = sc.store.SaveNode(n)
			continue
		}

		if best == nil || n.ActiveTasks < best.ActiveTasks {
			best = n
		}
	}
	return best
}
