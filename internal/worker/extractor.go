package worker

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/bzip2"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dist-log-analyzer/internal/model"

	"github.com/bodgit/sevenzip"
	"github.com/klauspost/compress/gzip"
)

var extractBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 2*1024*1024) // 2MB 大缓冲区，提升超大日志包解压写盘吞吐
		return &b
	},
}

// 动态自适应写缓冲区对象池，避免为数千个小文件重复分配 2MB 堆内存
var smallWriterPool = sync.Pool{
	New: func() interface{} {
		return bufio.NewWriterSize(nil, 64*1024) // 64KB 适配中小型日志
	},
}

var largeWriterPool = sync.Pool{
	New: func() interface{} {
		return bufio.NewWriterSize(nil, 1*1024*1024) // 1MB 适配超大日志文件
	},
}

func getBufioWriter(w io.Writer, approxSize int64) (*bufio.Writer, func()) {
	if approxSize > 0 && approxSize < 256*1024 {
		bw := smallWriterPool.Get().(*bufio.Writer)
		bw.Reset(w)
		return bw, func() {
			_ = bw.Flush()
			bw.Reset(nil)
			smallWriterPool.Put(bw)
		}
	}
	bw := largeWriterPool.Get().(*bufio.Writer)
	bw.Reset(w)
	return bw, func() {
		_ = bw.Flush()
		bw.Reset(nil)
		largeWriterPool.Put(bw)
	}
}

// lineCountingWriter 包装 io.Writer，在数据写入磁盘的同时统计换行符数量，实现零二次磁盘 I/O
type lineCountingWriter struct {
	w     io.Writer
	lines int64
}

func (lw *lineCountingWriter) Write(p []byte) (n int, err error) {
	n, err = lw.w.Write(p)
	if n > 0 {
		lw.lines += int64(bytes.Count(p[:n], []byte{'\n'}))
	}
	return n, err
}

type archiveType int

const (
	archNone archiveType = iota
	archTarGz
	archTarBz2
	archTar
	archZip
	arch7z
	archSingleGz
	archSingleBz2
)

func getArchiveType(filePath string) archiveType {
	lower := strings.ToLower(filePath)
	switch {
	case strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz"):
		return archTarGz
	case strings.HasSuffix(lower, ".tar.bz2") || strings.HasSuffix(lower, ".tbz2"):
		return archTarBz2
	case strings.HasSuffix(lower, ".tar"):
		return archTar
	case strings.HasSuffix(lower, ".zip"):
		return archZip
	case strings.HasSuffix(lower, ".7z"):
		return arch7z
	case strings.HasSuffix(lower, ".gz"):
		return archSingleGz
	case strings.HasSuffix(lower, ".bz2"):
		return archSingleBz2
	default:
		return archNone
	}
}

func getCleanArchiveBase(fileName string, aType archiveType) string {
	lower := strings.ToLower(fileName)
	switch aType {
	case archTarGz:
		if strings.HasSuffix(lower, ".tar.gz") {
			return fileName[:len(fileName)-7]
		}
		return strings.TrimSuffix(fileName, ".tgz")
	case archTarBz2:
		if strings.HasSuffix(lower, ".tar.bz2") {
			return fileName[:len(fileName)-8]
		}
		return strings.TrimSuffix(fileName, ".tbz2")
	case archTar:
		return strings.TrimSuffix(fileName, ".tar")
	case archZip:
		return strings.TrimSuffix(fileName, ".zip")
	case arch7z:
		return strings.TrimSuffix(fileName, ".7z")
	case archSingleGz:
		return strings.TrimSuffix(fileName, ".gz")
	case archSingleBz2:
		return strings.TrimSuffix(fileName, ".bz2")
	default:
		return fileName
	}
}

func extractOneArchive(aType archiveType, src, dest string, lineMap map[string]int64) error {
	switch aType {
	case archTarGz:
		return extractTarGz(src, dest, lineMap)
	case archZip:
		return extractZip(src, dest, lineMap)
	case arch7z:
		return extract7z(src, dest, lineMap)
	case archTarBz2:
		return extractTarBz2(src, dest, lineMap)
	case archTar:
		return extractTar(src, dest, lineMap)
	case archSingleGz:
		return extractSingleGz(src, dest, lineMap)
	case archSingleBz2:
		return extractSingleBz2(src, dest, lineMap)
	default:
		destFile := filepath.Join(dest, filepath.Base(src))
		return copyFile(src, destFile, lineMap)
	}
}

