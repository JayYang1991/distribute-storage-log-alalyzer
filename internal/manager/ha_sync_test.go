package manager

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"dist-log-analyzer/internal/config"
	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/store"
)

func TestHASync304Conditional(t *testing.T) {
	tempDir1, err := os.MkdirTemp("", "ha_sync_pri_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir1)

	tempDir2, err := os.MkdirTemp("", "ha_sync_sec_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir2)

	cfg1 := &config.Config{
		DataDir:      tempDir1,
		ClusterToken: "test-token-xyz",
	}
	cfg1.InitialAdmin.Username = "admin"
	cfg1.InitialAdmin.Password = "admin123"

	cfg2 := &config.Config{
		DataDir:      tempDir2,
		ClusterToken: "test-token-xyz",
	}
	cfg2.InitialAdmin.Username = "admin"
	cfg2.InitialAdmin.Password = "admin123"

	st1, err := store.NewStore(cfg1)
	if err != nil {
		t.Fatal(err)
	}
	defer st1.Close()

	st2, err := store.NewStore(cfg2)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	// 创建主节点 Server
	priServer := &Server{
		cfg:   cfg1,
		store: st1,
	}

	ts := httptest.NewServer(http.HandlerFunc(priServer.handleHASnapshot))
	defer ts.Close()

	haCfg := &config.Config{
		HAMode:       "backup",
		ClusterToken: "test-token-xyz",
		PeerURL:      ts.URL,
	}
	haMgr := &HAManager{
		cfg:     haCfg,
		peerURL: ts.URL,
		role:    "standby",
	}

	syncEngine := NewHASyncEngine(haMgr, st2)

	// 1. 首次同步：此时备机 lastTxID 为 0，主节点应当返回 200 OK，同步成功
	syncEngine.syncOnce()
	if syncEngine.syncStatus != "synced" {
		t.Fatalf("first sync failed: status=%s, err=%v", syncEngine.syncStatus, syncEngine.syncErr)
	}
	if syncEngine.lastTxID <= 0 {
		t.Fatalf("expected lastTxID > 0, got %d", syncEngine.lastTxID)
	}
	initialTxID := syncEngine.lastTxID

	// 2. 二次同步：主节点没有任何写事务，TxID 保持不变，应当触发 304 Not Modified
	syncEngine.lastSyncBytes = -1 // 重置以检验是否跳过写入
	syncEngine.syncOnce()
	if syncEngine.syncStatus != "synced" {
		t.Fatalf("second sync failed: status=%s, err=%v", syncEngine.syncStatus, syncEngine.syncErr)
	}
	if syncEngine.lastSyncBytes != -1 {
		t.Fatalf("expected 304 to skip DB reload (lastSyncBytes should remain -1), but got %d", syncEngine.lastSyncBytes)
	}
	if syncEngine.lastTxID != initialTxID {
		t.Fatalf("TxID should not change, got %d vs %d", syncEngine.lastTxID, initialTxID)
	}

	// 3. 在主节点写入新数据，触发写事务使 TxID 递增
	err = st1.SaveRule(&model.Rule{
		ID:          "rule-test-ha",
		Name:        "Test Rule",
		StorageType: "all",
		Pattern:     "corrupt",
		Severity:    "critical",
		Enabled:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 4. 再次同步：TxID 已变，应当返回 200 并重载快照
	syncEngine.syncOnce()
	if syncEngine.syncStatus != "synced" {
		t.Fatalf("third sync failed: status=%s, err=%v", syncEngine.syncStatus, syncEngine.syncErr)
	}
	if syncEngine.lastSyncBytes <= 0 {
		t.Fatalf("expected reload bytes > 0, got %d", syncEngine.lastSyncBytes)
	}
	if syncEngine.lastTxID <= initialTxID {
		t.Fatalf("expected new TxID > %d, got %d", initialTxID, syncEngine.lastTxID)
	}
}
