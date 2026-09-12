package worker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// 创建多层嵌套测试包，用于压测嵌套解压性能
func createNestedBenchmarkArchive(t testing.TB, tempDir string, numNested int) string {
	// 内层包
	innerBuf := new(bytes.Buffer)
	gw := gzip.NewWriter(innerBuf)
	tw := tar.NewWriter(gw)

	content := []byte("2026-09-13 12:00:00 [INFO] nested log line sample content\n")
	hdr := &tar.Header{
		Name: "inner/log.txt",
		Mode: 0644,
		Size: int64(len(content)),
	}
	_ = tw.WriteHeader(hdr)
	_, _ = tw.Write(content)
	_ = tw.Close()
	_ = gw.Close()
	innerBytes := innerBuf.Bytes()

	// 外层包，包含 numNested 个内部包
	outerPath := filepath.Join(tempDir, "outer.tar.gz")
	f, err := os.Create(outerPath)
	if err != nil {
		t.Fatalf("创建外层包失败: %v", err)
	}
	defer f.Close()

	outerGW := gzip.NewWriter(f)
	outerTW := tar.NewWriter(outerGW)

	for i := 0; i < numNested; i++ {
		nHdr := &tar.Header{
			Name: filepath.Join("sub", "inner_pkg_"+string(rune('0'+i))+".tar.gz"),
			Mode: 0644,
			Size: int64(len(innerBytes)),
		}
		_ = outerTW.WriteHeader(nHdr)
		_, _ = outerTW.Write(innerBytes)
	}
	_ = outerTW.Close()
	_ = outerGW.Close()

	return outerPath
}

func BenchmarkExtractArchiveNested(b *testing.B) {
	b.ReportAllocs()
	tempDir, err := os.MkdirTemp("", "bench_extract_nested_*")
	if err != nil {
		b.Fatalf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tempDir)

	archivePath := createNestedBenchmarkArchive(b, tempDir, 5)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		outDir := filepath.Join(tempDir, "out_"+string(rune('0'+i%10)))
		_ = os.RemoveAll(outDir)
		_, _, _ = ExtractArchive(archivePath, outDir)
	}
}
