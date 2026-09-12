package indexer

import (
	"testing"
)

func TestPreprocessLogAndDrainClustering(t *testing.T) {
	miner := NewDrainMiner(0.5, 4)

	logs := []string{
		"2026-09-12 14:00:01.123 [ERROR] Failed to connect to 192.168.1.10:8080 timeout after 3000ms",
		"2026-09-12 14:00:02.456 [ERROR] Failed to connect to 192.168.1.11:8080 timeout after 3005ms",
		"2026-09-12 14:00:03.789 [ERROR] Failed to connect to 10.0.0.5:9000 timeout after 1500ms",
		"2026-09-12 14:01:00.000 [INFO] User login success user_id=10001 ip=172.16.0.1",
		"2026-09-12 14:01:05.000 [INFO] User login success user_id=10002 ip=172.16.0.2",
		"2026-09-12 14:02:00.000 [FATAL] Out of memory: Kill process 5432 (mysqld) score 850",
	}

	for _, l := range logs {
		miner.AddLog(l, "test.log")
	}

	templates := miner.GetTemplates()
	if len(templates) != 3 {
		t.Fatalf("expected 3 clusters, got %d", len(templates))
	}

	// 命中频次最高的应该是 Failed to connect 模板 (3 次)
	top := templates[0]
	if top.Count != 3 {
		t.Errorf("expected top template count 3, got %d", top.Count)
	}
	if top.Level != "ERROR" {
		t.Errorf("expected level ERROR, got %s", top.Level)
	}
}

func TestExtractTraceID(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"2026-09-12 10:00:00 [INFO] trace_id=c89f3a1b4e2d5c6f process request", "c89f3a1b4e2d5c6f"},
		{"2026-09-12 10:00:00 [ERROR] request_id: req-99881122 failed", "req-99881122"},
		{`{"time":"2026-09-12T10:00:00Z","trace_id":"trace-abc-123","msg":"ok"}`, "trace-abc-123"},
		{"Plain log line without trace", ""},
	}

	for _, tc := range tests {
		got := ExtractTraceID(tc.input)
		if got != tc.expected {
			t.Errorf("for %q, expected %q, got %q", tc.input, tc.expected, got)
		}
	}
}
