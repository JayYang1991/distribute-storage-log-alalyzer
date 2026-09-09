package rules

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dist-log-analyzer/internal/model"
)

// Engine 故障规则诊断引擎
type Engine struct {
	compiledRules []*compiledRule
}

type compiledRule struct {
	rule         *model.Rule
	regex        *regexp.Regexp
	patternLower string
}

var scanBufPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 256*1024)
		return &buf
	},
}

// NewEngine 构造诊断引擎并预编译所有启用的规则
func NewEngine(rules []*model.Rule) *Engine {
	var compiled []*compiledRule
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		cr := &compiledRule{
			rule:         r,
			patternLower: strings.ToLower(r.Pattern),
		}
		if r.IsRegex {
			if reg, err := regexp.Compile(r.Pattern); err == nil {
				cr.regex = reg
			} else {
				// 降级为字面匹配
				cr.regex = regexp.MustCompile(regexp.QuoteMeta(r.Pattern))
			}
		}
		compiled = append(compiled, cr)
	}
	return &Engine{compiledRules: compiled}
}

type fileTask struct {
	relPath  string
	fullPath string
}

// DiagnoseDirectory 遍历指定目录并高并发诊断所有日志文件
func (e *Engine) DiagnoseDirectory(archiveID, userID, archiveName, rootDir string) (*model.DiagnosisReport, error) {
	report := &model.DiagnosisReport{
		ArchiveID:       archiveID,
		UserID:          userID,
		ArchiveName:     archiveName,
		Status:          "running",
		SeveritySummary: make(map[string]int),
		StorageSummary:  make(map[string]int),
		Events:          make([]model.DiagnosisEvent, 0),
		AnalyzedAt:      time.Now(),
	}

	// 限制最多记录 500 条事件以避免极端大日志爆内存
	maxEvents := 500

	// 1. 快速遍历收集所有待分析的目标日志文件
	var tasks []fileTask
	err := filepath.WalkDir(rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !isLogFile(d.Name()) {
			return nil
		}
		relPath, _ := filepath.Rel(rootDir, path)
		tasks = append(tasks, fileTask{
			relPath:  relPath,
			fullPath: path,
		})
		return nil
	})

	if err != nil {
		report.Status = "failed"
		return report, err
	}

	if len(tasks) == 0 {
		report.Status = "completed"
		report.SummaryText = "归档包中未包含可供分析的文本日志文件。"
		report.HealthScore = 100
		return report, nil
	}

	// 2. 依据 CPU 核数启动 Worker Pool 并行分析
	workerCount := runtime.NumCPU()
	if workerCount > len(tasks) {
		workerCount = len(tasks)
	}
	if workerCount > 16 {
		workerCount = 16
	}
	if workerCount < 1 {
		workerCount = 1
	}

	taskChan := make(chan fileTask, len(tasks))
	for _, t := range tasks {
		taskChan <- t
	}
	close(taskChan)

	var (
		mu          sync.Mutex
		allEvents   []model.DiagnosisEvent
		totalEvents int64
		wg          sync.WaitGroup
	)

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range taskChan {
				// 如果已达到上限，快速跳过后续任务
				currentCount := atomic.LoadInt64(&totalEvents)
				if currentCount >= int64(maxEvents) {
					break
				}

				remain := maxEvents - int(currentCount)
				if remain <= 0 {
					break
				}

				events, err := e.DiagnoseFile(archiveID, task.relPath, task.fullPath, remain)
				if err == nil && len(events) > 0 {
					mu.Lock()
					if len(allEvents) < maxEvents {
						avail := maxEvents - len(allEvents)
						if len(events) > avail {
							events = events[:avail]
						}
						allEvents = append(allEvents, events...)
						atomic.StoreInt64(&totalEvents, int64(len(allEvents)))
					}
					mu.Unlock()
				}
			}
		}()
	}

	wg.Wait()
	report.Events = allEvents
	report.TotalEvents = len(report.Events)
	report.Status = "completed"

	// 汇总统计与计算健康分
	score := 100
	for _, ev := range report.Events {
		report.SeveritySummary[ev.Severity]++
		report.StorageSummary[ev.StorageType]++
		switch ev.Severity {
		case model.SeverityFatal:
			score -= 25
		case model.SeverityCritical:
			score -= 10
		case model.SeverityWarning:
			score -= 3
		}
	}
	if score < 0 {
		score = 0
	}
	report.HealthScore = score

	if report.TotalEvents == 0 {
		report.SummaryText = "恭喜！未在日志包中匹配到预设的已知高危存储故障特征，集群日志运行状态总体良好。"
	} else {
		report.SummaryText = fmt.Sprintf("本次共检出 %d 处存储异常事件 (致命: %d, 严重: %d, 警告: %d)。系统健康评估分为 %d 分，请及时查看下方详细排查与自愈建议！",
			report.TotalEvents,
			report.SeveritySummary[model.SeverityFatal],
			report.SeveritySummary[model.SeverityCritical],
			report.SeveritySummary[model.SeverityWarning],
			report.HealthScore,
		)
	}

	return report, nil
}

// DiagnoseFile 诊断单个文件 (利用 sync.Pool 缓冲复用与单行一次性小写优化)
func (e *Engine) DiagnoseFile(archiveID, relPath, filePath string, limit int) ([]model.DiagnosisEvent, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var events []model.DiagnosisEvent
	scanner := bufio.NewScanner(file)

	// 从缓冲池复用 256KB 初始切片，规避高频堆内存申请
	bufPtr := scanBufPool.Get().(*[]byte)
	defer scanBufPool.Put(bufPtr)
	scanner.Buffer(*bufPtr, 10*1024*1024)

	var lineNum int64 = 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		var lineLower string
		var lineLowerInit bool

		for _, cr := range e.compiledRules {
			matched := false
			if cr.regex != nil {
				matched = cr.regex.MatchString(line)
			} else {
				// 优化：单行在命中非正则规则时仅做 1 次 strings.ToLower，使用预先小写的 patternLower 进行比对
				if !lineLowerInit {
					lineLower = strings.ToLower(line)
					lineLowerInit = true
				}
				matched = strings.Contains(lineLower, cr.patternLower)
			}

			if matched {
				events = append(events, model.DiagnosisEvent{
					ID:             fmt.Sprintf("evt_%s_%d_%s", archiveID, lineNum, cr.rule.ID),
					ArchiveID:      archiveID,
					RuleID:         cr.rule.ID,
					RuleName:       cr.rule.Name,
					Severity:       cr.rule.Severity,
					StorageType:    cr.rule.StorageType,
					FilePath:       relPath,
					LineNumber:     lineNum,
					MatchedContent: truncateString(line, 500),
					Suggestion:     cr.rule.Suggestion,
					Timestamp:      extractTimestamp(line),
				})
				if len(events) >= limit {
					return events, nil
				}
			}
		}
	}

	return events, scanner.Err()
}

func isLogFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".log", ".txt", ".out", ".err", ".trace", ".json", ".csv", "":
		return true
	default:
		// 很多无后缀或者带有日期如 .log.2026-09-08
		return strings.Contains(name, "log") || strings.Contains(name, "trace")
	}
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

var timeRegs = []*regexp.Regexp{
	regexp.MustCompile(`\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(\.\d+)?`),
	regexp.MustCompile(`\w{3}\s+\d{1,2}\s+\d{2}:\d{2}:\d{2}`),
}

func extractTimestamp(line string) string {
	for _, reg := range timeRegs {
		if match := reg.FindString(line); match != "" {
			return match
		}
	}
	return ""
}