func moveDirContents(srcDir, dstDir string) error {
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return err
	}
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		s := filepath.Join(srcDir, entry.Name())
		d := filepath.Join(dstDir, entry.Name())
		if entry.IsDir() {
			if err := moveDirContents(s, d); err != nil {
				return err
			}
			_ = os.Remove(s)
		} else {
			_ = os.Remove(d) // 覆盖已有同名文件
			if err := os.Rename(s, d); err != nil {
				if copyErr := copyFile(s, d, nil); copyErr != nil {
					return copyErr
				}
				_ = os.Remove(s)
			}
		}
	}
	return nil
}

var (
	reDmesgUptime = regexp.MustCompile(`^\[\s*(\d+(?:\.\d+)?)\]`)
	reIsoTime     = regexp.MustCompile(`(\d{4}[-/]\d{2}[-/]\d{2}[ T]\d{2}:\d{2}:\d{2}(?:\.\d+)?)`)
	reSyslogTime  = regexp.MustCompile(`^(?:<\d+>)?([A-Z][a-z]{2}\s+\d+\s+\d{2}:\d{2}:\d{2})`)
)

var syslogMonthMap = map[string]int{
	"Jan": 1, "Feb": 2, "Mar": 3, "Apr": 4, "May": 5, "Jun": 6,
	"Jul": 7, "Aug": 8, "Sep": 9, "Oct": 10, "Nov": 11, "Dec": 12,
}

// getLogPrefixBeforeDot 获取文件名第一个 . 之前的前缀 (如 dmesg.1.gz -> dmesg, syslog.2 -> syslog)
func getLogPrefixBeforeDot(fileName string) string {
	if idx := strings.Index(fileName, "."); idx != -1 {
		return fileName[:idx]
	}
	return fileName
}

func parseLogFirstLineTime(line string) (float64, bool) {
	clean := strings.TrimSpace(line)
	if clean == "" {
		return 0, false
	}

	// 1. 内核/dmesg 开机秒数: [    0.000000] 或 [ 1234.567890]
	if m := reDmesgUptime.FindStringSubmatch(clean); len(m) > 1 {
		if sec, err := strconv.ParseFloat(m[1], 64); err == nil {
			return sec, true
		}
	}

	// 2. ISO 8601 / RFC3339: 2026-09-13 01:08:09 或 2026-09-13T01:08:09
	if m := reIsoTime.FindStringSubmatch(clean); len(m) > 1 {
		timeStr := strings.Replace(m[1], "/", "-", -1)
		timeStr = strings.Replace(timeStr, "T", " ", -1)
		formats := []string{
			"2006-01-02 15:04:05.999999999",
			"2006-01-02 15:04:05.999",
			"2006-01-02 15:04:05",
		}
		for _, layout := range formats {
			if t, err := time.Parse(layout, timeStr); err == nil {
				return float64(t.UnixNano()) / 1e9, true
			}
		}
	}

	// 3. 传统 Syslog: Sep 13 01:08:09 或 <34>Sep 13 01:08:09
	if m := reSyslogTime.FindStringSubmatch(clean); len(m) > 1 {
		parts := strings.Fields(m[1])
		if len(parts) >= 3 {
			monthStr := parts[0]
			dayStr := parts[1]
			hmsStr := parts[2]
			if month, ok := syslogMonthMap[monthStr]; ok {
				day, _ := strconv.Atoi(dayStr)
				hmsParts := strings.Split(hmsStr, ":")
				if len(hmsParts) == 3 {
					h, _ := strconv.Atoi(hmsParts[0])
					min, _ := strconv.Atoi(hmsParts[1])
					sec, _ := strconv.Atoi(hmsParts[2])
					totalSeconds := float64(month)*31*86400 + float64(day)*86400 + float64(h)*3600 + float64(min)*60 + float64(sec)
					return totalSeconds, true
				}
			}
		}
	}

	return 0, false
}

func fallbackLogScore(filePath string) float64 {
	base := filepath.Base(filePath)
	ext := filepath.Ext(base)
	if ext != "" && len(ext) > 1 {
		numStr := ext[1:]
		if num, err := strconv.Atoi(numStr); err == nil {
			// logrotate 规范中：数字越大代表轮转越早，数值越小排序越靠前
			return -float64(num)
		}
	}
	// 没有数字后缀（如活跃日志 dmesg），时间最晚
	return 1000.0
}

