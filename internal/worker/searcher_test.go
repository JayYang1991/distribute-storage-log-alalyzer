package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"dist-log-analyzer/internal/model"
)

func generateSearchBenchmarkFiles(tb testing.TB, dir string, fileCount, linesPerFile int) {
	tb.Helper()
	for i := 0; i < fileCount; i++ {
		filePath := filepath.Join(dir, fmt.Sprintf("service_%d.log", i))
		f, err := os.Create(filePath)
		if err != nil {
			tb.Fatalf("创建文件失败: %v", err)
		}
		for j := 1; j <= linesPerFile; j++ {
			if j%400 == 0 {
				_, _ = f.WriteString(fmt.Sprintf("2026-09-09 10:00:%02d [ERROR] connection reset by peer in storage pool node-%d req=%d\n", j%60, i, j))
			} else if j%100 == 0 {
				_, _ = f.WriteString(fmt.Sprintf("2026-09-09 10:00:%02d [WARN] high latency detected partition=%d\n", j%60, i))
			} else {
				_, _ = f.WriteString(fmt.Sprintf("2026-09-09 10:00:%02d [INFO] heartbeat ack received from cluster member-%d\n", j%60, i))
			}
		}
		_ = f.Close()
	}
}

func TestSearchLogsStreamingAndContext(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "search_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	generateSearchBenchmarkFiles(t, tempDir, 4, 1000)

	// 测试带上下文的关键词检索
	query := &model.SearchQuery{
		Keyword:      "connection reset",
		Level:        "ERROR",
		ContextLines: 2,
		Page:         1,
		PageSize:     10,
	}

	resp, err := SearchLogs(tempDir, query)
	if err != nil {
		t.Fatalf("SearchLogs 失败: %v", err)
	}

	if resp.TotalHits == 0 {
		t.Fatal("期望匹配到日志，实际 TotalHits 为 0")
	}
	if len(resp.Hits) == 0 {
		t.Fatal("期望返回 Hits，实际为 0")
	}

	for _, hit := range resp.Hits {
		if hit.Level != "ERROR" {
			t.Errorf("期望日志级别为 ERROR，实际为 %s", hit.Level)
		}
		if len(hit.ContextBefore) > 2 {
			t.Errorf("前置上下文行数超出预期: %d", len(hit.ContextBefore))
		}
	}
	if len(resp.FileSummaries) == 0 {
		t.Fatal("期望返回 FileSummaries，实际为空")
	}
	t.Logf("搜索成功，总命中数: %d，涉及文件数: %d (首项: %s, 命中: %d)，耗时: %d ms",
		resp.TotalHits, len(resp.FileSummaries), resp.FileSummaries[0].FilePath, resp.FileSummaries[0].TotalHits, resp.CostMS)
}

