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
	"strings"
	"sync"

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

// collectArchivesInDir 收集指定目录下的压缩包文件
func collectArchivesInDir(dir string, queue *[]string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if model.IsInternalIndexFile(d.Name()) {
			return nil
		}
		if getArchiveType(path) != archNone {
			*queue = append(*queue, path)
		}
		return nil
	})
}

// unpackNestedArchives 使用增量式工作队列递归解压目录中所有嵌套的压缩包 (避免全盘重复扫描，复杂度从 O(D*N) 降至严格 O(N))
func unpackNestedArchives(targetDir string, lineMap map[string]int64) {
	var queue []string
	collectArchivesInDir(targetDir, &queue)

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
			if aType == archNone {
				continue
			}

			parentDir := filepath.Dir(archPath)
			fileName := filepath.Base(archPath)

			if aType == archSingleGz || aType == archSingleBz2 {
				cleanName := getCleanArchiveBase(fileName, aType)
				targetFile := filepath.Join(parentDir, cleanName)
				if targetFile == archPath {
					targetFile = archPath + ".out"
				}

				var err error
				if aType == archSingleGz {
					err = extractSingleGz(archPath, parentDir, lineMap)
				} else {
					err = extractSingleBz2(archPath, parentDir, lineMap)
				}
				if err == nil {
					_ = os.Remove(archPath)
					delete(lineMap, archPath)
					// 若单文件解压出来的目标文件自身又是某种压缩包 (如 foo.tar.gz 被解成了 foo.tar)，加入队列
					if getArchiveType(targetFile) != archNone {
						queue = append(queue, targetFile)
					}
				} else {
					log.Printf("[Extractor] 解压嵌套单文件失败 %s: %v", archPath, err)
				}
			} else {
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
					// 若临时目录下还有同级的其他零星文件/目录，也移入该 finalDest 统一收纳
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

				// 关键优化：仅对新解压出的 finalDest 局部目录收集下一层嵌套压缩包，避免全盘重复扫描！
				collectArchivesInDir(finalDest, &queue)
			}
		}
	}
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
	} else if strings.HasSuffix(lower, ".gz") {
		err = extractSingleGz(archivePath, targetDir, lineMap)
	} else if strings.HasSuffix(lower, ".bz2") {
		err = extractSingleBz2(archivePath, targetDir, lineMap)
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
