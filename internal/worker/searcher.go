package worker

import (
	"bufio"
	"bytes"
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

const maxCollectedHits = 1000

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
	var literalKw []byte
	var err error

	if q.Keyword != "" {
		if !q.IsRegex {
			literalKw = []byte(q.Keyword)
		} else {
			pattern := q.Keyword
			if !q.CaseSensitive {
				pattern = "(?i)" + pattern
			}
			reg, err = regexp.Compile(pattern)
			if err != nil {
				return nil, err
			}
			// 提取正则中的字面量前缀做短路加速
			if longest := extractLongestSearchLiteral(q.Keyword); len(longest) >= 3 {
				literalKw = []byte(longest)
			}
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

	// 2. 检索各个日志文件（带上限提前终止，防止大日志导致内存占满与超时）
	if len(tasks) == 1 {
		allMatchedHits = searchInSingleFile(tasks[0].fullPath, tasks[0].relPath, reg, literalKw, q.CaseSensitive, q.IsRegex, targetLevel, q.ContextLines, maxCollectedHits)
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
					mu.Lock()
					currentCount := len(allMatchedHits)
					mu.Unlock()
					if currentCount >= maxCollectedHits {
						break
					}

					remaining := maxCollectedHits - currentCount
					hits := searchInSingleFile(t.fullPath, t.relPath, reg, literalKw, q.CaseSensitive, q.IsRegex, targetLevel, q.ContextLines, remaining)
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

// searchInSingleFile 采用滑动环形缓冲区与零内存分配字节匹配，流式搜索单个文件，内存复杂度 O(contextLines)，杜绝大文件 OOM
func searchInSingleFile(fullPath, relPath string, reg *regexp.Regexp, literalKw []byte, caseSensitive, isRegex bool, levelFilter string, contextLines int, maxHits int) []model.SearchHit {
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
		lineBytes := scanner.Bytes()

		// 1. 先为等待后续上下文的 pending hit 追加当前行
		if len(pending) > 0 {
			lineText := string(lineBytes)
			var activePending []*pendingSearchHit
			for _, p := range pending {
				p.hit.ContextAfter = append(p.hit.ContextAfter, lineText)
				p.remaining--
				if p.remaining <= 0 {
					hits = append(hits, p.hit)
				} else {
					activePending = append(activePending, p)
				}
			}
			pending = activePending

			if len(hits) >= maxHits {
				break
			}
		}

		// 2. 判断当前行是否匹配日志级别
		lineLevel := detectLogLevelBytes(lineBytes)
		if levelFilter != "" && levelFilter != "ALL" {
			if !matchLogLevel(lineLevel, levelFilter) {
				if contextLines > 0 {
					pushToRing(&ring, string(lineBytes), contextLines)
				}
				continue
			}
		}

		// 3. 检查关键词匹配 (Fast Path 高速字节匹配，避免 Go 正则状态机开销)
		matched := false
		if len(literalKw) == 0 && reg == nil {
			matched = true
		} else if !isRegex {
			if caseSensitive {
				matched = bytes.Contains(lineBytes, literalKw)
			} else {
				matched = bytesContainsFoldASCII(lineBytes, literalKw)
			}
		} else {
			// 正则模式：若提取出字面量，先用字面量做高速 O(1) 预过滤
			if len(literalKw) > 0 {
				hasCandidate := false
				if caseSensitive {
					hasCandidate = bytes.Contains(lineBytes, literalKw)
				} else {
					hasCandidate = bytesContainsFoldASCII(lineBytes, literalKw)
				}
				if !hasCandidate {
					if contextLines > 0 {
						pushToRing(&ring, string(lineBytes), contextLines)
					}
					continue
				}
			}
			if reg != nil {
				matched = reg.Match(lineBytes)
			}
		}

		if !matched {
			if contextLines > 0 {
				pushToRing(&ring, string(lineBytes), contextLines)
			}
			continue
		}

		// 4. 命中：此时才分配 string 构造 SearchHit 实体
		lineText := string(lineBytes)
		contextBefore := make([]string, len(ring))
		copy(contextBefore, ring)

		newHit := model.SearchHit{
			FilePath:      relPath,
			LineNumber:    lineNum,
			Content:       lineText,
			Level:         lineLevel,
			Timestamp:     extractTimestamp(lineText),
			ContextBefore: contextBefore,
			ContextAfter:  make([]string, 0, contextLines),
		}

		if contextLines == 0 {
			hits = append(hits, newHit)
			if len(hits) >= maxHits {
				break
			}
		} else {
			pending = append(pending, &pendingSearchHit{
				hit:       newHit,
				remaining: contextLines,
			})
		}

		// 5. 将当前行推入前序环形缓冲区
		if contextLines > 0 {
			pushToRing(&ring, lineText, contextLines)
		}
	}

	// 处理尾部未填满的 pending hits
	for _, p := range pending {
		hits = append(hits, p.hit)
	}

	return hits
}

// bytesContainsFoldASCII 零内存分配的 ASCII 大小写不敏感快速子串匹配
func bytesContainsFoldASCII(s, substr []byte) bool {
	if len(substr) == 0 {
		return true
	}
	if len(s) < len(substr) {
		return false
	}
	c0 := substr[0]
	c0Alt := c0
	if c0 >= 'a' && c0 <= 'z' {
		c0Alt = c0 - 32
	} else if c0 >= 'A' && c0 <= 'Z' {
		c0Alt = c0 + 32
	}

	maxI := len(s) - len(substr)
	for i := 0; i <= maxI; i++ {
		b := s[i]
		if b == c0 || b == c0Alt {
			matched := true
			for j := 1; j < len(substr); j++ {
				sb := s[i+j]
				tb := substr[j]
				if sb != tb {
					if sb >= 'A' && sb <= 'Z' {
						sb += 32
					}
					if tb >= 'A' && tb <= 'Z' {
						tb += 32
					}
					if sb != tb {
						matched = false
						break
					}
				}
			}
			if matched {
				return true
			}
		}
	}
	return false
}

// extractLongestSearchLiteral 从正则表达式中提取最长连续字面量字符串用于预过滤
func extractLongestSearchLiteral(pattern string) string {
	metaChars := `.*+?^${}()|[]\`
	parts := strings.FieldsFunc(pattern, func(r rune) bool {
		return strings.ContainsRune(metaChars, r)
	})
	longest := ""
	for _, p := range parts {
		clean := strings.TrimSpace(p)
		if len(clean) > len(longest) {
			longest = clean
		}
	}
	return longest
}

// detectLogLevelBytes 零内存分配快速检测日志级别，避免 string 堆分配
func detectLogLevelBytes(b []byte) string {
	if bytes.Contains(b, []byte("FATAL")) || bytes.Contains(b, []byte("fatal")) || bytes.Contains(b, []byte("EMERG")) {
		return "FATAL"
	}
	if bytes.Contains(b, []byte("CRIT")) || bytes.Contains(b, []byte("critical")) || bytes.Contains(b, []byte("CRITICAL")) {
		return "CRITICAL"
	}
	if bytes.Contains(b, []byte("ERR")) || bytes.Contains(b, []byte("error")) || bytes.Contains(b, []byte("ERROR")) || bytes.Contains(b, []byte("Error")) {
		return "ERROR"
	}
	if bytes.Contains(b, []byte("WARN")) || bytes.Contains(b, []byte("warning")) || bytes.Contains(b, []byte("WARNING")) || bytes.Contains(b, []byte("Warn")) {
		return "WARN"
	}
	if bytes.Contains(b, []byte("DEBUG")) || bytes.Contains(b, []byte("debug")) || bytes.Contains(b, []byte("Debug")) {
		return "DEBUG"
	}
	if bytes.Contains(b, []byte("TRACE")) || bytes.Contains(b, []byte("trace")) {
		return "TRACE"
	}
	return "INFO"
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

