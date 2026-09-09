package rules

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"dist-log-analyzer/internal/model"
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

func TestMatchFilePath(t *testing.T) {
	tests := []struct {
		pattern string
		relPath string
		expect  bool
	}{
		// 1. 空 pattern 匹配任意路径
		{"", "ceph/ceph-osd.0.log", true},
		{"   ", "sys/dmesg.log", true},

		// 2. 单纯文件名精确匹配
		{"dmesg.log", "sys/dmesg.log", true},
		{"dmesg.log", "dmesg.log", true},
		{"dmesg.log", "var/log/dmesg.log", true},
		{"dmesg.log", "sys/ceph-osd.log", false},

		// 3. 通配符文件名匹配
		{"*.log", "ceph/ceph-osd.0.log", true},
		{"ceph-*.log", "ceph/ceph-osd.0.log", true},
		{"ceph-*.log", "ceph/ceph-mon.log", true},
		{"ceph-*.log", "sys/dmesg.log", false},

		// 4. 相对全路径与路径通配符匹配
		{"sys/dmesg.log", "sys/dmesg.log", true},
		{"sys/dmesg.log", "root/sys/dmesg.log", true},
		{"sys/dmesg.log", "other/dmesg.log", false},
		{"ceph/*.log", "ceph/ceph-osd.0.log", true},
		{"ceph/*.log", "hdfs/hadoop.log", false},

		// 5. 逗号/分号多模式候选
		{"dmesg.log, syslog, messages", "sys/dmesg.log", true},
		{"dmesg.log; syslog; messages", "var/log/syslog", true},
		{"dmesg.log, syslog", "ceph/ceph-osd.0.log", false},

		// 6. 大小写不敏感
		{"DMESG.LOG", "sys/dmesg.log", true},
		{"Ceph/*.LOG", "ceph/ceph-osd.0.log", true},
	}

	for _, tt := range tests {
		got := MatchFilePath(tt.pattern, tt.relPath)
		if got != tt.expect {
			t.Errorf("MatchFilePath(%q, %q) = %v, 期望 %v", tt.pattern, tt.relPath, got, tt.expect)
		}
	}
}

func TestDiagnoseDirectoryWithFilePathPattern(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "rules_path_scope_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	// 构造测试文件
	cephDir := filepath.Join(tempDir, "ceph")
	hdfsDir := filepath.Join(tempDir, "hdfs")
	sysDir := filepath.Join(tempDir, "sys")
	_ = os.MkdirAll(cephDir, 0755)
	_ = os.MkdirAll(hdfsDir, 0755)
	_ = os.MkdirAll(sysDir, 0755)

	_ = os.WriteFile(filepath.Join(cephDir, "ceph-osd.0.log"), []byte("2026-09-09 10:00:01 [ERROR] osd.0 marked down\n"), 0644)
	_ = os.WriteFile(filepath.Join(hdfsDir, "hadoop-datanode.log"), []byte("2026-09-09 10:00:02 [ERROR] DataNode-1 is dead\n"), 0644)
	_ = os.WriteFile(filepath.Join(sysDir, "dmesg.log"), []byte("2026-09-09 10:00:03 [CRITICAL] blk_update_request: I/O error\n"), 0644)

	customRules := []*model.Rule{
		{
			ID:              "rule_scoped_ceph",
			Name:            "Ceph OSD 下线 (限定 ceph-osd*.log 文件名)",
			StorageType:     "Ceph",
			Severity:        "FATAL",
			Pattern:         "marked down",
			FilePathPattern: "ceph-osd*.log",
			Enabled:         true,
		},
		{
			ID:              "rule_scoped_hdfs",
			Name:            "HDFS 节点死亡 (限定 hdfs/*.log 路径通配)",
			StorageType:     "HDFS",
			Severity:        "CRITICAL",
			Pattern:         "DataNode.*is dead",
			IsRegex:         true,
			FilePathPattern: "hdfs/*.log",
			Enabled:         true,
		},
		{
			ID:              "rule_scoped_sys",
			Name:            "系统 IO 错误 (限定全路径 sys/dmesg.log)",
			StorageType:     "Generic",
			Severity:        "CRITICAL",
			Pattern:         "I/O error",
			FilePathPattern: "sys/dmesg.log",
			Enabled:         true,
		},
		{
			ID:              "rule_mismatched_scope",
			Name:            "故意限定不匹配的文件路径 (验证过滤拦截)",
			StorageType:     "Ceph",
			Severity:        "FATAL",
			Pattern:         "marked down",
			FilePathPattern: "not_exist_file.log",
			Enabled:         true,
		},
	}

	engine := NewEngine(customRules)
	report, err := engine.DiagnoseDirectory("arc_test_scope", "usr_01", "test.tar.gz", tempDir)
	if err != nil {
		t.Fatalf("DiagnoseDirectory error: %v", err)
	}

	if len(report.Events) != 3 {
		t.Fatalf("期望命中 3 个限定作用域的事件，实际命中: %d 条", len(report.Events))
	}

	for _, ev := range report.Events {
		switch ev.RuleID {
		case "rule_scoped_ceph":
			if ev.FilePath != "ceph/ceph-osd.0.log" {
				t.Errorf("rule_scoped_ceph 命中错误文件: %s", ev.FilePath)
			}
		case "rule_scoped_hdfs":
			if ev.FilePath != "hdfs/hadoop-datanode.log" {
				t.Errorf("rule_scoped_hdfs 命中错误文件: %s", ev.FilePath)
			}
		case "rule_scoped_sys":
			if ev.FilePath != "sys/dmesg.log" {
				t.Errorf("rule_scoped_sys 命中错误文件: %s", ev.FilePath)
			}
		case "rule_mismatched_scope":
			t.Errorf("rule_mismatched_scope 路径不匹配却错误触发了诊断事件！")
		}
	}
	t.Log("✔ 规则指定文件名或全路径精准作用域过滤与诊断验证通过！")
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
