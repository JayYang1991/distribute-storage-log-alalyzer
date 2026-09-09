package rules

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"dist-log-analyzer/internal/model"
)

// Engine 故障规则诊断引擎
type Engine struct {
	compiledRules []*compiledRule
}

type compiledRule struct {
	rule  *model.Rule
	regex *regexp.Regexp
}

// NewEngine 构造诊断引擎并预编译所有启用的规则
func NewEngine(rules []*model.Rule) *Engine {
	var compiled []*compiledRule
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		cr := &compiledRule{rule: r}
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

// DiagnoseDirectory 遍历指定目录并诊断所有日志文件
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

	err := filepath.WalkDir(rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		// 只检查文本或日志文件
		if !isLogFile(d.Name()) {
			return nil
		}

		relPath, _ := filepath.Rel(rootDir, path)
		events, err := e.DiagnoseFile(archiveID, relPath, path, maxEvents-len(report.Events))
		if err == nil && len(events) > 0 {
			report.Events = append(report.Events, events...)
		}
		if len(report.Events) >= maxEvents {
			return filepath.SkipAll
		}
		return nil
	})

	if err != nil {
		report.Status = "failed"
		return report, err
	}

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

// DiagnoseFile 诊断单个文件
func (e *Engine) DiagnoseFile(archiveID, relPath, filePath string, limit int) ([]model.DiagnosisEvent, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var events []model.DiagnosisEvent
	scanner := bufio.NewScanner(file)
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, 10*1024*1024)

	var lineNum int64 = 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		for _, cr := range e.compiledRules {
			matched := false
			if cr.regex != nil {
				matched = cr.regex.MatchString(line)
			} else {
				matched = strings.Contains(strings.ToLower(line), strings.ToLower(cr.rule.Pattern))
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
