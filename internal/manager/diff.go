package manager

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"dist-log-analyzer/internal/indexer"
	"dist-log-analyzer/internal/model"
)

// CompareArchives 对比两个归档日志包（arcA 为基准包，arcB 为待测/故障包），输出全维基准差分对比报告
func (s *Server) CompareArchives(archiveIDA, archiveIDB string) (*model.DiffReport, error) {
	arcA, err := s.store.GetArchive(archiveIDA)
	if err != nil || arcA == nil {
		return nil, fmt.Errorf("基准日志包不存在: %s", archiveIDA)
	}
	arcB, err := s.store.GetArchive(archiveIDB)
	if err != nil || arcB == nil {
		return nil, fmt.Errorf("对比目标日志包不存在: %s", archiveIDB)
	}

	report := &model.DiffReport{
		ArchiveIDA:   archiveIDA,
		ArchiveNameA: arcA.Filename,
		ArchiveIDB:   archiveIDB,
		ArchiveNameB: arcB.Filename,
		SeverityA:    make(map[string]int),
		SeverityB:    make(map[string]int),
		AnalyzedAt:   time.Now(),
	}

	// 1. 对比解压文件列表差异
	filesMapA := collectArchiveFiles(arcA.ExtractPath)
	filesMapB := collectArchiveFiles(arcB.ExtractPath)
	report.TotalFilesA = len(filesMapA)
	report.TotalFilesB = len(filesMapB)

	for pathB := range filesMapB {
		if _, exists := filesMapA[pathB]; !exists {
			report.AddedFiles = append(report.AddedFiles, pathB)
		}
	}
	for pathA := range filesMapA {
		if _, exists := filesMapB[pathA]; !exists {
			report.RemovedFiles = append(report.RemovedFiles, pathA)
		}
	}
	sort.Strings(report.AddedFiles)
	sort.Strings(report.RemovedFiles)

	// 2. 对比诊断报告与严重故障事件
	diagA, _ := s.store.GetReport(archiveIDA)
	diagB, _ := s.store.GetReport(archiveIDB)

	eventsAKeys := make(map[string]bool)
	if diagA != nil {
		report.TotalEventsA = diagA.TotalEvents
		for k, v := range diagA.SeveritySummary {
			report.SeverityA[k] = v
		}
		for _, ev := range diagA.Events {
			key := fmt.Sprintf("%s:%s", ev.RuleName, ev.FilePath)
			eventsAKeys[key] = true
		}
	}

	if diagB != nil {
		report.TotalEventsB = diagB.TotalEvents
		for k, v := range diagB.SeveritySummary {
			report.SeverityB[k] = v
		}
		for _, ev := range diagB.Events {
			key := fmt.Sprintf("%s:%s", ev.RuleName, ev.FilePath)
			if !eventsAKeys[key] {
				// 仅在 B 中发生的新增事件
				if ev.Severity == model.SeverityFatal || ev.Severity == model.SeverityCritical {
					report.NewFatalEvents = append(report.NewFatalEvents, ev)
				}
			}
		}
	}

	// 3. 利用 Drain 算法挖掘 B 相对 A 全新涌现的异常日志模式 (Zero-Shot Anomaly Patterns)
	minerA := indexer.NewDrainMiner(0.55, 4)
	minerB := indexer.NewDrainMiner(0.55, 4)

	mineArchiveSamples(arcA.ExtractPath, minerA, 1000)
	mineArchiveSamples(arcB.ExtractPath, minerB, 1000)

	tplsA := minerA.GetTemplates()
	patternsA := make(map[string]bool)
	for _, tpl := range tplsA {
		patternsA[tpl.Pattern] = true
	}

	tplsB := minerB.GetTemplates()
	for _, tpl := range tplsB {
		if !patternsA[tpl.Pattern] {
			// 该模板在基准包 A 中从未出现
			if tpl.Level == "ERROR" || tpl.Level == "FATAL" || tpl.Count >= 2 {
				report.NewTemplates = append(report.NewTemplates, tpl)
			}
		}
	}
	if len(report.NewTemplates) > 15 {
		report.NewTemplates = report.NewTemplates[:15]
	}

	// 4. 自动生成综合结论
	diffFatal := report.SeverityB["FATAL"] - report.SeverityA["FATAL"]
	diffCritical := report.SeverityB["CRITICAL"] - report.SeverityA["CRITICAL"]
	var summary strings.Builder
	summary.WriteString(fmt.Sprintf("对比完成：基准包 [%s] 共有 %d 个文件、%d 起异常事件；待测包 [%s] 共有 %d 个文件、%d 起异常事件。\n",
		arcA.Filename, report.TotalFilesA, report.TotalEventsA,
		arcB.Filename, report.TotalFilesB, report.TotalEventsB))

	if diffFatal > 0 || diffCritical > 0 {
		summary.WriteString(fmt.Sprintf("⚠️ 告警变化：待测包较基准包新增 %d 起致命错误 (FATAL) 与 %d 起严重事件 (CRITICAL)！\n", diffFatal, diffCritical))
	} else {
		summary.WriteString("✔ 告警变化：待测包未见新增致命级别故障。\n")
	}

	if len(report.NewTemplates) > 0 {
		summary.WriteString(fmt.Sprintf("🔍 模式差分：检测到 %d 个仅在待测包中突增/新增的全新日志模式（可能是引发故障或版本变更的根本诱因）。", len(report.NewTemplates)))
	}

	report.SummaryText = summary.String()
	return report, nil
}

func collectArchiveFiles(extractDir string) map[string]int64 {
	res := make(map[string]int64)
	if extractDir == "" {
		return res
	}
	_ = filepath.WalkDir(extractDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(extractDir, path)
		if model.IsInternalIndexFile(rel) {
			return nil
		}
		fi, sErr := os.Stat(path)
		if sErr == nil {
			res[rel] = fi.Size()
		}
		return nil
	})
	return res
}

func mineArchiveSamples(extractDir string, miner *indexer.DrainMiner, maxLinesPerFile int) {
	if extractDir == "" {
		return
	}
	files := collectArchiveFiles(extractDir)
	for rel := range files {
		fullPath := filepath.Join(extractDir, rel)
		_ = miner.MineFile(fullPath, maxLinesPerFile)
	}
}