func extractLogStartTime(filePath string) float64 {
	f, err := os.Open(filePath)
	if err != nil {
		return fallbackLogScore(filePath)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	linesRead := 0
	for scanner.Scan() && linesRead < 5 {
		line := scanner.Text()
		linesRead++
		if score, ok := parseLogFirstLineTime(line); ok {
			return score
		}
	}
	return fallbackLogScore(filePath)
}

func mergeLogFilesIntoOne(targetFile string, srcFiles []string, lineMap map[string]int64) error {
	tmpFile := targetFile + fmt.Sprintf(".tmp_merge_%d", time.Now().UnixNano())
	out, err := os.Create(tmpFile)
	if err != nil {
		return err
	}

	bw := bufio.NewWriterSize(out, 1*1024*1024)
	buf := make([]byte, 1*1024*1024)
	var totalLines int64
	var lastByte byte = '\n'

	for _, src := range srcFiles {
		in, err := os.Open(src)
		if err != nil {
			log.Printf("[Extractor] 打开待合并文件失败 %s: %v", src, err)
			continue
		}

		// 若前一个文件末尾缺少换行符，自动补齐换行，防止首尾行粘连
		if lastByte != '\n' {
			_ = bw.WriteByte('\n')
			totalLines++
			lastByte = '\n'
		}

		for {
			n, rErr := in.Read(buf)
			if n > 0 {
				_, _ = bw.Write(buf[:n])
				totalLines += int64(bytes.Count(buf[:n], []byte{'\n'}))
				lastByte = buf[n-1]
			}
			if rErr != nil {
				break
			}
		}
		_ = in.Close()
	}

	if lastByte != '\n' {
		_ = bw.WriteByte('\n')
		totalLines++
	}

	_ = bw.Flush()
	_ = out.Close()

	// 原子替换目标文件
	_ = os.Remove(targetFile)
	if err := os.Rename(tmpFile, targetFile); err != nil {
		_ = os.Remove(tmpFile)
		return err
	}

	// 安全删除参与合并的原始碎片文件
	for _, src := range srcFiles {
		if src != targetFile {
			_ = os.Remove(src)
			if lineMap != nil {
				delete(lineMap, src)
			}
		}
	}

	if lineMap != nil {
		lineMap[targetFile] = totalLines
	}
	return nil
}

func processSingleArchivesInDir(dir string, lineMap map[string]int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	// 1. 扫描当前目录下所有单文件压缩包 (.gz / .bz2)，按第一个 . 之前的前缀分组
	prefixArchives := make(map[string][]string)
	for _, entry := range entries {
		if entry.IsDir() || model.IsInternalIndexFile(entry.Name()) {
			continue
		}
		fullPath := filepath.Join(dir, entry.Name())
		aType := getArchiveType(fullPath)
		if aType == archSingleGz || aType == archSingleBz2 {
			prefix := getLogPrefixBeforeDot(entry.Name())
			prefixArchives[prefix] = append(prefixArchives[prefix], fullPath)
		}
	}

	if len(prefixArchives) == 0 {
		return
	}

	// 2. 针对每个前缀：创建同名专属目录、移动同前缀普通文件、解压单压缩包并流式合并
	for prefix, archPaths := range prefixArchives {
		destDir := filepath.Join(dir, prefix)

		// 规避文件系统同名冲突：若当前已存在名为 prefix 的普通文件，先暂存重命名
		var origFileTempPath string
		if fi, statErr := os.Lstat(destDir); statErr == nil && !fi.IsDir() {
			origFileTempPath = filepath.Join(dir, fmt.Sprintf(".tmp_orig_%s_%d", prefix, time.Now().UnixNano()))
			if renameErr := os.Rename(destDir, origFileTempPath); renameErr != nil {
				log.Printf("[Extractor] 暂存重命名同名文件失败 %s: %v", destDir, renameErr)
			}
		}

		if err := os.MkdirAll(destDir, 0755); err != nil {
			log.Printf("[Extractor] 创建前缀专属目录失败 %s: %v", destDir, err)
			if origFileTempPath != "" {
				_ = os.Rename(origFileTempPath, destDir)
			}
			continue
		}

		if origFileTempPath != "" {
			_ = os.Rename(origFileTempPath, filepath.Join(destDir, prefix))
		}

		// 规则 2: 将父目录下所有属于该前缀的未压缩普通文件 (如 dmesg.0, syslog.1 等) 移动到 destDir
		curEntries, _ := os.ReadDir(dir)
		for _, e := range curEntries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") || model.IsInternalIndexFile(e.Name()) {
				continue
			}
			fullSrc := filepath.Join(dir, e.Name())
			aType := getArchiveType(fullSrc)
			if aType == archSingleGz || aType == archSingleBz2 {
				continue
			}
			if getLogPrefixBeforeDot(e.Name()) == prefix {
				destFile := filepath.Join(destDir, e.Name())
				if err := os.Rename(fullSrc, destFile); err != nil {
					_ = copyFile(fullSrc, destFile, lineMap)
					_ = os.Remove(fullSrc)
				}
				if lineMap != nil {
					lineMap[destFile] = lineMap[fullSrc]
					delete(lineMap, fullSrc)
				}
			}
		}

		// 规则 1: 将该前缀对应的单文件压缩包解压到 destDir 目录下
		for _, archPath := range archPaths {
			aType := getArchiveType(archPath)
			fName := filepath.Base(archPath)
			cleanBase := getCleanArchiveBase(fName, aType)
			targetFile := filepath.Join(destDir, cleanBase)
			if targetFile == archPath {
				targetFile = targetFile + ".out"
			}

			var extractErr error
			if aType == archSingleGz {
				extractErr = extractSingleGz(archPath, destDir, lineMap)
			} else {
				extractErr = extractSingleBz2(archPath, destDir, lineMap)
			}
			if extractErr == nil {
				_ = os.Remove(archPath)
				if lineMap != nil {
					delete(lineMap, archPath)
				}
			} else {
				log.Printf("[Extractor] 解压单文件至专属目录失败 %s: %v", archPath, extractErr)
			}
		}

		// 规则 3: 新建目录下同前缀的普通文件合并成单一文件 (读取每个文件第一行确定时间先后)
		destEntries, _ := os.ReadDir(destDir)
		var filesToMerge []string
		for _, de := range destEntries {
			if de.IsDir() || strings.HasPrefix(de.Name(), ".") || model.IsInternalIndexFile(de.Name()) {
				continue
			}
			if getLogPrefixBeforeDot(de.Name()) == prefix {
				filesToMerge = append(filesToMerge, filepath.Join(destDir, de.Name()))
			}
		}

		if len(filesToMerge) == 0 {
			continue
		}

		finalFile := filepath.Join(destDir, prefix)

		if len(filesToMerge) == 1 {
			if filesToMerge[0] != finalFile {
				_ = os.Rename(filesToMerge[0], finalFile)
				if lineMap != nil {
					lineMap[finalFile] = lineMap[filesToMerge[0]]
					delete(lineMap, filesToMerge[0])
				}
			}
			continue
		}

		// 多个文件：读取每个文件第一行解析时间戳并升序排序
		type itemSort struct {
			path  string
			score float64
		}
		var sortList []itemSort
		for _, f := range filesToMerge {
			score := extractLogStartTime(f)
			sortList = append(sortList, itemSort{path: f, score: score})
		}
		sort.SliceStable(sortList, func(i, j int) bool {
			return sortList[i].score < sortList[j].score
		})

		var sortedPaths []string
		for _, item := range sortList {
			sortedPaths = append(sortedPaths, item.path)
		}

		if err := mergeLogFilesIntoOne(finalFile, sortedPaths, lineMap); err != nil {
			log.Printf("[Extractor] 合并日志文件失败 %s: %v", finalFile, err)
		}
	}
}

