package worker

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"dist-log-analyzer/internal/model"
)

// SearchLogs 在目标解压目录下执行流式并发日志检索
func SearchLogs(extractDir string, q *model.SearchQuery) (*model.SearchResponse, error) {
	start := time.Now()
	resp := &model.SearchResponse{
		Page:     q.Page,
		PageSize: q.PageSize,
		Hits:     make([]model.SearchHit, 0),
	}
	if resp.Page <= 0 {
		resp.Page = 1
	}
	if resp.PageSize <= 0 {
		resp.PageSize = 50
	}
	if q.ContextLines < 0 {
		q.ContextLines = 2
	} else if q.ContextLines > 10 {
		q.ContextLines = 10
	}

	var reg *regexp.Regexp
	var err error
	if q.Keyword != "" {
		pattern := q.Keyword
		if !q.IsRegex {
			pattern = regexp.QuoteMeta(pattern)
		}
		if !q.CaseSensitive {
			pattern = "(?i)" + pattern
		}
		reg, err = regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
	}

	targetLevel := strings.ToUpper(strings.TrimSpace(q.Level))

	var allMatchedHits []model.SearchHit

	// 遍历目录
	_ = filepath.WalkDir(extractDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(extractDir, path)

		// 文件路径过滤
		if q.FilePath != "" && !strings.Contains(strings.ToLower(rel), strings.ToLower(q.FilePath)) {
			return nil
		}

		hits := searchInSingleFile(path, rel, reg, targetLevel, q.ContextLines)
		if len(hits) > 0 {
			allMatchedHits = append(allMatchedHits, hits...)
		}
		return nil
	})

	resp.TotalHits = int64(len(allMatchedHits))

	// 进行内存分页切片
	offset := (resp.Page - 1) * resp.PageSize
	if offset < len(allMatchedHits) {
		end := offset + resp.PageSize
		if end > len(allMatchedHits) {
			end = len(allMatchedHits)
		}
		resp.Hits = allMatchedHits[offset:end]
	}

	resp.CostMS = time.Since(start).Milliseconds()
	return resp, nil
}

func searchInSingleFile(fullPath, relPath string, reg *regexp.Regexp, levelFilter string, contextLines int) []model.SearchHit {
	f, err := os.Open(fullPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var allLines []string
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 256*1024)
	scanner.Buffer(buf, 10*1024*1024)

	// 收集行数据用于支持上下文展开（若文件特别大，循环环形缓冲区；通常单个日志文件行数适中）
	for scanner.Scan() {
		allLines = append(allLines, scanner.Text())
	}

	var hits []model.SearchHit
	total := len(allLines)

	for i, line := range allLines {
		lineLevel := detectLogLevel(line)

		// 检查日志级别过滤
		if levelFilter != "" && levelFilter != "ALL" {
			if !matchLogLevel(lineLevel, levelFilter) {
				continue
			}
		}

		// 检查正则或关键字
		if reg != nil && !reg.MatchString(line) {
			continue
		}

		// 提取前后上下文
		beforeStart := i - contextLines
		if beforeStart < 0 {
			beforeStart = 0
		}
		var contextBefore []string
		if beforeStart < i {
			contextBefore = allLines[beforeStart:i]
		}

		afterEnd := i + 1 + contextLines
		if afterEnd > total {
			afterEnd = total
		}
		var contextAfter []string
		if i+1 < afterEnd {
			contextAfter = allLines[i+1 : afterEnd]
		}

		hits = append(hits, model.SearchHit{
			FilePath:      relPath,
			LineNumber:    int64(i + 1),
			Content:       line,
			Level:         lineLevel,
			Timestamp:     extractTimestamp(line),
			ContextBefore: contextBefore,
			ContextAfter:  contextAfter,
		})
	}

	return hits
}

func detectLogLevel(line string) string {
	upper := strings.ToUpper(line)
	switch {
	case strings.Contains(upper, "FATAL") || strings.Contains(upper, "EMERG"):
		return "FATAL"
	case strings.Contains(upper, "CRIT") || strings.Contains(upper, "CRITICAL"):
		return "CRITICAL"
	case strings.Contains(upper, "ERR") || strings.Contains(upper, "ERROR"):
		return "ERROR"
	case strings.Contains(upper, "WARN") || strings.Contains(upper, "WARNING"):
		return "WARN"
	case strings.Contains(upper, "INFO"):
		return "INFO"
	case strings.Contains(upper, "DEBUG"):
		return "DEBUG"
	case strings.Contains(upper, "TRACE"):
		return "TRACE"
	default:
		return "INFO"
	}
}

func matchLogLevel(detected, filter string) bool {
	if filter == "ERROR" {
		return detected == "ERROR" || detected == "CRITICAL" || detected == "FATAL"
	}
	if filter == "WARN" {
		return detected == "WARN" || detected == "ERROR" || detected == "CRITICAL" || detected == "FATAL"
	}
	return detected == filter
}

var timeRegexList = []*regexp.Regexp{
	regexp.MustCompile(`\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(\.\d+)?`),
	regexp.MustCompile(`\w{3}\s+\d{1,2}\s+\d{2}:\d{2}:\d{2}`),
}

func extractTimestamp(line string) string {
	for _, reg := range timeRegexList {
		if match := reg.FindString(line); match != "" {
			return match
		}
	}
	return ""
}

