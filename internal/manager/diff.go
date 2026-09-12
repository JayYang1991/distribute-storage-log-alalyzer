package manager

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"dist-log-analyzer/internal/indexer"
	"dist-log-analyzer/internal/model"
)

// DetectSubArchives 智能探测归档解压目录中包含的内部子压缩包/独立子节点模块 (支持穿透单层顶层包装目录)
func DetectSubArchives(extractPath string) ([]model.SubArchiveItem, error) {
	if extractPath == "" {
		return nil, fmt.Errorf("解压路径为空")
	}
	if _, err := os.Stat(extractPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("归档解压目录不存在: %s", extractPath)
	}

	entries, err := os.ReadDir(extractPath)
	if err != nil {
		return nil, fmt.Errorf("读取解压目录失败: %v", err)
	}

	// 过滤有效目录
	var dirEntries []fs.DirEntry
	for _, e := range entries {
		if e.IsDir() && !model.IsInternalIndexFile(e.Name()) && !strings.HasPrefix(e.Name(), ".") {
			dirEntries = append(dirEntries, e)
		}
	}

	searchBaseDir := extractPath
	searchPrefix := ""

	// 若顶层仅有 1 个单一包装目录（如打包时多包了一层 cluster-dir/），智能穿透下钻一层
	if len(dirEntries) == 1 {
		singleSub := filepath.Join(extractPath, dirEntries[0].Name())
		subEntries, sErr := os.ReadDir(singleSub)
		if sErr == nil {
			var subDirs []fs.DirEntry
			for _, se := range subEntries {
				if se.IsDir() && !model.IsInternalIndexFile(se.Name()) && !strings.HasPrefix(se.Name(), ".") {
					subDirs = append(subDirs, se)
				}
			}
			if len(subDirs) >= 2 {
				// 穿透生效
				searchBaseDir = singleSub
				searchPrefix = dirEntries[0].Name()
				dirEntries = subDirs
			}
		}
	}

	var subArchives []model.SubArchiveItem
	for _, de := range dirEntries {
		subName := de.Name()
		subRelPath := subName
		if searchPrefix != "" {
			subRelPath = filepath.Join(searchPrefix, subName)
		}
		subFullPath := filepath.Join(searchBaseDir, subName)

		var totalFiles int
		var totalSize int64
		var hasNested bool

		_ = filepath.WalkDir(subFullPath, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if path != subFullPath {
					hasNested = true
				}
				return nil
			}
			rel, _ := filepath.Rel(subFullPath, path)
			if model.IsInternalIndexFile(rel) {
				return nil
			}
			totalFiles++
			if info, sErr := d.Info(); sErr == nil {
				totalSize += info.Size()
			}
			return nil
		})

		if totalFiles > 0 {
			subArchives = append(subArchives, model.SubArchiveItem{
				Name:       subName,
				Path:       subRelPath,
				TotalFiles: totalFiles,
				TotalSize:  totalSize,
				HasNested:  hasNested,
			})
		}
	}

	sort.Slice(subArchives, func(i, j int) bool {
		return subArchives[i].Name < subArchives[j].Name
	})

	return subArchives, nil
}

// CompareArchives 向前兼容的归档全包差分比对入口
func (s *Server) CompareArchives(archiveIDA, archiveIDB string) (*model.DiffReport, error) {
	return s.CompareArchiveScopes(archiveIDA, "", archiveIDB, "")
}

