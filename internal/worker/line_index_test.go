package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSparseLineIndex(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "line_idx_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	// 创建一个约 5MB、含 60,000 行的测试文件
	filePath := filepath.Join(tempDir, "test_indexed.log")
	f, err := os.Create(filePath)
	if err != nil {
		t.Fatal(err)
	}

	totalLines := 60000
	payload := "2026-09-10 10:00:00 [INFO] log payload line data for testing sparse index\n"
	for i := 1; i <= totalLines; i++ {
		_, _ = f.WriteString(fmt.Sprintf("%06d: %s", i, payload))
	}
	_ = f.Close()

	// 1. 构建索引
	idx, err := GetOrBuildLineIndex(filePath)
	if err != nil {
		t.Fatalf("构建稀疏行号索引失败: %v", err)
	}
	if idx == nil {
		t.Fatal("期望返回稀疏索引，实际为 nil")
	}
	if idx.TotalLines != int64(totalLines) {
		t.Errorf("总行数不匹配: 期望 %d，实际 %d", totalLines, idx.TotalLines)
	}
	t.Logf("稀疏索引建立成功，总行数: %d，记录检查点数: %d", idx.TotalLines, len(idx.Offsets))

	// 2. 测试任意行秒级直达读取
	targetLines := []int{1, 5001, 12345, 30000, 59990}
	for _, target := range targetLines {
		start := time.Now()
		lines, _, _, err := ReadFileLinesWithIndex(filePath, target, 5)
		cost := time.Since(start)
		if err != nil {
			t.Fatalf("读取行 %d 失败: %v", target, err)
		}
		if len(lines) == 0 {
			t.Fatalf("行 %d 返回内容为空", target)
		}
		expectedPrefix := fmt.Sprintf("%06d:", target)
		if len(lines[0]) < len(expectedPrefix) || lines[0][:len(expectedPrefix)] != expectedPrefix {
			t.Errorf("行 %d 内容错误: 期望前缀 %s，实际 %s", target, expectedPrefix, lines[0])
		}
		t.Logf("跳转读取行 %d 耗时: %v (首行: %s)", target, cost, lines[0][:20])
	}

	// 3. 测试从持久化文件 (.lidx) 加载
	time.Sleep(50 * time.Millisecond) // 等待异步写入磁盘
	idxPath := filePath + ".lidx"
	loadedIdx, err := loadLineIndexFromDisk(idxPath, idx.FileSize, idx.ModTimeNano)
	if err != nil {
		t.Fatalf("从磁盘加载 .lidx 失败: %v", err)
	}
	if loadedIdx.TotalLines != idx.TotalLines || len(loadedIdx.Offsets) != len(idx.Offsets) {
		t.Fatalf("从磁盘恢复的索引数据不一致: %d vs %d", loadedIdx.TotalLines, idx.TotalLines)
	}
	t.Log("磁盘持久化 .lidx 校验成功")
}
