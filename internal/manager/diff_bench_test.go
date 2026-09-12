package manager

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"dist-log-analyzer/internal/indexer"
)

func BenchmarkMineScopedArchiveSamples(b *testing.B) {
	tempDir, err := os.MkdirTemp("", "bench_diff_mine_*")
	if err != nil {
		b.Fatalf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// 创建 20 个日志文件，模拟中大型归档日志
	numFiles := 20
	linesPerFile := 500
	for i := 0; i < numFiles; i++ {
		subDir := filepath.Join(tempDir, fmt.Sprintf("service-%02d", i%5))
		_ = os.MkdirAll(subDir, 0755)
		filePath := filepath.Join(subDir, fmt.Sprintf("app-%02d.log", i))

		f, err := os.Create(filePath)
		if err != nil {
			b.Fatalf("创建测试文件失败: %v", err)
		}
		for j := 0; j < linesPerFile; j++ {
			fmt.Fprintf(f, "2026-09-13 12:00:%02d [ERROR] Service-%d failed processing request 192.168.1.%d code=%d error_id=ERR-%d\n",
				j%60, i, (j*7)%250, 500+(j%5), j%100)
		}
		_ = f.Close()
	}

	b.ResetTimer()
	b.ReportAllocs()

	for n := 0; n < b.N; n++ {
		miner := indexer.NewDrainMiner(0.55, 4)
		mineScopedArchiveSamples(tempDir, "", miner, 1000)
	}
}