// consolidateSingleArchivesAndRotatedLogs 遍历目标目录下所有子目录，对单文件压缩包及同前缀日志执行规整与合并
func consolidateSingleArchivesAndRotatedLogs(targetDir string, lineMap map[string]int64) {
	dirsToProcess := make(map[string]bool)
	_ = filepath.WalkDir(targetDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if model.IsInternalIndexFile(d.Name()) {
			return nil
		}
		aType := getArchiveType(path)
		if aType == archSingleGz || aType == archSingleBz2 {
			dirsToProcess[filepath.Dir(path)] = true
		}
		return nil
	})

	for dir := range dirsToProcess {
		processSingleArchivesInDir(dir, lineMap)
	}
}

// collectMultiArchivesInDir 收集指定目录下的多文件压缩包 (.tar.gz, .zip, .7z 等，排除单文件 gz/bz2)
func collectMultiArchivesInDir(dir string, queue *[]string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if model.IsInternalIndexFile(d.Name()) {
			return nil
		}
		aType := getArchiveType(path)
		if aType != archNone && aType != archSingleGz && aType != archSingleBz2 {
			*queue = append(*queue, path)
		}
		return nil
	})
}

// unpackNestedArchives 使用增量式工作队列递归解压目录中所有嵌套的压缩包
func unpackNestedArchives(targetDir string, lineMap map[string]int64) {
	var queue []string
	collectMultiArchivesInDir(targetDir, &queue)

	const maxDepth = 10
	depth := 0

	for len(queue) > 0 && depth < maxDepth {
		depth++
		currentBatch := queue
		queue = nil

		for _, archPath := range currentBatch {
			if _, statErr := os.Stat(archPath); statErr != nil {
				continue
			}
			aType := getArchiveType(archPath)
			if aType == archNone || aType == archSingleGz || aType == archSingleBz2 {
				continue
			}

			parentDir := filepath.Dir(archPath)
			fileName := filepath.Base(archPath)
			cleanName := getCleanArchiveBase(fileName, aType)
			tmpDir, err := os.MkdirTemp(parentDir, ".tmp_extract_*")
			if err != nil {
				log.Printf("[Extractor] 创建嵌套解压临时目录失败: %v", err)
				continue
			}

			err = extractOneArchive(aType, archPath, tmpDir, lineMap)
			if err != nil {
				log.Printf("[Extractor] 解压多层嵌套包失败 %s: %v", archPath, err)
				_ = os.RemoveAll(tmpDir)
				continue
			}

			entries, _ := os.ReadDir(tmpDir)

			// 检查子压缩包内部是否本身已经包含了一个与该子包名完全同名的文件夹 (忽略大小写精确比对 cleanName)
			var sameNameDirEntry os.DirEntry
			for _, entry := range entries {
				if entry.IsDir() && (entry.Name() == cleanName || strings.EqualFold(entry.Name(), cleanName)) {
					sameNameDirEntry = entry
					break
				}
			}

			var finalDest string
			if sameNameDirEntry != nil {
				// 内部本身就已经包含了一个同名文件夹：绝不额外嵌套生成同名目录，直接提升该同名文件夹
				innerDirName := sameNameDirEntry.Name()
				finalDest = filepath.Join(parentDir, innerDirName)
				innerSrc := filepath.Join(tmpDir, innerDirName)
				if _, destErr := os.Stat(finalDest); os.IsNotExist(destErr) {
					_ = os.Rename(innerSrc, finalDest)
				} else {
					_ = moveDirContents(innerSrc, finalDest)
				}
				_ = os.RemoveAll(innerSrc)
				if len(entries) > 1 {
					_ = moveDirContents(tmpDir, finalDest)
				}
			} else {
				// 否则（内部本身未包含同名文件夹）：必须统一生成同名专属目录，将解出内容收纳于 parentDir/cleanName
				finalDest = filepath.Join(parentDir, cleanName)
				if _, destErr := os.Stat(finalDest); os.IsNotExist(destErr) {
					_ = os.Rename(tmpDir, finalDest)
				} else {
					_ = moveDirContents(tmpDir, finalDest)
				}
			}

			_ = os.RemoveAll(tmpDir)
			_ = os.Remove(archPath)
			delete(lineMap, archPath)

			collectMultiArchivesInDir(finalDest, &queue)
		}
	}

	// 多层多文件归档解压完毕后，对全部单文件压缩包及同前缀日志执行规整与按时间流式合并
	consolidateSingleArchivesAndRotatedLogs(targetDir, lineMap)
}