// CompareArchiveScopes 统一的全维基准差分对比引擎
// 支持两种核心模式：
// 1. 跨归档包差分对比：archiveIDA != archiveIDB
// 2. 同一归档包内多个子压缩包间对比：archiveIDA == archiveIDB 且 subPathA != subPathB
func (s *Server) CompareArchiveScopes(archiveIDA, subPathA, archiveIDB, subPathB string) (*model.DiffReport, error) {
	arcA, err := s.store.GetArchive(archiveIDA)
	if err != nil || arcA == nil {
		return nil, fmt.Errorf("基准日志包不存在: %s", archiveIDA)
	}
	arcB, err := s.store.GetArchive(archiveIDB)
	if err != nil || arcB == nil {
		return nil, fmt.Errorf("对比目标日志包不存在: %s", archiveIDB)
	}

	// 校验同包同子路径
	isIntraArchive := archiveIDA == archiveIDB
	if isIntraArchive && subPathA == subPathB {
		return nil, fmt.Errorf("同包内差分比对必须选择两个不同的子压缩包/模块路径")
	}

	scopeType := "inter_archive"
	if isIntraArchive {
		scopeType = "intra_archive"
	}

	nameA := arcA.Filename
	if subPathA != "" {
		nameA = fmt.Sprintf("%s [%s]", arcA.Filename, subPathA)
	}
	nameB := arcB.Filename
	if subPathB != "" {
		nameB = fmt.Sprintf("%s [%s]", arcB.Filename, subPathB)
	}

	report := &model.DiffReport{
		ArchiveIDA:   archiveIDA,
		ArchiveNameA: nameA,
		SubPathA:     subPathA,
		ArchiveIDB:   archiveIDB,
		ArchiveNameB: nameB,
		SubPathB:     subPathB,
		ScopeType:    scopeType,
		SeverityA:    make(map[string]int),
		SeverityB:    make(map[string]int),
		AnalyzedAt:   time.Now(),
	}

	// 1. 递归收集并归一化对齐两作用域内的所有解压文件 (自动穿透多层嵌套压缩包和深层目录)
	normFilesA, rawFilesA := collectScopedFiles(arcA.ExtractPath, subPathA)
	normFilesB, rawFilesB := collectScopedFiles(arcB.ExtractPath, subPathB)

	report.TotalFilesA = len(normFilesA)
	report.TotalFilesB = len(normFilesB)

	for normB := range normFilesB {
		if _, exists := normFilesA[normB]; !exists {
			report.AddedFiles = append(report.AddedFiles, normB)
		}
	}
	for normA := range normFilesA {
		if _, exists := normFilesB[normA]; !exists {
			report.RemovedFiles = append(report.RemovedFiles, normA)
		}
	}
	sort.Strings(report.AddedFiles)
	sort.Strings(report.RemovedFiles)

	// 2. 对比作用域内的诊断告警与严重事件
	diagA, _ := s.store.GetReport(archiveIDA)
	diagB, _ := s.store.GetReport(archiveIDB)

	eventsAKeys := make(map[string]bool)
	if diagA != nil {
		for _, ev := range diagA.Events {
			if isEventInScope(ev.FilePath, subPathA, rawFilesA) {
				report.TotalEventsA++
				report.SeverityA[string(ev.Severity)]++
				// 归一化 key: RuleName + 归一化路径
				normPath := normalizeEventPath(ev.FilePath, subPathA)
				eventsAKeys[fmt.Sprintf("%s:%s", ev.RuleName, normPath)] = true
			}
		}
	}

	if diagB != nil {
		for _, ev := range diagB.Events {
			if isEventInScope(ev.FilePath, subPathB, rawFilesB) {
				report.TotalEventsB++
				report.SeverityB[string(ev.Severity)]++
				normPath := normalizeEventPath(ev.FilePath, subPathB)
				key := fmt.Sprintf("%s:%s", ev.RuleName, normPath)
				if !eventsAKeys[key] {
					if ev.Severity == model.SeverityFatal || ev.Severity == model.SeverityCritical {
						scopedEv := ev
						scopedEv.FilePath = normPath // 呈现归一化相对路径，更易辨识
						report.NewFatalEvents = append(report.NewFatalEvents, scopedEv)
					}
				}
			}
		}
	}

	// 3. 基于 Drain 算法挖掘 B 相对 A 全新突发异质日志模式 (Zero-Shot Patterns)
	minerA := indexer.NewDrainMiner(0.55, 4)
	minerB := indexer.NewDrainMiner(0.55, 4)

	mineScopedArchiveSamples(arcA.ExtractPath, subPathA, minerA, 1000)
	mineScopedArchiveSamples(arcB.ExtractPath, subPathB, minerB, 1000)

	tplsA := minerA.GetTemplates()
	patternsA := make(map[string]bool)
	for _, tpl := range tplsA {
		patternsA[tpl.Pattern] = true
	}

	tplsB := minerB.GetTemplates()
	for _, tpl := range tplsB {
		if !patternsA[tpl.Pattern] {
			if tpl.Level == "ERROR" || tpl.Level == "FATAL" || tpl.Count >= 2 {
				// 转换为相对于归档包解压根目录的相对路径，并附带对应归档包 ID，便于前端日志查看器精准跳转直达
				if arcB.ExtractPath != "" && tpl.SampleFile != "" {
					if rel, err := filepath.Rel(arcB.ExtractPath, tpl.SampleFile); err == nil {
						tpl.SampleFile = rel
					}
				}
				tpl.ArchiveID = arcB.ID
				report.NewTemplates = append(report.NewTemplates, tpl)
			}
		}
	}
	if len(report.NewTemplates) > 15 {
		report.NewTemplates = report.NewTemplates[:15]
	}

	// 4. 自动生成专家级智能差分诊断结论
	diffFatal := report.SeverityB["FATAL"] - report.SeverityA["FATAL"]
	diffCritical := report.SeverityB["CRITICAL"] - report.SeverityA["CRITICAL"]
	var summary strings.Builder

	if isIntraArchive {
		summary.WriteString(fmt.Sprintf("对比完成：在归档包 [%s] 内部对子模块/子包 [%s] 与 [%s] 执行控制变量差分诊断。\n",
			arcA.Filename, subPathA, subPathB))
		summary.WriteString(fmt.Sprintf("• 基准子包 [%s] 共有 %d 个文件、%d 起检出事件；\n• 待测子包 [%s] 共有 %d 个文件、%d 起检出事件。\n",
			subPathA, report.TotalFilesA, report.TotalEventsA,
			subPathB, report.TotalFilesB, report.TotalEventsB))
	} else {
		summary.WriteString(fmt.Sprintf("对比完成：基准包 [%s] 共有 %d 个文件、%d 起检出事件；待测包 [%s] 共有 %d 个文件、%d 起检出事件。\n",
			report.ArchiveNameA, report.TotalFilesA, report.TotalEventsA,
			report.ArchiveNameB, report.TotalFilesB, report.TotalEventsB))
	}

	if diffFatal > 0 || diffCritical > 0 {
		summary.WriteString(fmt.Sprintf("⚠️ 告警变化：待测目标较基准新增 %d 起致命故障 (FATAL) 与 %d 起严重事件 (CRITICAL)！\n", diffFatal, diffCritical))
	} else {
		summary.WriteString("✔ 告警变化：待测目标未检出新增致命级别故障。\n")
	}

	if len(report.NewTemplates) > 0 {
		summary.WriteString(fmt.Sprintf("🔍 突增模式：检出 %d 个仅在待测目标中突发涌现的异质日志模式（疑似导致异常或版本漂移的关键特征）。", len(report.NewTemplates)))
	}

	report.SummaryText = summary.String()
	return report, nil
}