func BenchmarkSearchLogs(b *testing.B) {
	tempDir, err := os.MkdirTemp("", "search_bench_*")
	if err != nil {
		b.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	// 创建 8 个文件，每个 5000 行，共 40,000 行日志
	generateSearchBenchmarkFiles(b, tempDir, 8, 5000)

	query := &model.SearchQuery{
		Keyword:      "connection reset",
		ContextLines: 3,
		Page:         1,
		PageSize:     20,
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := SearchLogs(tempDir, query)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func TestSearchLogsChunkParallelAndCancellation(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "search_parallel_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	// 生成一个超过 33MB 的单文件日志，验证分块并行检索
	largeFilePath := filepath.Join(tempDir, "large_service.log")
	f, err := os.Create(largeFilePath)
	if err != nil {
		t.Fatal(err)
	}

	targetHits := map[int64]bool{
		100:     true,
		50000:   true,
		150000:  true,
		250000:  true,
		320000:  true,
	}

	totalLines := 420000
	linePayload := "2026-09-09 12:00:00 [INFO] standard log event payload with padding text for sizing\n"
	for i := 1; i <= totalLines; i++ {
		if targetHits[int64(i)] {
			_, _ = f.WriteString(fmt.Sprintf("2026-09-09 12:00:00 [ERROR] specific_needle_for_parallel_test at line=%d\n", i))
		} else {
			_, _ = f.WriteString(linePayload)
		}
	}
	_ = f.Close()

	fi, err := os.Stat(largeFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() < largeFileParallelThreshold {
		t.Fatalf("测试文件大小为 %d 字节，未达到大文件分块阈值 %d", fi.Size(), largeFileParallelThreshold)
	}

	// 1. 验证多核分块并行检索准确性
	query := &model.SearchQuery{
		Keyword:  "specific_needle_for_parallel_test",
		Page:     1,
		PageSize: 10,
	}

	resp, err := SearchLogs(tempDir, query)
	if err != nil {
		t.Fatalf("SearchLogs 并行分块失败: %v", err)
	}

	if resp.TotalHits != int64(len(targetHits)) {
		t.Fatalf("期望命中 %d 条，实际命中 %d 条", len(targetHits), resp.TotalHits)
	}

	for _, hit := range resp.Hits {
		if !targetHits[hit.LineNumber] {
			t.Errorf("行号计算错误: 未知命中行号 %d", hit.LineNumber)
		}
	}
	t.Logf("大文件 (%d 字节) 分块并行检索验证成功，耗时: %d ms, 命中数: %d", fi.Size(), resp.CostMS, resp.TotalHits)

	// 2. 验证短期 LRU 缓存命中
	respCached, err := SearchLogs(tempDir, query)
	if err != nil {
		t.Fatalf("获取缓存结果失败: %v", err)
	}
	if respCached.TotalHits != resp.TotalHits {
		t.Fatalf("缓存结果不匹配: %d vs %d", respCached.TotalHits, resp.TotalHits)
	}
	t.Logf("缓存检索成功，耗时: %d ms (预期近乎 0ms)", respCached.CostMS)

	// 3. 验证级联取消机制
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消
	_, errCanceled := SearchLogsContext(ctx, tempDir, &model.SearchQuery{Keyword: "needle_canceled"})
	if errCanceled == nil {
		t.Fatal("期望被取消的 context 报错或中断，实际成功返回")
	}
	t.Logf("级联取消验证成功: %v", errCanceled)
}

func TestSearchWithBloomSkip(t *testing.T) {
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "large_sparse.log")

	f, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("创建临时文件失败: %v", err)
	}

	// 构造 80,000 行日志 (大于 4MB 门槛，跨 5 个 Chunk)
	// 仅在第 68,200 行写入罕见关键短语 "disk_sector_corrupt"
	totalLines := 80000
	rareLine := 68200
	for i := 1; i <= totalLines; i++ {
		if i == rareLine {
			_, _ = f.WriteString(fmt.Sprintf("%06d: 2026-09-10 CRITICAL kernel: disk_sector_corrupt at lba 0x9900\n", i))
		} else {
			_, _ = f.WriteString(fmt.Sprintf("%06d: 2026-09-10 INFO heartbeat healthy node_%d\n", i, i%10))
		}
	}
	_ = f.Close()

	fi, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("获取文件状态失败: %v", err)
	}

	// 预先建立 Bloom 索引
	bloomIdx, err := GetOrBuildBloomIndex(logPath)
	if err != nil || bloomIdx == nil {
		t.Fatalf("构建布隆索引失败: %v", err)
	}

	// 执行并行检索
	hits, err := searchInSingleFileParallel(context.Background(), logPath, "large_sparse.log", fi.Size(), nil, []byte("disk_sector_corrupt"), false, false, "", 1, 100)
	if err != nil {
		t.Fatalf("searchInSingleFileParallel 失败: %v", err)
	}

	if len(hits) != 1 {
		t.Fatalf("检索命中数预期 1，实际: %d", len(hits))
	}

	if hits[0].LineNumber != int64(rareLine) {
		t.Fatalf("绝对行号对齐失败: 预期 %d, 实际 %d", rareLine, hits[0].LineNumber)
	}

	t.Logf("✔ 布隆过滤跳过 + 绝对行号对齐测试通过！命中行: %d, 内容: %s", hits[0].LineNumber, hits[0].Content)
}

