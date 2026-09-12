package indexer

import (
	"testing"
)

var sampleLogs = []string{
	"2026-09-12 14:00:01.123 [ERROR] Failed to connect to 192.168.1.10:8080 timeout after 3000ms",
	"2026-09-12 14:00:02.456 [ERROR] Failed to connect to 192.168.1.11:8080 timeout after 3005ms",
	"2026-09-12 14:00:03.789 [ERROR] Failed to connect to 10.0.0.5:9000 timeout after 1500ms",
	"2026-09-12 14:01:00.000 [INFO] User login success user_id=10001 ip=172.16.0.1 uuid=550e8400-e29b-41d4-a716-446655440000",
	"2026-09-12 14:01:05.000 [INFO] User login success user_id=10002 ip=172.16.0.2 addr=0x7fff5fbff8b0",
	"2026-09-12 14:02:00.000 [FATAL] Out of memory: Kill process 5432 (mysqld) score 850",
	"2026-09-12 14:03:00.000 [WARN] Response slow: payload=\"invalid token in header\" status=401",
}

func BenchmarkPreprocessLog(b *testing.B) {
	b.ReportAllocs()
	n := len(sampleLogs)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		line := sampleLogs[i%n]
		_, _, _ = PreprocessLog(line)
	}
}

func BenchmarkDrainAddLog(b *testing.B) {
	b.ReportAllocs()
	miner := NewDrainMiner(0.55, 4)
	n := len(sampleLogs)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		line := sampleLogs[i%n]
		miner.AddLog(line, "bench.log")
	}
}
