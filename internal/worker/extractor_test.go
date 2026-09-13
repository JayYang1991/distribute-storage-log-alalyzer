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

func TestNestedArchiveSameNameDirPolicy(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "extractor_policy_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	// 1. 创建子包 A：内部本身包含同名文件夹 "sub_with_dir/app.log"
	subWithDirPath := filepath.Join(tempDir, "sub_with_dir.tar.gz")
	{
		f, _ := os.Create(subWithDirPath)
		gw := gzip.NewWriter(f)
		tw := tar.NewWriter(gw)
		content := "log line in sub_with_dir"
		_ = tw.WriteHeader(&tar.Header{
			Name: "sub_with_dir/app.log",
			Mode: 0644,
			Size: int64(len(content)),
		})
		_, _ = tw.Write([]byte(content))
		_ = tw.Close()
		_ = gw.Close()
		_ = f.Close()
	}

	// 2. 创建子包 B：内部无同名文件夹，仅包含根层文件 "direct.log"
	subWithoutDirPath := filepath.Join(tempDir, "sub_without_dir.tar.gz")
	{
		f, _ := os.Create(subWithoutDirPath)
		gw := gzip.NewWriter(f)
		tw := tar.NewWriter(gw)
		content := "log line in sub_without_dir"
		_ = tw.WriteHeader(&tar.Header{
			Name: "direct.log",
			Mode: 0644,
			Size: int64(len(content)),
		})
		_, _ = tw.Write([]byte(content))
		_ = tw.Close()
		_ = gw.Close()
		_ = f.Close()
	}

	// 3. 将两个子包打入外层包 outer.tar.gz
	outerTarGz := filepath.Join(tempDir, "outer.tar.gz")
	{
		f, _ := os.Create(outerTarGz)
		gw := gzip.NewWriter(f)
		tw := tar.NewWriter(gw)

		dataA, _ := os.ReadFile(subWithDirPath)
		_ = tw.WriteHeader(&tar.Header{
			Name: "sub_with_dir.tar.gz",
			Mode: 0644,
			Size: int64(len(dataA)),
		})
		_, _ = tw.Write(dataA)

		dataB, _ := os.ReadFile(subWithoutDirPath)
		_ = tw.WriteHeader(&tar.Header{
			Name: "sub_without_dir.tar.gz",
			Mode: 0644,
			Size: int64(len(dataB)),
		})
		_, _ = tw.Write(dataB)

		_ = tw.Close()
		_ = gw.Close()
		_ = f.Close()
	}

	// 4. 执行解压
	targetDir := filepath.Join(tempDir, "extracted")
	fileList, _, err := ExtractArchive(outerTarGz, targetDir)
	if err != nil {
		t.Fatalf("ExtractArchive 失败: %v", err)
	}

	fileMap := make(map[string]bool)
	for _, item := range fileList {
		fileMap[filepath.ToSlash(item.RelativePath)] = true
		t.Logf("解压文件项: %s (isDir=%v)", filepath.ToSlash(item.RelativePath), item.IsDirectory)
	}

	// 验证规则 A: 内部自带同名目录 -> 不额外生成同名目录 (即仅一层 sub_with_dir/app.log，不可出现 sub_with_dir/sub_with_dir/app.log)
	if !fileMap["sub_with_dir/app.log"] {
		t.Errorf("期望解压出 sub_with_dir/app.log，但未找到")
	}
	if fileMap["sub_with_dir/sub_with_dir/app.log"] {
		t.Errorf("错误：子包内部已有同名文件夹，不应额外嵌套生成同名目录 (出现重复套娃)")
	}

	// 验证规则 B: 内部未包含同名目录 -> 必须生成同名目录收纳 (即必须在 sub_without_dir/direct.log，不可直接散落在根 direct.log)
	if !fileMap["sub_without_dir/direct.log"] {
		t.Errorf("期望解压出 sub_without_dir/direct.log，但未找到")
	}
	if fileMap["direct.log"] {
		t.Errorf("错误：子包内部散列文件未收纳进同名目录，直接散落到了顶级目录")
	}
}

