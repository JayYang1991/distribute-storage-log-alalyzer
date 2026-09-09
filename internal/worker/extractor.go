package worker

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"dist-log-analyzer/internal/model"
)

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

// ExtractArchive 根据压缩包后缀自动选择解压格式并解压至 targetDir
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
		return nil, 0, fmt.Errorf("解压缩失败: %w", err)
	}

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
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// 防止 Zip Slip 漏洞
		target := filepath.Join(dest, header.Name)
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(dest)) {
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
			cw := &lineCountingWriter{w: outFile}
			_, _ = io.Copy(cw, tr)
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

	for _, f := range r.File {
		target := filepath.Join(dest, f.Name)
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(dest)) {
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
		cw := &lineCountingWriter{w: outFile}
		_, _ = io.Copy(cw, rc)
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

	cw := &lineCountingWriter{w: outFile}
	_, err = io.Copy(cw, gzr)
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

	cw := &lineCountingWriter{w: outFile}
	_, err = io.Copy(cw, bzr)
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

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	cw := &lineCountingWriter{w: out}
	_, err = io.Copy(cw, in)
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
