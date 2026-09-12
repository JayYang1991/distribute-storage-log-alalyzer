package manager

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractUpgradeBinary(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "upgrade_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	// 1. 构建一个模拟的 tar.gz 安装包，里面包含一个 mock 脚本作为 dist-log-analyzer 二进制
	mockBinContent := []byte("#!/bin/sh\nif [ \"$1\" = \"--help\" ]; then echo 'mock analyzer v2.0'; exit 0; fi\nexit 0\n")

	var tarGzBuf bytes.Buffer
	gw := gzip.NewWriter(&tarGzBuf)
	tw := tar.NewWriter(gw)

	header := &tar.Header{
		Name: "bin/dist-log-analyzer",
		Mode: 0755,
		Size: int64(len(mockBinContent)),
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatalf("WriteHeader failed: %v", err)
	}
	if _, err := tw.Write(mockBinContent); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	_ = tw.Close()
	_ = gw.Close()

	// 2. 提取并校验升级包
	binPath, err := extractUpgradeBinary(tempDir, "dist-log-analyzer-linux-amd64.tar.gz", &tarGzBuf)
	if err != nil {
		t.Fatalf("extractUpgradeBinary failed: %v", err)
	}

	if filepath.Base(binPath) != "dist-log-analyzer" {
		t.Errorf("expected dist-log-analyzer, got: %s", binPath)
	}

	// 3. 测试非法/损坏升级包拦截
	badBuf := bytes.NewBufferString("corrupted random data")
	_, badErr := extractUpgradeBinary(tempDir, "broken.tar.gz", badBuf)
	if badErr == nil {
		t.Errorf("expected error for corrupted tar.gz, got nil")
	}
}