// collectScopedFiles 收集指定子路径范围内的所有解压文件
// 返回:
// 1. normMap: 键为归一化路径 (去掉 subPath 前缀，例如 "logs/ceph.log")
// 2. rawMap: 键为在全包中的完整相对路径 (例如 "node-01/logs/ceph.log")
func collectScopedFiles(extractDir, subPath string) (map[string]int64, map[string]int64) {
	normMap := make(map[string]int64)
	rawMap := make(map[string]int64)
	if extractDir == "" {
		return normMap, rawMap
	}

	targetDir := extractDir
	if subPath != "" {
		targetDir = filepath.Join(extractDir, subPath)
	}

	if _, err := os.Stat(targetDir); os.IsNotExist(err) {
		return normMap, rawMap
	}

	_ = filepath.WalkDir(targetDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		// 相对 targetDir 的归一化路径
		normRel, nErr := filepath.Rel(targetDir, path)
		if nErr != nil || model.IsInternalIndexFile(normRel) {
			return nil
		}
		// 相对 extractDir 的全包相对路径
		fullRel, fErr := filepath.Rel(extractDir, path)
		if fErr != nil || model.IsInternalIndexFile(fullRel) {
			return nil
		}

		fi, sErr := os.Stat(path)
		if sErr == nil {
			normMap[normRel] = fi.Size()
			rawMap[fullRel] = fi.Size()
		}
		return nil
	})

	return normMap, rawMap
}

func mineArchiveSamples(extractDir string, miner *indexer.DrainMiner, maxLinesPerFile int) {
	mineScopedArchiveSamples(extractDir, "", miner, maxLinesPerFile)
}

func mineScopedArchiveSamples(extractDir, subPath string, miner *indexer.DrainMiner, maxLinesPerFile int) {
	if extractDir == "" {
		return
	}
	targetDir := extractDir
	if subPath != "" {
		targetDir = filepath.Join(extractDir, subPath)
	}

	if _, err := os.Stat(targetDir); os.IsNotExist(err) {
		return
	}

	var fileList []string
	_ = filepath.WalkDir(targetDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(targetDir, path)
		if model.IsInternalIndexFile(rel) {
			return nil
		}
		fileList = append(fileList, path)
		return nil
	})

	if len(fileList) == 0 {
		return
	}

	// 依据 CPU 核数启动 Worker Pool 多核并行挖掘样本，显著缩短差分分析耗时
	workerCount := runtime.NumCPU()
	if workerCount > len(fileList) {
		workerCount = len(fileList)
	}
	if workerCount > 16 {
		workerCount = 16
	}
	if workerCount < 1 {
		workerCount = 1
	}

	taskChan := make(chan string, len(fileList))
	for _, fp := range fileList {
		taskChan <- fp
	}
	close(taskChan)

	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for fp := range taskChan {
				_ = miner.MineFile(fp, maxLinesPerFile)
			}
		}()
	}
	wg.Wait()
}

func isEventInScope(eventFilePath, subPath string, rawFiles map[string]int64) bool {
	if subPath == "" {
		return true
	}
	if _, exists := rawFiles[eventFilePath]; exists {
		return true
	}
	cleanSub := filepath.Clean(subPath)
	cleanEvent := filepath.Clean(eventFilePath)
	return cleanEvent == cleanSub || strings.HasPrefix(cleanEvent, cleanSub+string(filepath.Separator))
}

func normalizeEventPath(eventFilePath, subPath string) string {
	if subPath == "" {
		return eventFilePath
	}
	cleanSub := filepath.Clean(subPath)
	cleanEvent := filepath.Clean(eventFilePath)
	if strings.HasPrefix(cleanEvent, cleanSub+string(filepath.Separator)) {
		return strings.TrimPrefix(cleanEvent, cleanSub+string(filepath.Separator))
	}
	return filepath.Base(eventFilePath)
}
