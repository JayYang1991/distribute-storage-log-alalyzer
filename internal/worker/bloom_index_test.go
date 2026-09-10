package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestBloomFilterBasic(t *testing.T) {
	var bf BloomFilter

	tokens := [][]byte{
		[]byte("sda"),
		[]byte("corrupt"),
		[]byte("panic"),
		[]byte("timeout"),
	}

	for _, tok := range tokens {
		bf.Add(tok)
	}

	for _, tok := range tokens {
		if !bf.MayContain(tok) {
			t.Fatalf("预期包含词项 %s，但 MayContain 返回 false", string(tok))
		}
	}

	absentTokens := [][]byte{
		[]byte("nonexistent123"),
		[]byte("foobar_xyz"),
		[]byte("kernel_deadlock"),
	}

	for _, tok := range absentTokens {
		if bf.MayContain(tok) {
			t.Logf("布隆过滤器正常假阳性 (允许范围): %s", string(tok))
		}
	}
}

func TestChunkBloomIndexBuildAndPersist(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test_large.log")

	f, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("创建临时测试文件失败: %v", err)
	}

	// 写入 45,000 行日志 (跨 3 个 Chunk)
	// 仅在第 32,000 行写入罕见关键字 "sda_error_trigger"
	totalLines := 45000
	rareLine := 32000
	for i := 1; i <= totalLines; i++ {
		if i == rareLine {
			_, _ = f.WriteString(fmt.Sprintf("%06d: 2026-09-10 CRITICAL kernel: sda_error_trigger disk failed\n", i))
		} else {
			_, _ = f.WriteString(fmt.Sprintf("%06d: 2026-09-10 INFO heartbeat healthy node_%d\n", i, i%10))
		}
	}
	_ = f.Close()

	fi, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat 失败: %v", err)
	}

	idx, err := buildChunkBloomIndex(logPath, fi.Size(), fi.ModTime().UnixNano())
	if err != nil {
		t.Fatalf("构建布隆索引失败: %v", err)
	}

	if idx.TotalLines != int64(totalLines) {
		t.Fatalf("总行数统计不一致: 预期 %d, 实际 %d", totalLines, idx.TotalLines)
	}

	if len(idx.Chunks) != 3 {
		t.Fatalf("分块数量预期为 3 (45000/15000), 实际为 %d", len(idx.Chunks))
	}

	// 验证罕见关键字过滤效果
	kw := []byte("sda_error_trigger")

	// Chunk 0 (1..15000) 和 Chunk 1 (15001..30000) 必须 MayContain = false
	if idx.Chunks[0].Filter.MayContain(kw) {
		t.Fatalf("Chunk 0 不应包含 sda_error_trigger")
	}
	if idx.Chunks[1].Filter.MayContain(kw) {
		t.Fatalf("Chunk 1 不应包含 sda_error_trigger")
	}
	// Chunk 2 (30001..45000) 必须包含
	if !idx.Chunks[2].Filter.MayContain(kw) {
		t.Fatalf("Chunk 2 必须包含 sda_error_trigger")
	}

	t.Logf("✔ 布隆稀疏分块跳过验证通过: 3 个分块成功跳过前 2 个 (跳过率 66.7%%)")

	// 验证磁盘持久化与反序列化
	bidxPath := logPath + ".bidx"
	if err := saveBloomIndexToDisk(bidxPath, idx); err != nil {
		t.Fatalf("保存 .bidx 失败: %v", err)
	}

	loadedIdx, err := loadBloomIndexFromDisk(bidxPath, fi.Size(), fi.ModTime().UnixNano())
	if err != nil {
		t.Fatalf("加载 .bidx 失败: %v", err)
	}

	if loadedIdx.TotalLines != idx.TotalLines || len(loadedIdx.Chunks) != len(idx.Chunks) {
		t.Fatalf("反序列化后数据不一致")
	}

	if !loadedIdx.Chunks[2].Filter.MayContain(kw) {
		t.Fatalf("反序列化后 Chunk 2 判定失败")
	}

	t.Logf("✔ 磁盘持久化 .bidx 校验成功")
}