// ExtractArchive 根据压缩包后缀自动选择解压格式并解压至 targetDir，同时自动递归解压多层压缩包
func ExtractArchive(archivePath, targetDir string) ([]*model.LogFileItem, int64, error) {
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return nil, 0, err
	}

	lower := strings.ToLower(archivePath)
	var err error
	lineMap := make(map[string]int64)

	if strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz") {
		err = extractTarGz(archivePath, targetDir, lineMap)
	} else if strings.HasSuffix(lower, ".zip") {
		err = extractZip(archivePath, targetDir, lineMap)
	} else if strings.HasSuffix(lower, ".7z") {
		err = extract7z(archivePath, targetDir, lineMap)
	} else if strings.HasSuffix(lower, ".tar.bz2") || strings.HasSuffix(lower, ".tbz2") {
		err = extractTarBz2(archivePath, targetDir, lineMap)
	} else if strings.HasSuffix(lower, ".tar") {
		err = extractTar(archivePath, targetDir, lineMap)
	} else if strings.HasSuffix(lower, ".gz") || strings.HasSuffix(lower, ".bz2") {
		prefix := getLogPrefixBeforeDot(filepath.Base(archivePath))
		destDir := filepath.Join(targetDir, prefix)
		_ = os.MkdirAll(destDir, 0755)
		var aType archiveType
		if strings.HasSuffix(lower, ".gz") {
			aType = archSingleGz
			err = extractSingleGz(archivePath, destDir, lineMap)
		} else {
			aType = archSingleBz2
			err = extractSingleBz2(archivePath, destDir, lineMap)
		}
		if err == nil {
			cleanBase := getCleanArchiveBase(filepath.Base(archivePath), aType)
			extractedFile := filepath.Join(destDir, cleanBase)
			finalFile := filepath.Join(destDir, prefix)
			if extractedFile != finalFile {
				_ = os.Rename(extractedFile, finalFile)
				if lineMap != nil {
					lineMap[finalFile] = lineMap[extractedFile]
					delete(lineMap, extractedFile)
				}
			}
		}
	} else {
		// 单个纯文本日志文件或未压缩包，直接拷贝
		dest := filepath.Join(targetDir, filepath.Base(archivePath))
		err = copyFile(archivePath, dest, lineMap)
	}

	if err != nil {
		_ = os.RemoveAll(targetDir)
		return nil, 0, fmt.Errorf("解压缩失败: %w", err)
	}

	// 自动递归解压多层嵌套压缩包
	unpackNestedArchives(targetDir, lineMap)

	// 遍历解压目录生成文件树列表及统计总行数（直接从 lineMap 中获取行数，无需再次全量读盘）
	var fileList []*model.LogFileItem
	var totalLines int64

	_ = filepath.Walk(targetDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(targetDir, path)
		if rel == "." {
			return nil
		}
		// 隐藏系统内部生成的索引文件
		if model.IsInternalIndexFile(info.Name()) {
			return nil
		}

		item := &model.LogFileItem{
			RelativePath: rel,
			Size:         info.Size(),
			ModTime:      info.ModTime(),
			IsDirectory:  info.IsDir(),
		}

		if !info.IsDir() {
			lines, ok := lineMap[path]
			if !ok {
				// 容错降级：若有漏统文件则单独计算
				lines = countFileLines(path)
			}
			item.LineCount = lines
			totalLines += lines
		}

		fileList = append(fileList, item)
		return nil
	})

	return fileList, totalLines, nil
}

