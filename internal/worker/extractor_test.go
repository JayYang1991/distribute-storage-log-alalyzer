package worker

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dist-log-analyzer/internal/model"
)

func createSampleTarGz(tb testing.TB, archivePath string, fileCount, linesPerFile int) int64 {
	tb.Helper()
	outFile, err := os.Create(archivePath)
	if err != nil {
		tb.Fatalf("创建归档包失败: %v", err)
	}
	defer outFile.Close()

	gw := gzip.NewWriter(outFile)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	var expectedTotalLines int64

	for i := 0; i < fileCount; i++ {
		fileName := fmt.Sprintf("service-%d/access.log", i)
		content := ""
		for j := 1; j <= linesPerFile; j++ {
			content += fmt.Sprintf("2026-09-09 10:00:%02d [INFO] file=%d line=%d req_id=abcdefg-12345\n", j%60, i, j)
		}
		expectedTotalLines += int64(linesPerFile)

		hdr := &tar.Header{
			Name: fileName,
			Mode: 0644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			tb.Fatalf("写入 tar 头失败: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			tb.Fatalf("写入 tar 内容失败: %v", err)
		}
	}

	return expectedTotalLines
}

func TestExtractArchiveLineCount(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "extractor_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	archivePath := filepath.Join(tempDir, "test_logs.tar.gz")
	targetDir := filepath.Join(tempDir, "extracted")

	expectedLines := createSampleTarGz(t, archivePath, 5, 200)

	fileList, totalLines, err := ExtractArchive(archivePath, targetDir)
	if err != nil {
		t.Fatalf("ExtractArchive 失败: %v", err)
	}

	if totalLines != expectedLines {
		t.Fatalf("预期总行数 %d，实际为 %d", expectedLines, totalLines)
	}

	for _, item := range fileList {
		if !item.IsDirectory {
			if item.LineCount != 200 {
				t.Errorf("文件 %s 预期行数 200，实际: %d", item.RelativePath, item.LineCount)
			}
		}
	}
	t.Logf("解压与行数流式统计成功，文件项: %d，总行数: %d", len(fileList), totalLines)
}

func TestExtract7zArchive(t *testing.T) {
	sample7z := "../../testdata/sample.7z"
	if _, err := os.Stat(sample7z); os.IsNotExist(err) {
		t.Skip("跳过 7z 测试：未找到 sample.7z")
	}

	tempDir, err := os.MkdirTemp("", "extractor_7z_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	fileList, totalLines, err := ExtractArchive(sample7z, tempDir)
	if err != nil {
		t.Fatalf("ExtractArchive(.7z) 失败: %v", err)
	}

	if len(fileList) == 0 {
		t.Fatalf("预期 7z 内有解压文件，实际为空")
	}
	t.Logf("7z 解压成功，文件数: %d, 总行数: %d", len(fileList), totalLines)
	for _, f := range fileList {
		t.Logf("  - %s (dir=%v, lines=%d, size=%d)", f.RelativePath, f.IsDirectory, f.LineCount, f.Size)
	}
}

func TestExtractNestedArchive(t *testing.T) {
	nestedArchive := "../../testdata/nested_outer.tar.gz"
	if _, err := os.Stat(nestedArchive); os.IsNotExist(err) {
		t.Skip("跳过多层嵌套测试：未找到 nested_outer.tar.gz")
	}

	tempDir, err := os.MkdirTemp("", "extractor_nested_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	fileList, totalLines, err := ExtractArchive(nestedArchive, tempDir)
	if err != nil {
		t.Fatalf("ExtractArchive 多层嵌套解压失败: %v", err)
	}

	fileMap := make(map[string]*model.LogFileItem)
	for _, f := range fileList {
		fileMap[f.RelativePath] = f
		t.Logf("解压结果项: %s (dir=%v, lines=%d)", f.RelativePath, f.IsDirectory, f.LineCount)
	}

	// 1. 验证 outer 文件
	if _, ok := fileMap["info.txt"]; !ok {
		t.Errorf("缺少顶级文件 info.txt")
	}

	// 2. 验证单文件 gzip 自动解开
	if _, ok := fileMap["syslog.1"]; !ok {
		t.Errorf("缺少自动解压后的 syslog.1 (应从 syslog.1.gz 展开)")
	}
	if _, ok := fileMap["syslog.1.gz"]; ok {
		t.Errorf("原始 syslog.1.gz 应已清理，但仍存在")
	}

	// 3. 验证嵌套的 7z 自动解压
	hasNodeA := false
	for rel := range fileMap {
		if strings.Contains(rel, "node_a") || strings.Contains(rel, "syslog.log") {
			hasNodeA = true
			break
		}
	}
	if !hasNodeA {
		t.Errorf("嵌套 node_a.7z 未成功递归解压")
	}
	if _, ok := fileMap["node_a.7z"]; ok {
		t.Errorf("原始 node_a.7z 应已清理，但仍存在")
	}

	// 4. 验证嵌套 zip 及其内部 gz 自动递归解压
	hasDaemon := false
	for rel := range fileMap {
		if strings.Contains(rel, "daemon.log") {
			hasDaemon = true
			break
		}
	}
	if !hasDaemon {
		t.Errorf("嵌套 zip 内的 daemon.log.gz 未能递归解压成 daemon.log")
	}
	if _, ok := fileMap["node_b.zip"]; ok {
		t.Errorf("原始 node_b.zip 应已清理，但仍存在")
	}

	t.Logf("多层嵌套压缩包递归解压验证全部通过！总行数: %d", totalLines)
}

func BenchmarkExtractArchive(b *testing.B) {
	tempDir, err := os.MkdirTemp("", "extractor_bench_*")
	if err != nil {
		b.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	archivePath := filepath.Join(tempDir, "bench_logs.tar.gz")
	createSampleTarGz(b, archivePath, 5, 1000)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		targetDir := filepath.Join(tempDir, fmt.Sprintf("out_%d", i))
		_, _, err := ExtractArchive(archivePath, targetDir)
		if err != nil {
			b.Fatal(err)
		}
	}
}
