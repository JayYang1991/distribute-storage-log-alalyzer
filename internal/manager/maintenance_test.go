package manager

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dist-log-analyzer/internal/config"
	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/store"

	bolt "go.etcd.io/bbolt"
)

func TestNodeMaintenanceMode(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "maintenance_test_*")
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

	sc := NewScheduler(s)

	node1 := &model.Node{
		ID:            "worker_node_1",
		Name:          "worker-01",
		Role:          "worker",
		Status:        "online",
		LastHeartbeat: time.Now(),
		Resource: model.SystemResource{
			DiskTotalMB: 10000,
			DiskUsedMB:  1000,
			DiskFreeMB:  9000,
		},
	}
	_ = s.SaveNode(node1)

	// 1. 正常在线时应能被 PickLowestUsageWorker 选中
	picked := sc.PickLowestUsageWorker(1024 * 1024 * 100)
	if picked == nil || picked.ID != "worker_node_1" {
		t.Fatalf("online node should be picked, but got: %v", picked)
	}

	// 2. 置为维护模式 (Drain)
	node1.Status = "maintenance"
	_ = s.SaveNode(node1)

	pickedAfterMaintenance := sc.PickLowestUsageWorker(1024 * 1024 * 100)
	if pickedAfterMaintenance != nil {
		t.Fatalf("maintenance node must NOT be picked for new uploads, but got: %v", pickedAfterMaintenance)
	}

	// 3. 恢复上线
	node1.Status = "online"
	_ = s.SaveNode(node1)

	pickedOnline := sc.PickLowestUsageWorker(1024 * 1024 * 100)
	if pickedOnline == nil || pickedOnline.ID != "worker_node_1" {
		t.Fatalf("node should be picked again after recovery to online, but got: %v", pickedOnline)
	}
}

func TestDatabaseBackupSnapshot(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "backup_test_*")
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

	// 写入测试节点
	_ = s.SaveNode(&model.Node{
		ID:     "node_backup_test",
		Name:   "worker-backup",
		Role:   "worker",
		Status: "online",
	})

	// 导出快照
	var buf bytes.Buffer
	if err := s.BackupSnapshot(&buf); err != nil {
		t.Fatalf("failed to export backup snapshot: %v", err)
	}

	if buf.Len() == 0 {
		t.Fatalf("exported backup snapshot is empty")
	}

	// 将快照落地到临时文件并使用 bbolt 验证完整性
	snapshotPath := filepath.Join(tmpDir, "snapshot.db")
	if err := os.WriteFile(snapshotPath, buf.Bytes(), 0600); err != nil {
		t.Fatalf("failed to write snapshot file: %v", err)
	}

	snapshotDB, err := bolt.Open(snapshotPath, 0600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		t.Fatalf("snapshot file is not a valid bbolt database: %v", err)
	}
	defer snapshotDB.Close()

	// 验证快照中的数据一致性
	err = snapshotDB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("nodes"))
		if b == nil {
			t.Fatalf("bucket 'nodes' missing in snapshot")
		}
		data := b.Get([]byte("node_backup_test"))
		if data == nil {
			t.Fatalf("test node missing in snapshot")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to read from snapshot: %v", err)
	}
}
