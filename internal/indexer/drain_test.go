package indexer

import (
	"strings"
	"testing"

	"dist-log-analyzer/internal/model"
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

func TestLogLevelAbbreviations(t *testing.T) {
	cases := []struct {
		log           string
		expectedLevel string
	}{
		{"2026-09-13 12:00:00 [ERR] disk write failed", "ERROR"},
		{"2026-09-13 12:00:00 [ERROR] disk write failed", "ERROR"},
		{"2026-09-13 12:00:00 [CRIT] heartbeat lost to peer", "FATAL"},
		{"2026-09-13 12:00:00 [FATAL] heartbeat lost to peer", "FATAL"},
		{"2026-09-13 12:00:00 [FTL] crash detected", "FATAL"},
		{"2026-09-13 12:00:00 [WRN] latency is high", "WARN"},
		{"2026-09-13 12:00:00 [WARNING] latency is high", "WARN"},
		{"2026-09-13 12:00:00 [INF] server started", "INFO"},
		{"2026-09-13 12:00:00 [DBG] processing packet", "DEBUG"},
	}

	for _, c := range cases {
		_, lvl, _ := PreprocessLog(c.log)
		if lvl != c.expectedLevel {
			t.Errorf("log %q: expected level %s, got %s", c.log, c.expectedLevel, lvl)
		}
	}
}

func TestPreprocessCustomRules(t *testing.T) {
	customRules := []*model.PreprocessRule{
		{
			ID:          "rule_tenant_mask",
			Name:        "租户ID通配",
			Type:        model.PreprocessTypeMask,
			Pattern:     `\btenant_[0-9a-zA-Z]+\b`,
			Replacement: "<*>",
			Enabled:     true,
		},
		{
			ID:          "rule_custom_fatal",
			Name:        "特定panic匹配为FATAL",
			Type:        model.PreprocessTypeLevelMapping,
			Pattern:     `(?i)runtime error: panic`,
			Replacement: "FATAL",
			Enabled:     true,
		},
	}

	raw1 := "2026-09-13 12:00:00 [INFO] handling request for tenant_98234a on cluster"
	cleaned1, lvl1, _ := PreprocessLogWithRules(raw1, customRules)
	if lvl1 != "INFO" {
		t.Errorf("expected level INFO, got %s", lvl1)
	}
	if !strings.Contains(cleaned1, "<*>") || strings.Contains(cleaned1, "tenant_98234a") {
		t.Errorf("expected tenant_98234a masked to <*>, got: %s", cleaned1)
	}

	raw2 := "2026-09-13 12:00:00 [INFO] caught runtime error: panic in thread main"
	_, lvl2, _ := PreprocessLogWithRules(raw2, customRules)
	if lvl2 != "FATAL" {
		t.Errorf("expected level overridden to FATAL, got %s", lvl2)
	}
}
