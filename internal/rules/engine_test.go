package rules

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func generateBenchmarkLogFiles(tb testing.TB, dir string, fileCount, linesPerFile int) {
	tb.Helper()
	for i := 0; i < fileCount; i++ {
		filePath := filepath.Join(dir, fmt.Sprintf("node_%d.log", i))
		f, err := os.Create(filePath)
		if err != nil {
			tb.Fatalf("创建临时日志文件失败: %v", err)
		}
		for j := 1; j <= linesPerFile; j++ {
			if j%500 == 0 {
				_, _ = f.WriteString(fmt.Sprintf("2026-09-09 10:00:%02d [ERROR] osd.%d marked down, heartbeat_check: no reply\n", j%60, i))
			} else if j%200 == 0 {
				_, _ = f.WriteString(fmt.Sprintf("2026-09-09 10:00:%02d [WARN] 12 slow requests are blocked currently\n", j%60))
			} else {
				_, _ = f.WriteString(fmt.Sprintf("2026-09-09 10:00:%02d [INFO] sync block completed partition=%d seq=%d\n", j%60, i, j))
			}
		}
		_ = f.Close()
	}
}

func TestDiagnoseDirectoryParallel(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "rules_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	generateBenchmarkLogFiles(t, tempDir, 8, 2000)

	engine := NewEngine(DefaultPresets())
	result, err := engine.DiagnoseDirectory("arc-test-01", "user-01", "test_archive.tar.gz", tempDir)
	if err != nil {
		t.Fatalf("DiagnoseDirectory 失败: %v", err)
	}

	if result.Status != "completed" {
		t.Fatalf("期望状态 completed，实际: %s", result.Status)
	}
	if len(result.Events) == 0 {
		t.Fatal("期望检测到异常事件，实际为 0")
	}
	t.Logf("扫描成功，检测到 %d 个事件，健康分: %d，严重级别汇总: %v", len(result.Events), result.HealthScore, result.SeveritySummary)
}

func BenchmarkDiagnoseDirectory(b *testing.B) {
	tempDir, err := os.MkdirTemp("", "rules_bench_*")
	if err != nil {
		b.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	generateBenchmarkLogFiles(b, tempDir, 8, 5000) // 8 个文件，共 40,000 行日志
	engine := NewEngine(DefaultPresets())

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := engine.DiagnoseDirectory("bench-01", "user-bench", "bench.tar.gz", tempDir)
		if err != nil {
			b.Fatal(err)
		}
	}
}
