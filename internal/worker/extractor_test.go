package worker

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"testing"
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