func extractTarGz(src, dest string, lineMap map[string]int64) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gzr.Close()

	return untar(gzr, dest, lineMap)
}

func extractTarBz2(src, dest string, lineMap map[string]int64) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	bzr := bzip2.NewReader(f)
	return untar(bzr, dest, lineMap)
}

func extractTar(src, dest string, lineMap map[string]int64) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	return untar(f, dest, lineMap)
}

func untar(r io.Reader, dest string, lineMap map[string]int64) error {
	tr := tar.NewReader(r)
	bufPtr := extractBufPool.Get().(*[]byte)
	defer extractBufPool.Put(bufPtr)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// 防止 Zip Slip 漏洞与前缀截断绕过
		target := filepath.Join(dest, header.Name)
		if !model.IsSafeSubpath(dest, target) {
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			_ = os.MkdirAll(target, 0755)
		case tar.TypeReg:
			_ = os.MkdirAll(filepath.Dir(target), 0755)
			outFile, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, header.FileInfo().Mode())
			if err != nil {
				continue
			}
			bw, release := getBufioWriter(outFile, header.Size)
			cw := &lineCountingWriter{w: bw}
			_, _ = io.CopyBuffer(cw, tr, *bufPtr)
			release()
			outFile.Close()
			if lineMap != nil {
				lineMap[target] = cw.lines
			}
		}
	}
	return nil
}

