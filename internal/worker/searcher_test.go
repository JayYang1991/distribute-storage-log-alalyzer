package worker

import (
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
	t.Logf("搜索成功，总命中数: %d，分页返回数: %d，耗时: %d ms", resp.TotalHits, len(resp.Hits), resp.CostMS)
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
