package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dist-log-analyzer/internal/config"
	"dist-log-analyzer/internal/model"

	bolt "go.etcd.io/bbolt"
)

func TestReportColdHotSeparation(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "store_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir) // 保证清理临时文件

	cfg := &config.Config{
		DataDir: tempDir,
	}
	cfg.InitialAdmin.Username = "admin"
	cfg.InitialAdmin.Password = "admin123"
	s, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer s.Close()

	archiveID := "arc-12345"
	report := &model.DiagnosisReport{
		ArchiveID:   archiveID,
		UserID:      "u1",
		ArchiveName: "node1.tar.gz",
		Status:      "completed",
		TotalEvents: 2,
		Events: []model.DiagnosisEvent{
			{RuleID: "r1", MatchedContent: "disk full", LineNumber: 100},
			{RuleID: "r2", MatchedContent: "oom killer", LineNumber: 200},
		},
		HealthScore: 85,
		AnalyzedAt:  time.Now(),
	}

	// 1. 保存报告
	if err := s.SaveReport(report); err != nil {
		t.Fatalf("SaveReport failed: %v", err)
	}

	// 2. 检查磁盘文件是否存在
	targetFile := filepath.Join(tempDir, "reports", archiveID+".json")
	if _, err := os.Stat(targetFile); err != nil {
		t.Fatalf("expected report file to exist, but stat failed: %v", err)
	}

	// 3. 检查 BoltDB 中是否只保留了元数据（Events 应该为 nil 或空）
	err = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketReports)
		data := b.Get([]byte(archiveID))
		if data == nil {
			t.Fatalf("BoltDB meta not found")
		}
		var meta model.DiagnosisReport
		if err := json.Unmarshal(data, &meta); err != nil {
			t.Fatalf("unmarshal meta error: %v", err)
		}
		if len(meta.Events) != 0 {
			t.Fatalf("expected 0 events in BoltDB meta, got %d", len(meta.Events))
		}
		if meta.HealthScore != 85 || meta.ArchiveID != archiveID {
			t.Fatalf("meta data mismatch: %+v", meta)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// 4. GetReport 读取，应该能完整还原 Events
	fetched, err := s.GetReport(archiveID)
	if err != nil {
		t.Fatalf("GetReport failed: %v", err)
	}
	if len(fetched.Events) != 2 || fetched.Events[0].RuleID != "r1" {
		t.Fatalf("fetched events mismatch: %+v", fetched.Events)
	}

	// 5. 模拟历史兼容性：删除独立文件，只保留 BoltDB 数据
	_ = os.Remove(targetFile)
	fetchedLegacy, err := s.GetReport(archiveID)
	if err != nil {
		t.Fatalf("legacy fallback GetReport failed: %v", err)
	}
	if fetchedLegacy.ArchiveID != archiveID {
		t.Fatalf("legacy fallback mismatch")
	}

	// 6. DeleteReport
	// 先重新保存文件再删除
	if err := s.SaveReport(report); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteReport(archiveID); err != nil {
		t.Fatalf("DeleteReport failed: %v", err)
	}
	if _, err := os.Stat(targetFile); !os.IsNotExist(err) {
		t.Fatalf("expected report file to be deleted, but still exists")
	}
	_, err = s.GetReport(archiveID)
	if err == nil {
		t.Fatalf("expected GetReport to fail after delete, but got nil error")
	}
}