func extractZip(src, dest string, lineMap map[string]int64) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()

	bufPtr := extractBufPool.Get().(*[]byte)
	defer extractBufPool.Put(bufPtr)

	for _, f := range r.File {
		target := filepath.Join(dest, f.Name)
		if !model.IsSafeSubpath(dest, target) {
			continue
		}
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(target, 0755)
			continue
		}

		_ = os.MkdirAll(filepath.Dir(target), 0755)
		outFile, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			continue
		}
		bw, release := getBufioWriter(outFile, f.FileInfo().Size())
		cw := &lineCountingWriter{w: bw}
		_, _ = io.CopyBuffer(cw, rc, *bufPtr)
		release()
		outFile.Close()
		rc.Close()
		if lineMap != nil {
			lineMap[target] = cw.lines
		}
	}
	return nil
}

func extract7z(src, dest string, lineMap map[string]int64) error {
	r, err := sevenzip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()

	bufPtr := extractBufPool.Get().(*[]byte)
	defer extractBufPool.Put(bufPtr)

	for _, f := range r.File {
		target := filepath.Join(dest, f.Name)
		if !model.IsSafeSubpath(dest, target) {
			continue
		}
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(target, 0755)
			continue
		}

		_ = os.MkdirAll(filepath.Dir(target), 0755)
		mode := f.Mode()
		if mode&0600 != 0600 {
			mode = mode | 0644
		}
		outFile, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
		if err != nil {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			continue
		}
		bw, release := getBufioWriter(outFile, f.FileInfo().Size())
		cw := &lineCountingWriter{w: bw}
		_, _ = io.CopyBuffer(cw, rc, *bufPtr)
		release()
		outFile.Close()
		rc.Close()
		if lineMap != nil {
			lineMap[target] = cw.lines
		}
	}
	return nil
}

func extractSingleGz(src, dest string, lineMap map[string]int64) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gzr.Close()

	base := strings.TrimSuffix(filepath.Base(src), ".gz")
	target := filepath.Join(dest, base)
	outFile, err := os.Create(target)
	if err != nil {
		return err
	}
	defer outFile.Close()

	bw, release := getBufioWriter(outFile, 0)
	defer release()

	cw := &lineCountingWriter{w: bw}
	bufPtr := extractBufPool.Get().(*[]byte)
	defer extractBufPool.Put(bufPtr)

	_, err = io.CopyBuffer(cw, gzr, *bufPtr)
	if lineMap != nil {
		lineMap[target] = cw.lines
	}
	return err
}

func extractSingleBz2(src, dest string, lineMap map[string]int64) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	bzr := bzip2.NewReader(f)
	base := strings.TrimSuffix(filepath.Base(src), ".bz2")
	target := filepath.Join(dest, base)
	outFile, err := os.Create(target)
	if err != nil {
		return err
	}
	defer outFile.Close()

	bw, release := getBufioWriter(outFile, 0)
	defer release()

	cw := &lineCountingWriter{w: bw}
	bufPtr := extractBufPool.Get().(*[]byte)
	defer extractBufPool.Put(bufPtr)

	_, err = io.CopyBuffer(cw, bzr, *bufPtr)
	if lineMap != nil {
		lineMap[target] = cw.lines
	}
	return err
}

func copyFile(src, dest string, lineMap map[string]int64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	fi, _ := in.Stat()
	var fileSize int64
	if fi != nil {
		fileSize = fi.Size()
	}

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	bw, release := getBufioWriter(out, fileSize)
	defer release()

	cw := &lineCountingWriter{w: bw}
	bufPtr := extractBufPool.Get().(*[]byte)
	defer extractBufPool.Put(bufPtr)

	_, err = io.CopyBuffer(cw, in, *bufPtr)
	if lineMap != nil {
		lineMap[dest] = cw.lines
	}
	return err
}

func countFileLines(filePath string) int64 {
	f, err := os.Open(filePath)
	if err != nil {
		return 0
	}
	defer f.Close()

	var count int64
	buf := make([]byte, 32*1024)
	for {
		c, err := f.Read(buf)
		count += int64(bytes.Count(buf[:c], []byte{'\n'}))
		if err != nil {
			break
		}
	}
	return count
}
