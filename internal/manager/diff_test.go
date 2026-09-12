package manager

import (
	"os"
	"path/filepath"
	"testing"

	"dist-log-analyzer/internal/config"
	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/store"
)

func TestCompareArchivesDiff(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "diff_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := config.DefaultConfig()
	cfg.DataDir = tmpDir
	s, err := store.NewStore(cfg)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer s.Close()

	srv := &Server{
		cfg:   cfg,
		store: s,
	}

	// 1. 创建基准包 A 的目录与日志
	dirA := filepath.Join(tmpDir, "extract_a")
	_ = os.MkdirAll(dirA, 0755)
	_ = os.WriteFile(filepath.Join(dirA, "app.log"), []byte(
		"2026-09-11 10:00:00 [INFO] service startup success\n"+
			"2026-09-11 10:01:00 [INFO] query processed normal\n",
	), 0644)

	arcA := &model.LogArchive{
		ID:          "arc_a",
		Filename:    "baseline_normal.tar.gz",
		Username:    "admin",
		ExtractPath: dirA,
	}
	_ = s.SaveArchive(arcA)

	// 2. 创建故障包 B 的目录与日志 (多了 crash.log，且包含 OOM 致命错误)
	dirB := filepath.Join(tmpDir, "extract_b")
	_ = os.MkdirAll(dirB, 0755)
	_ = os.WriteFile(filepath.Join(dirB, "app.log"), []byte(
		"2026-09-12 10:00:00 [INFO] service startup success\n"+
			"2026-09-12 10:01:00 [ERROR] Out of memory: Kill process 1234 (app)\n"+
			"2026-09-12 10:02:00 [ERROR] Out of memory: Kill process 5678 (worker)\n",
	), 0644)
	_ = os.WriteFile(filepath.Join(dirB, "crash.log"), []byte(
		"2026-09-12 10:03:00 [FATAL] Core dumped on SIGSEGV\n",
	), 0644)

	arcB := &model.LogArchive{
		ID:          "arc_b",
		Filename:    "incident_crash.tar.gz",
		Username:    "admin",
		ExtractPath: dirB,
	}
	_ = s.SaveArchive(arcB)

	// 模拟 B 的诊断报告
	_ = s.SaveReport(&model.DiagnosisReport{
		ArchiveID:   "arc_b",
		TotalEvents: 2,
		SeveritySummary: map[string]int{
			"FATAL": 1,
			"ERROR": 1,
		},
		Events: []model.DiagnosisEvent{
			{
				RuleName: "OOM Killer",
				Severity: model.SeverityFatal,
				FilePath: "app.log",
			},
		},
	})

	// 3. 执行对比
	diffRep, err := srv.CompareArchives("arc_a", "arc_b")
	if err != nil {
		t.Fatalf("CompareArchives failed: %v", err)
	}

	// 4. 验证对比结果
	if len(diffRep.AddedFiles) != 1 || diffRep.AddedFiles[0] != "crash.log" {
		t.Errorf("expected added file crash.log, got: %v", diffRep.AddedFiles)
	}

	if len(diffRep.NewFatalEvents) != 1 {
		t.Errorf("expected 1 new fatal event in B, got: %d", len(diffRep.NewFatalEvents))
	}

	// 验证挖掘出 B 中特有的新增日志模板
	if len(diffRep.NewTemplates) == 0 {
		t.Errorf("expected new anomaly templates in B, got none")
	} else {
		topTpl := diffRep.NewTemplates[0]
		if topTpl.SampleFile == "" {
			t.Errorf("expected top template SampleFile not empty, got %q", topTpl.SampleFile)
		}
		if topTpl.SampleLine <= 0 {
			t.Errorf("expected top template SampleLine > 0, got %d", topTpl.SampleLine)
		}
		if topTpl.ArchiveID != arcB.ID {
			t.Errorf("expected top template ArchiveID %q, got %q", arcB.ID, topTpl.ArchiveID)
		}
		t.Logf("✔ 样本定位验证通过: File=%s, Line=%d, ArchiveID=%s, Sample=%s",
			topTpl.SampleFile, topTpl.SampleLine, topTpl.ArchiveID, topTpl.Sample)
	}

	t.Logf("✔ 差分对比成功，总结: %s", diffRep.SummaryText)
}
