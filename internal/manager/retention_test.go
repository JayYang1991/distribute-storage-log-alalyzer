package manager

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"dist-log-analyzer/internal/config"
	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/store"
)

func TestArchivePinningAndRetention(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "retention_test_*")
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

	// 1. 创建归档包并测试锁定保护
	arc := &model.LogArchive{
		ID:         "arc_retention_1",
		Filename:   "syslog-2026.tar.gz",
		Username:   "admin",
		Size:       1024 * 1024 * 10,
		Pinned:     true,
		UploadTime: time.Now().AddDate(0, 0, -40), // 40 天前上传
	}
	if err := s.SaveArchive(arc); err != nil {
		t.Fatalf("failed to save archive: %v", err)
	}

	// 配置保留天数 30 天
	retCfg := &model.RetentionConfig{
		RetentionDays:            30,
		HighWatermarkPercent:     85,
		EmergencyWatermarkPercent: 92,
		TargetWatermarkPercent:   75,
		AutoCleanEnabled:         true,
	}
	if err := s.SaveRetentionConfig(retCfg); err != nil {
		t.Fatalf("failed to save retention config: %v", err)
	}

	// 运行一次巡检：由于 arc 处于 Pinned 保护状态，即使超过 30 天也不应被清理
	srv.RunRetentionCycle()

	fetched, err := s.GetArchive("arc_retention_1")
	if err != nil || fetched == nil {
		t.Fatalf("pinned archive should NOT be deleted by retention cycle")
	}

	// 2. 解除锁定后再次巡检，应被平滑清理
	if _, err := s.SetArchivePinned("arc_retention_1", false); err != nil {
		t.Fatalf("failed to unpin archive: %v", err)
	}

	srv.RunRetentionCycle()

	afterClean, _ := s.GetArchive("arc_retention_1")
	if afterClean != nil {
		t.Fatalf("unpinned expired archive should have been cleaned by retention cycle")
	}
}

func TestEmergencyWatermarkSelfHealing(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "watermark_test_*")
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

	// 模拟一个磁盘使用率达到 95% 的 Worker 节点
	node := &model.Node{
		ID:     "worker_test_1",
		Name:   "worker-01",
		Status: "online",
		Resource: model.SystemResource{
			DiskTotalMB:     10000,
			DiskUsedMB:      9500,
			DiskUsedPercent: 95.0, // 超过 92% 紧急水位
		},
	}
	_ = s.SaveNode(node)

	// 创建两个归档包：一个锁定，一个未锁定
	extractDir := filepath.Join(tmpDir, "extract_arc2")
	_ = os.MkdirAll(extractDir, 0755)

	arc1 := &model.LogArchive{
		ID:            "arc_wm_1",
		Filename:      "critical_pinned.tar.gz",
		StorageNodeID: "worker_test_1",
		Size:          1024 * 1024 * 1000, // 1000MB
		Pinned:        true,
		UploadTime:    time.Now().Add(-2 * time.Hour),
	}
	arc2 := &model.LogArchive{
		ID:            "arc_wm_2",
		Filename:      "regular_unpinned.tar.gz",
		StorageNodeID: "worker_test_1",
		Size:          1024 * 1024 * 2500, // 2500MB
		ExtractPath:   extractDir,
		Pinned:        false,
		UploadTime:    time.Now().Add(-1 * time.Hour),
	}
	_ = s.SaveArchive(arc1)
	_ = s.SaveArchive(arc2)

	// 运行自愈巡检
	srv.RunRetentionCycle()

	// 验证：arc1 (Pinned) 依然完好保留，arc2 (未保护) 已被清理释放
	if a1, _ := s.GetArchive("arc_wm_1"); a1 == nil {
		t.Errorf("pinned archive arc1 should NOT be deleted during emergency cleanup")
	}
	if a2, _ := s.GetArchive("arc_wm_2"); a2 != nil {
		t.Errorf("unpinned archive arc2 should be cleaned to reduce watermark")
	}
}

func TestDefaultTTLAndExemptTagsRetention(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ttl_exempt_test_*")
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

	// 1. 验证默认生命周期配置为 180 天 (6 个月)
	defaultCfg, err := s.GetRetentionConfig()
	if err != nil {
		t.Fatalf("failed to get default retention config: %v", err)
	}
	if defaultCfg.RetentionDays != 180 {
		t.Errorf("expected default retention days to be 180 (6 months), got %d", defaultCfg.RetentionDays)
	}
	if !defaultCfg.AutoCleanEnabled {
		t.Errorf("expected auto clean to be enabled by default")
	}

	// 2. 模拟管理员配置指导免清理标签
	defaultCfg.ExemptTags = []string{"永久保留", "重要故障"}
	if err := s.SaveRetentionConfig(defaultCfg); err != nil {
		t.Fatalf("failed to save retention config: %v", err)
	}

	// 3. 创建 3 个超期日志包 (200 天前上传，超过 180 天保留期)
	// (a) 普通日志包：无锁定、无免清理标签 -> 应被自动清理
	arcNormal := &model.LogArchive{
		ID:         "arc_normal",
		Filename:   "normal_old.tar.gz",
		Username:   "admin",
		Size:       1024 * 1024 * 5,
		Pinned:     false,
		Tags:       []string{"普通调试", "开发测试"},
		UploadTime: time.Now().AddDate(0, 0, -200),
	}
	// (b) 管理员锁定保护日志包 -> 应被绝对保留
	arcPinned := &model.LogArchive{
		ID:         "arc_pinned",
		Filename:   "pinned_audit.tar.gz",
		Username:   "admin",
		Size:       1024 * 1024 * 5,
		Pinned:     true,
		UploadTime: time.Now().AddDate(0, 0, -200),
	}
	// (c) 携带管理员免清理指导标签日志包 -> 应被绝对保留
	arcExempt := &model.LogArchive{
		ID:         "arc_exempt",
		Filename:   "ceph_critical_incident.tar.gz",
		Username:   "admin",
		Size:       1024 * 1024 * 5,
		Pinned:     false,
		Tags:       []string{"Ceph", "重要故障"},
		UploadTime: time.Now().AddDate(0, 0, -200),
	}

	_ = s.SaveArchive(arcNormal)
	_ = s.SaveArchive(arcPinned)
	_ = s.SaveArchive(arcExempt)

	// 4. 执行生命周期与自愈巡检
	srv.RunRetentionCycle()

	// 5. 校验结果
	if a, _ := s.GetArchive("arc_normal"); a != nil {
		t.Errorf("expired normal archive arc_normal should have been cleaned by 180-day TTL")
	}
	if a, _ := s.GetArchive("arc_pinned"); a == nil {
		t.Errorf("pinned archive arc_pinned should NOT be cleaned")
	}
	if a, _ := s.GetArchive("arc_exempt"); a == nil {
		t.Errorf("archive arc_exempt with exempt tag should NOT be cleaned by auto retention")
	}
}
