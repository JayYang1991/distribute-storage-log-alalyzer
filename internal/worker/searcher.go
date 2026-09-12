package worker

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"dist-log-analyzer/internal/indexer"
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

const (
	maxCollectedHits            = 5000
	maxPerFileHits              = 1000
	largeFileParallelThreshold = 32 * 1024 * 1024 // 32MB 以上文件开启多核分块并行检索
	defaultChunkSize           = 32 * 1024 * 1024 // 32MB 每个分块
)

// ================= 短期搜索结果 LRU 缓存 =================

type searchCacheKey struct {
	archiveID     string
	filePath      string
	keyword       string
	isRegex       bool
	caseSensitive bool
	wholeWord     bool
	level         string
	contextLines  int
}

type searchCacheItem struct {
	allHits         []model.SearchHit
	fileSummaries   []model.SearchFileSummary
	histogram       []model.TimeHistogramBucket
	extractedTraces []string
	facets          map[string]map[string]int64
	expiresAt       time.Time
}

var (
	queryCacheMu sync.RWMutex
	queryCache   = make(map[searchCacheKey]*searchCacheItem)
)

func getCachedHits(key searchCacheKey) (*searchCacheItem, bool) {
	queryCacheMu.RLock()
	item, found := queryCache[key]
	queryCacheMu.RUnlock()
	if found && time.Now().Before(item.expiresAt) {
		return item, true
	}
	return nil, false
}

func setCachedHits(key searchCacheKey, hits []model.SearchHit, summaries []model.SearchFileSummary, hist []model.TimeHistogramBucket, traces []string, facets map[string]map[string]int64) {
	queryCacheMu.Lock()
	defer queryCacheMu.Unlock()
	// 清理过期或限制大小
	now := time.Now()
	if len(queryCache) > 50 {
		for k, v := range queryCache {
			if now.After(v.expiresAt) {
				delete(queryCache, k)
			}
		}
		if len(queryCache) > 50 {
			for k := range queryCache {
				delete(queryCache, k)
				break
			}
		}
	}
	queryCache[key] = &searchCacheItem{
		allHits:         hits,
		fileSummaries:   summaries,
		histogram:       hist,
		extractedTraces: traces,
		facets:          facets,
		expiresAt:       now.Add(60 * time.Second),
	}
}

// SearchLogs 保持向后兼容的检索入口
func SearchLogs(extractDir string, q *model.SearchQuery) (*model.SearchResponse, error) {
	return SearchLogsContext(context.Background(), extractDir, q)
}

