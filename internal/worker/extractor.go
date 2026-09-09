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

// ExtractArchive 根据压缩包后缀自动选择解压格式并解压至 targetDir
func ExtractArchive(archivePath, targetDir string) ([]*model.LogFileItem, int64, error) {
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return nil, 0, err
	}

	lower := strings.ToLower(archivePath)
	var err error

	if strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz") {
		err = extractTarGz(archivePath, targetDir)
	} else if strings.HasSuffix(lower, ".zip") {
		err = extractZip(archivePath, targetDir)
	} else if strings.HasSuffix(lower, ".tar.bz2") || strings.HasSuffix(lower, ".tbz2") {
		err = extractTarBz2(archivePath, targetDir)
	} else if strings.HasSuffix(lower, ".tar") {
		err = extractTar(archivePath, targetDir)
	} else if strings.HasSuffix(lower, ".gz") {
		err = extractSingleGz(archivePath, targetDir)
	} else if strings.HasSuffix(lower, ".bz2") {
		err = extractSingleBz2(archivePath, targetDir)
	} else {
		// 单个纯文本日志文件或未压缩包，直接拷贝
		dest := filepath.Join(targetDir, filepath.Base(archivePath))
		err = copyFile(archivePath, dest)
	}

	if err != nil {
		return nil, 0, fmt.Errorf("解压缩失败: %w", err)
	}

	// 遍历解压目录生成文件树列表及统计总行数
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
			lines := countFileLines(path)
			item.LineCount = lines
			totalLines += lines
		}

		fileList = append(fileList, item)
		return nil
	})

	return fileList, totalLines, nil
}

func extractTarGz(src, dest string) error {
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

	return untar(gzr, dest)
}

func extractTarBz2(src, dest string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	bzr := bzip2.NewReader(f)
	return untar(bzr, dest)
}

func extractTar(src, dest string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	return untar(f, dest)
}

func untar(r io.Reader, dest string) error {
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
			_, _ = io.Copy(outFile, tr)
			outFile.Close()
		}
	}
	return nil
}

func extractZip(src, dest string) error {
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
		_, _ = io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()
	}
	return nil
}

func extractSingleGz(src, dest string) error {
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

	_, err = io.Copy(outFile, gzr)
	return err
}

func extractSingleBz2(src, dest string) error {
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

	_, err = io.Copy(outFile, bzr)
	return err
}

func copyFile(src, dest string) error {
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

	_, err = io.Copy(out, in)
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
