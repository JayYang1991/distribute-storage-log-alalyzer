package worker

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"dist-log-analyzer/internal/model"
)

var searchBufPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 256*1024)
		return &buf
	},
}

type searchTask struct {
	fullPath string
	relPath  string
}

// SearchLogs 在目标解压目录下执行高并发、流式低内存日志检索
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

	// 1. 快速收集所有满足路径过滤条件的文件任务
	var tasks []searchTask
	_ = filepath.WalkDir(extractDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(extractDir, path)

		// 文件路径过滤
		if q.FilePath != "" && !strings.Contains(strings.ToLower(rel), strings.ToLower(q.FilePath)) {
			return nil
		}

		tasks = append(tasks, searchTask{
			fullPath: path,
			relPath:  rel,
		})
		return nil
	})

	if len(tasks) == 0 {
		resp.CostMS = time.Since(start).Milliseconds()
		return resp, nil
	}

	var allMatchedHits []model.SearchHit

	// 2. 多核并发检索各个日志文件
	if len(tasks) == 1 {
		allMatchedHits = searchInSingleFile(tasks[0].fullPath, tasks[0].relPath, reg, targetLevel, q.ContextLines)
	} else {
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

		taskChan := make(chan searchTask, len(tasks))
		for _, t := range tasks {
			taskChan <- t
		}
		close(taskChan)

		var mu sync.Mutex
		var wg sync.WaitGroup

		for i := 0; i < workerCount; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for t := range taskChan {
					hits := searchInSingleFile(t.fullPath, t.relPath, reg, targetLevel, q.ContextLines)
					if len(hits) > 0 {
						mu.Lock()
						allMatchedHits = append(allMatchedHits, hits...)
						mu.Unlock()
					}
				}
			}()
		}
		wg.Wait()
	}

	resp.TotalHits = int64(len(allMatchedHits))

	// 进行分页切片
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

type pendingSearchHit struct {
	hit       model.SearchHit
	remaining int
}

// searchInSingleFile 采用滑动环形缓冲区流式搜索单个文件，内存复杂度 O(contextLines)，杜绝大文件 OOM
func searchInSingleFile(fullPath, relPath string, reg *regexp.Regexp, levelFilter string, contextLines int) []model.SearchHit {
	f, err := os.Open(fullPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	bufPtr := searchBufPool.Get().(*[]byte)
	defer searchBufPool.Put(bufPtr)
	scanner.Buffer(*bufPtr, 10*1024*1024)

	var hits []model.SearchHit
	var ring []string
	var pending []*pendingSearchHit

	var lineNum int64 = 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		// 1. 先为等待后续上下文的 pending hit 追加当前行
		var activePending []*pendingSearchHit
		for _, p := range pending {
			p.hit.ContextAfter = append(p.hit.ContextAfter, line)
			p.remaining--
			if p.remaining <= 0 {
				hits = append(hits, p.hit)
			} else {
				activePending = append(activePending, p)
			}
		}
		pending = activePending

		// 2. 判断当前行是否匹配级别与搜索模式
		lineLevel := detectLogLevel(line)
		if levelFilter != "" && levelFilter != "ALL" {
			if !matchLogLevel(lineLevel, levelFilter) {
				pushToRing(&ring, line, contextLines)
				continue
			}
		}

		if reg != nil && !reg.MatchString(line) {
			pushToRing(&ring, line, contextLines)
			continue
		}

		// 3. 构造匹配命中实体，前序上下文从 ring 取出深拷贝
		contextBefore := make([]string, len(ring))
		copy(contextBefore, ring)

		newHit := model.SearchHit{
			FilePath:      relPath,
			LineNumber:    lineNum,
			Content:       line,
			Level:         lineLevel,
			Timestamp:     extractTimestamp(line),
			ContextBefore: contextBefore,
			ContextAfter:  make([]string, 0, contextLines),
		}

		if contextLines == 0 {
			hits = append(hits, newHit)
		} else {
			pending = append(pending, &pendingSearchHit{
				hit:       newHit,
				remaining: contextLines,
			})
		}

		// 4. 将当前行推入前序环形缓冲区
		pushToRing(&ring, line, contextLines)
	}

	// 5. 文件结束处理未收集满 contextLines 的尾部 pending hits
	for _, p := range pending {
		hits = append(hits, p.hit)
	}

	return hits
}

func pushToRing(ring *[]string, line string, maxCap int) {
	if maxCap <= 0 {
		return
	}
	if len(*ring) < maxCap {
		*ring = append(*ring, line)
	} else {
		copy((*ring)[0:], (*ring)[1:])
		(*ring)[maxCap-1] = line
	}
}

// detectLogLevel 零内存分配快速检测日志级别，避免 strings.ToUpper 全量字符串堆分配
func detectLogLevel(line string) string {
	if strings.Contains(line, "FATAL") || strings.Contains(line, "fatal") || strings.Contains(line, "EMERG") {
		return "FATAL"
	}
	if strings.Contains(line, "CRIT") || strings.Contains(line, "critical") || strings.Contains(line, "CRITICAL") {
		return "CRITICAL"
	}
	if strings.Contains(line, "ERR") || strings.Contains(line, "error") || strings.Contains(line, "ERROR") || strings.Contains(line, "Error") {
		return "ERROR"
	}
	if strings.Contains(line, "WARN") || strings.Contains(line, "warning") || strings.Contains(line, "WARNING") || strings.Contains(line, "Warn") {
		return "WARN"
	}
	if strings.Contains(line, "DEBUG") || strings.Contains(line, "debug") || strings.Contains(line, "Debug") {
		return "DEBUG"
	}
	if strings.Contains(line, "TRACE") || strings.Contains(line, "trace") {
		return "TRACE"
	}
	return "INFO"
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