// SearchLogsContext 在目标解压目录下执行高并发、支持级联取消、分块并行加速的日志检索
func SearchLogsContext(ctx context.Context, extractDir string, q *model.SearchQuery) (*model.SearchResponse, error) {
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

	targetLevel := strings.ToUpper(strings.TrimSpace(q.Level))

	// 1. 检查短期结果缓存 (0ms 极速响应翻页与模式切换)
	cacheKey := searchCacheKey{
		archiveID:     q.ArchiveID,
		filePath:      q.FilePath,
		keyword:       q.Keyword,
		isRegex:       q.IsRegex,
		caseSensitive: q.CaseSensitive,
		wholeWord:     q.WholeWord,
		level:         targetLevel,
		contextLines:  q.ContextLines,
	}
	if cachedItem, ok := getCachedHits(cacheKey); ok {
		resp.TotalHits = int64(len(cachedItem.allHits))
		resp.FileSummaries = cachedItem.fileSummaries
		resp.Histogram = cachedItem.histogram
		resp.ExtractedTraces = cachedItem.extractedTraces
		resp.Facets = cachedItem.facets
		offset := (resp.Page - 1) * resp.PageSize
		if offset < len(cachedItem.allHits) {
			end := offset + resp.PageSize
			if end > len(cachedItem.allHits) {
				end = len(cachedItem.allHits)
			}
			resp.Hits = cachedItem.allHits[offset:end]
		}
		resp.CostMS = time.Since(start).Milliseconds()
		return resp, nil
	}

	var reg *regexp.Regexp
	var literalKw []byte
	var err error

	if q.Keyword != "" {
		if !q.IsRegex {
			literalKw = []byte(q.Keyword)
		} else {
			pattern := q.Keyword
			if q.WholeWord {
				pattern = `\b(?:` + pattern + `)\b`
			}
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

	// 2. 快速收集所有满足路径过滤条件的文件任务
	var tasks []searchTask
	_ = filepath.WalkDir(extractDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(extractDir, path)
		if model.IsInternalIndexFile(rel) {
			return nil
		}

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
	fileSummaryMap := make(map[string]*model.SearchFileSummary)
	var summaryMu sync.Mutex

	// 3. 检索各个日志文件（大文件采用多核分块并行；多文件采用文件级并发，每个文件保底配额）
	if len(tasks) == 1 {
		t := tasks[0]
		fi, statErr := os.Stat(t.fullPath)
		if statErr == nil && fi.Size() >= largeFileParallelThreshold {
			allMatchedHits, err = searchInSingleFileParallel(ctx, t.fullPath, t.relPath, fi.Size(), reg, literalKw, q.CaseSensitive, q.IsRegex, q.WholeWord, targetLevel, q.ContextLines, maxCollectedHits)
			if err != nil {
				return nil, err
			}
		} else {
			allMatchedHits = searchInSingleFile(ctx, t.fullPath, t.relPath, reg, literalKw, q.CaseSensitive, q.IsRegex, q.WholeWord, targetLevel, q.ContextLines, maxCollectedHits)
		}
		if len(allMatchedHits) > 0 {
			fileSummaryMap[t.relPath] = &model.SearchFileSummary{
				FilePath:  t.relPath,
				TotalHits: int64(len(allMatchedHits)),
				MaxLevel:  findMaxLevel(allMatchedHits),
			}
		}
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
					if ctx.Err() != nil {
						return
					}
					mu.Lock()
					currentCount := len(allMatchedHits)
					mu.Unlock()
					if currentCount >= maxCollectedHits {
						break
					}

					var hits []model.SearchHit
					fi, statErr := os.Stat(t.fullPath)
					if statErr == nil && fi.Size() >= largeFileParallelThreshold {
						hits, _ = searchInSingleFileParallel(ctx, t.fullPath, t.relPath, fi.Size(), reg, literalKw, q.CaseSensitive, q.IsRegex, q.WholeWord, targetLevel, q.ContextLines, maxPerFileHits)
					} else {
						hits = searchInSingleFile(ctx, t.fullPath, t.relPath, reg, literalKw, q.CaseSensitive, q.IsRegex, q.WholeWord, targetLevel, q.ContextLines, maxPerFileHits)
					}

					if len(hits) > 0 {
						summaryMu.Lock()
						fileSummaryMap[t.relPath] = &model.SearchFileSummary{
							FilePath:  t.relPath,
							TotalHits: int64(len(hits)),
							MaxLevel:  findMaxLevel(hits),
						}
						allMatchedHits = append(allMatchedHits, hits...)
						summaryMu.Unlock()
					}
				}
			}()
		}
		wg.Wait()
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// 汇总各文件统计并排序 (命中数由高到低)
	var fileSummaries []model.SearchFileSummary
	for _, s := range fileSummaryMap {
		fileSummaries = append(fileSummaries, *s)
	}
	sort.Slice(fileSummaries, func(i, j int) bool {
		if fileSummaries[i].TotalHits != fileSummaries[j].TotalHits {
			return fileSummaries[i].TotalHits > fileSummaries[j].TotalHits
		}
		return fileSummaries[i].FilePath < fileSummaries[j].FilePath
	})

	// 4. 构建时序频次直方图、提取 TraceID 与多维分面
	hist, traces, facets := buildSearchHistogramAndFacets(allMatchedHits)

	// 写入短期缓存
	setCachedHits(cacheKey, allMatchedHits, fileSummaries, hist, traces, facets)

	resp.TotalHits = int64(len(allMatchedHits))
	resp.FileSummaries = fileSummaries
	resp.Histogram = hist
	resp.ExtractedTraces = traces
	resp.Facets = facets

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

func parseLogTimeToTime(tsStr string) (time.Time, bool) {
	if tsStr == "" {
		return time.Time{}, false
	}
	layouts := []string{
		"2006-01-02 15:04:05.000000",
		"2006-01-02 15:04:05.000",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05.000000Z07:00",
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05",
		"2006/01/02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, tsStr); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func buildSearchHistogramAndFacets(hits []model.SearchHit) ([]model.TimeHistogramBucket, []string, map[string]map[string]int64) {
	facets := map[string]map[string]int64{
		"level": make(map[string]int64),
		"file":  make(map[string]int64),
	}

	var parsedTimes []time.Time
	type timeHitPair struct {
		t time.Time
		h model.SearchHit
	}
	timeToHitMap := make([]timeHitPair, 0, len(hits))

	traceSet := make(map[string]bool)
	var extractedTraces []string

	for _, h := range hits {
		lvl := strings.ToUpper(h.Level)
		if lvl == "" {
			lvl = "INFO"
		}
		facets["level"][lvl]++
		facets["file"][h.FilePath]++

		if tr := indexer.ExtractTraceID(h.Content); tr != "" && !traceSet[tr] {
			traceSet[tr] = true
			if len(extractedTraces) < 20 {
				extractedTraces = append(extractedTraces, tr)
			}
		}

		if t, ok := parseLogTimeToTime(h.Timestamp); ok {
			parsedTimes = append(parsedTimes, t)
			timeToHitMap = append(timeToHitMap, timeHitPair{t: t, h: h})
		}
	}

	var histogram []model.TimeHistogramBucket
	if len(parsedTimes) > 0 {
		minT := parsedTimes[0]
		maxT := parsedTimes[0]
		for _, t := range parsedTimes {
			if t.Before(minT) {
				minT = t
			}
			if t.After(maxT) {
				maxT = t
			}
		}

		bucketCount := 20
		duration := maxT.Sub(minT)
		if duration <= time.Second {
			b := model.TimeHistogramBucket{
				Timestamp:  minT.Format("2006-01-02 15:04:05"),
				TotalCount: int64(len(parsedTimes)),
			}
			for _, th := range timeToHitMap {
				accumulateBucketLevel(&b, th.h.Level)
			}
			histogram = append(histogram, b)
		} else {
			step := duration / time.Duration(bucketCount)
			if step < time.Second {
				step = time.Second
			}
			buckets := make([]model.TimeHistogramBucket, bucketCount)
			for i := 0; i < bucketCount; i++ {
				bt := minT.Add(time.Duration(i) * step)
				buckets[i] = model.TimeHistogramBucket{
					Timestamp: bt.Format("2006-01-02 15:04:05"),
				}
			}

			for _, th := range timeToHitMap {
				idx := int(th.t.Sub(minT) / step)
				if idx < 0 {
					idx = 0
				} else if idx >= bucketCount {
					idx = bucketCount - 1
				}
				buckets[idx].TotalCount++
				accumulateBucketLevel(&buckets[idx], th.h.Level)
			}
			histogram = buckets
		}
	}

	return histogram, extractedTraces, facets
}

func accumulateBucketLevel(b *model.TimeHistogramBucket, lvl string) {
	switch strings.ToUpper(lvl) {
	case "FATAL", "CRITICAL":
		b.FatalCount++
	case "ERROR":
		b.ErrorCount++
	case "WARN", "WARNING":
		b.WarnCount++
	default:
		b.InfoCount++
	}
}

func findMaxLevel(hits []model.SearchHit) string {
	hasErr := false
	hasWarn := false
	for _, h := range hits {
		lvl := strings.ToUpper(h.Level)
		if lvl == "FATAL" || lvl == "CRITICAL" {
			return "CRITICAL"
		}
		if lvl == "ERROR" {
			hasErr = true
		} else if lvl == "WARN" || lvl == "WARNING" {
			hasWarn = true
		}
	}
	if hasErr {
		return "ERROR"
	}
	if hasWarn {
		return "WARN"
	}
	return "INFO"
}

type pendingSearchHit struct {
	hit       model.SearchHit
	remaining int
}

// ================= 超大单文件多核分块并行检索 =================

type fileChunk struct {
	index     int
	startOff  int64
	endOff    int64
	startLine int64 // 绝对起始行号 (若 > 0 则可直接计算精准绝对行号)
}

type chunkResult struct {
	index      int
	totalLines int64
	hits       []model.SearchHit
	err        error
}

// calculateFileChunks 将大文件按行边界对齐均匀切分成 N 个数据块，耗时 < 1ms
func calculateFileChunks(fullPath string, fileSize int64, numChunks int) ([]fileChunk, error) {
	f, err := os.Open(fullPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	approxChunkSize := fileSize / int64(numChunks)
	rawOffsets := make([]int64, numChunks+1)
	rawOffsets[0] = 0
	rawOffsets[numChunks] = fileSize

	buf := make([]byte, 64*1024)
	for i := 1; i < numChunks; i++ {
		targetOff := int64(i) * approxChunkSize
		if targetOff >= fileSize {
			rawOffsets[i] = fileSize
			continue
		}

		// 从 targetOff 开始寻找首个 '\n'，作为严格对齐的行起始偏移
		currOff := targetOff
		found := false
		for currOff < fileSize {
			n, readErr := f.ReadAt(buf, currOff)
			if n > 0 {
				idx := bytes.IndexByte(buf[:n], '\n')
				if idx != -1 {
					rawOffsets[i] = currOff + int64(idx) + 1
					found = true
					break
				}
				currOff += int64(n)
			}
			if readErr != nil {
				break
			}
		}
		if !found {
			rawOffsets[i] = fileSize
		}
	}

	chunks := make([]fileChunk, 0, numChunks)
	for i := 0; i < numChunks; i++ {
		start := rawOffsets[i]
		end := rawOffsets[i+1]
		if start < end {
			chunks = append(chunks, fileChunk{
				index:    len(chunks),
				startOff: start,
				endOff:   end,
			})
		}
	}
	return chunks, nil
}

// searchChunk 独立扫描单个分块，记录块内相对行号与块内总行数
func searchChunk(ctx context.Context, fullPath, relPath string, chunk fileChunk, reg *regexp.Regexp, literalKw []byte, caseSensitive, isRegex, wholeWord bool, levelFilter string, contextLines int) chunkResult {
	res := chunkResult{index: chunk.index}

	f, err := os.Open(fullPath)
	if err != nil {
		res.err = err
		return res
	}
	defer f.Close()
	adviseSequential(f)

	section := io.NewSectionReader(f, chunk.startOff, chunk.endOff-chunk.startOff)
	scanner := bufio.NewScanner(section)
	bufPtr := searchBufPool.Get().(*[]byte)
	defer searchBufPool.Put(bufPtr)
	scanner.Buffer(*bufPtr, 10*1024*1024)

	var lineNum int64 = 0
	ring := newByteRing(contextLines)
	var pending []*pendingSearchHit

	checkCounter := 0
	for scanner.Scan() {
		checkCounter++
		if checkCounter%1024 == 0 {
			if ctx.Err() != nil {
				res.err = ctx.Err()
				return res
			}
		}

		lineNum++
		lineBytes := scanner.Bytes()

		if len(pending) > 0 {
			lineText := string(lineBytes)
			var activePending []*pendingSearchHit
			for _, p := range pending {
				p.hit.ContextAfter = append(p.hit.ContextAfter, lineText)
				p.remaining--
				if p.remaining <= 0 {
					res.hits = append(res.hits, p.hit)
				} else {
					activePending = append(activePending, p)
				}
			}
			pending = activePending
		}

		lineLevel := detectLogLevelBytes(lineBytes)
		if levelFilter != "" && levelFilter != "ALL" {
			if !matchLogLevel(lineLevel, levelFilter) {
				ring.push(lineBytes)
				continue
			}
		}

		matched := false
		if len(literalKw) == 0 && reg == nil {
			matched = true
		} else if !isRegex {
			if wholeWord {
				matched = bytesContainsWholeWord(lineBytes, literalKw, caseSensitive)
			} else if caseSensitive {
				matched = bytes.Contains(lineBytes, literalKw)
			} else {
				matched = bytesContainsFoldASCII(lineBytes, literalKw)
			}
		} else {
			if len(literalKw) > 0 {
				hasCandidate := false
				if wholeWord {
					hasCandidate = bytesContainsWholeWord(lineBytes, literalKw, caseSensitive)
				} else if caseSensitive {
					hasCandidate = bytes.Contains(lineBytes, literalKw)
				} else {
					hasCandidate = bytesContainsFoldASCII(lineBytes, literalKw)
				}
				if !hasCandidate {
					ring.push(lineBytes)
					continue
				}
			}
			if reg != nil {
				matched = reg.Match(lineBytes)
			}
		}

		if !matched {
			ring.push(lineBytes)
			continue
		}

		lineText := string(lineBytes)
		contextBefore := ring.toStrings()

		newHit := model.SearchHit{
			FilePath:      relPath,
			LineNumber:    lineNum, // 相对本分块的局部行号
			Content:       lineText,
			Level:         lineLevel,
			Timestamp:     extractTimestamp(lineText),
			ContextBefore: contextBefore,
			ContextAfter:  make([]string, 0, contextLines),
		}

		if contextLines == 0 {
			res.hits = append(res.hits, newHit)
		} else {
			pending = append(pending, &pendingSearchHit{
				hit:       newHit,
				remaining: contextLines,
			})
		}

		ring.push(lineBytes)
	}

	for _, p := range pending {
		res.hits = append(res.hits, p.hit)
	}

	res.totalLines = lineNum
	return res
}

// searchInSingleFileParallel 对大文件利用多核进行分块并行检索
func searchInSingleFileParallel(ctx context.Context, fullPath, relPath string, fileSize int64, reg *regexp.Regexp, literalKw []byte, caseSensitive, isRegex, wholeWord bool, levelFilter string, contextLines int, maxHits int) ([]model.SearchHit, error) {
	workerCount := runtime.NumCPU()
	if workerCount < 2 {
		workerCount = 2
	}
	if workerCount > 16 {
		workerCount = 16
	}

	var chunks []fileChunk
	var hasPrecomputedStartLines bool

	// 1. 尝试利用分块布隆稀疏索引执行 Skip-Scan 优化 (跳过 90%~99% 的无关分块与磁盘 I/O)
	bloomIdx, _ := GetOrBuildBloomIndex(fullPath)
	if bloomIdx != nil && len(bloomIdx.Chunks) > 0 {
		hasPrecomputedStartLines = true
		chunks = make([]fileChunk, 0, len(bloomIdx.Chunks))
		for _, bc := range bloomIdx.Chunks {
			if len(literalKw) > 0 && !bc.Filter.MayContainSearchKeyword(literalKw) {
				// 布隆过滤器判定绝对不含该关键字，零磁盘 I/O 直接跳过该分块！
				continue
			}
			chunks = append(chunks, fileChunk{
				index:     int(bc.ChunkIndex),
				startOff:  bc.StartOffset,
				endOff:    bc.EndOffset,
				startLine: bc.StartLine,
			})
		}
	} else {
		// 2. 无布隆索引时，回退至基础等距分块切分
		numChunks := int(fileSize / defaultChunkSize)
		if numChunks < workerCount {
			numChunks = workerCount
		}
		if numChunks > 256 {
			numChunks = 256
		}

		var err error
		chunks, err = calculateFileChunks(fullPath, fileSize, numChunks)
		if err != nil {
			return nil, err
		}
	}

	if len(chunks) == 0 {
		// 整个文件都被布隆过滤器安全跳过，直接返回空结果
		return nil, nil
	}

	activeWorkers := workerCount
	if activeWorkers > len(chunks) {
		activeWorkers = len(chunks)
	}

	type chunkWorkItem struct {
		c    fileChunk
		slot int
	}

	results := make([]chunkResult, len(chunks))
	chunkChan := make(chan chunkWorkItem, len(chunks))
	for slot, c := range chunks {
		chunkChan <- chunkWorkItem{c: c, slot: slot}
	}
	close(chunkChan)

	var wg sync.WaitGroup
	var errOnce sync.Once
	var firstErr error

	for i := 0; i < activeWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range chunkChan {
				if ctx.Err() != nil {
					errOnce.Do(func() { firstErr = ctx.Err() })
					return
				}
				res := searchChunk(ctx, fullPath, relPath, item.c, reg, literalKw, caseSensitive, isRegex, wholeWord, levelFilter, contextLines)
				if res.err != nil && res.err != context.Canceled {
					errOnce.Do(func() { firstErr = res.err })
				}
				results[item.slot] = res
			}
		}()
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// 汇总前缀和并计算精准绝对行号
	var allHits []model.SearchHit
	var currentBaseLine int64 = 1

	for slot, res := range results {
		c := chunks[slot]
		var base int64
		if hasPrecomputedStartLines && c.startLine > 0 {
			base = c.startLine
		} else {
			base = currentBaseLine
		}

		for _, hit := range res.hits {
			hit.LineNumber = base + hit.LineNumber - 1
			allHits = append(allHits, hit)
			if len(allHits) >= maxHits {
				return allHits, nil
			}
		}
		currentBaseLine += res.totalLines
	}

	return allHits, nil
}

// searchInSingleFile 单协程流式搜索单个文件（支持级联取消与系统预读）
func searchInSingleFile(ctx context.Context, fullPath, relPath string, reg *regexp.Regexp, literalKw []byte, caseSensitive, isRegex, wholeWord bool, levelFilter string, contextLines int, maxHits int) []model.SearchHit {
	f, err := os.Open(fullPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	adviseSequential(f)

	scanner := bufio.NewScanner(f)
	bufPtr := searchBufPool.Get().(*[]byte)
	defer searchBufPool.Put(bufPtr)
	scanner.Buffer(*bufPtr, 10*1024*1024)

	var hits []model.SearchHit
	ring := newByteRing(contextLines)
	var pending []*pendingSearchHit

	var lineNum int64 = 0
	checkCounter := 0

	for scanner.Scan() {
		checkCounter++
		if checkCounter%1024 == 0 {
			if ctx.Err() != nil {
				return hits
			}
		}

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
				ring.push(lineBytes)
				continue
			}
		}

		// 3. 检查关键词匹配 (Fast Path 高速字节匹配，避免 Go 正则状态机开销)
		matched := false
		if len(literalKw) == 0 && reg == nil {
			matched = true
		} else if !isRegex {
			if wholeWord {
				matched = bytesContainsWholeWord(lineBytes, literalKw, caseSensitive)
			} else if caseSensitive {
				matched = bytes.Contains(lineBytes, literalKw)
			} else {
				matched = bytesContainsFoldASCII(lineBytes, literalKw)
			}
		} else {
			// 正则模式：若提取出字面量，先用字面量做高速 O(1) 预过滤
			if len(literalKw) > 0 {
				hasCandidate := false
				if wholeWord {
					hasCandidate = bytesContainsWholeWord(lineBytes, literalKw, caseSensitive)
				} else if caseSensitive {
					hasCandidate = bytes.Contains(lineBytes, literalKw)
				} else {
					hasCandidate = bytesContainsFoldASCII(lineBytes, literalKw)
				}
				if !hasCandidate {
					ring.push(lineBytes)
					continue
				}
			}
			if reg != nil {
				matched = reg.Match(lineBytes)
			}
		}

		if !matched {
			ring.push(lineBytes)
			continue
		}

		// 4. 命中：此时才分配 string 构造 SearchHit 实体
		lineText := string(lineBytes)
		contextBefore := ring.toStrings()

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
		ring.push(lineBytes)
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

func isWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

// bytesContainsWholeWord 零内存分配检查 s 中是否包含全词匹配的 substr (支持大小写敏感控制与单词边界 \b)
func bytesContainsWholeWord(s, substr []byte, caseSensitive bool) bool {
	n := len(substr)
	if n == 0 {
		return true
	}
	m := len(s)
	if m < n {
		return false
	}

	c0 := substr[0]
	c0Alt := c0
	if !caseSensitive {
		if c0 >= 'a' && c0 <= 'z' {
			c0Alt = c0 - 32
		} else if c0 >= 'A' && c0 <= 'Z' {
			c0Alt = c0 + 32
		}
	}

	maxI := m - n
	for i := 0; i <= maxI; i++ {
		b := s[i]
		var hitFirst bool
		if caseSensitive {
			hitFirst = (b == c0)
		} else {
			hitFirst = (b == c0 || b == c0Alt)
		}

		if hitFirst {
			// 前边界检查：i == 0 或者前一个字符不是单词字符
			if i > 0 && isWordByte(s[i-1]) {
				continue
			}

			matched := true
			for j := 1; j < n; j++ {
				sb := s[i+j]
				tb := substr[j]
				if caseSensitive {
					if sb != tb {
						matched = false
						break
					}
				} else {
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
			}

			if matched {
				// 后边界检查：i+n == m 或者后一个字符不是单词字符
				if i+n < m && isWordByte(s[i+n]) {
					continue
				}
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

type byteRing struct {
	lines  [][]byte
	head   int
	count  int
	maxCap int
}

func newByteRing(maxCap int) *byteRing {
	if maxCap <= 0 {
		return nil
	}
	r := &byteRing{
		lines:  make([][]byte, maxCap),
		maxCap: maxCap,
	}
	for i := range r.lines {
		r.lines[i] = make([]byte, 0, 256)
	}
	return r
}

func (r *byteRing) push(b []byte) {
	if r == nil || r.maxCap <= 0 {
		return
	}
	var slot int
	if r.count < r.maxCap {
		slot = (r.head + r.count) % r.maxCap
		r.count++
	} else {
		slot = r.head
		r.head = (r.head + 1) % r.maxCap
	}
	r.lines[slot] = append(r.lines[slot][:0], b...)
}

func (r *byteRing) toStrings() []string {
	if r == nil || r.count == 0 {
		return nil
	}
	res := make([]string, r.count)
	for i := 0; i < r.count; i++ {
		idx := (r.head + i) % r.maxCap
		res[i] = string(r.lines[idx])
	}
	return res
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

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func extractTimestamp(line string) string {
	// Fast Path: 绝大多数 Linux 存储日志（Ceph/HDFS/MinIO/syslog）以 ISO 8601 或 YYYY-MM-DD 开头
	if len(line) >= 19 {
		b := line[:19]
		if (b[4] == '-' || b[4] == '/') && (b[7] == '-' || b[7] == '/') &&
			(b[10] == ' ' || b[10] == 'T') && b[13] == ':' && b[16] == ':' &&
			isDigit(b[0]) && isDigit(b[1]) && isDigit(b[2]) && isDigit(b[3]) &&
			isDigit(b[5]) && isDigit(b[6]) && isDigit(b[8]) && isDigit(b[9]) &&
			isDigit(b[11]) && isDigit(b[12]) && isDigit(b[14]) && isDigit(b[15]) &&
			isDigit(b[17]) && isDigit(b[18]) {
			end := 19
			if len(line) > 20 && line[19] == '.' {
				end = 20
				for end < len(line) && isDigit(line[end]) {
					end++
				}
			}
			return line[:end]
		}
	}
	for _, reg := range timeRegexList {
		if match := reg.FindString(line); match != "" {
			return match
		}
	}
	return ""
}

