package manager

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dist-log-analyzer/internal/config"
	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/store"
)

func TestSnapshotExportAndReload(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ha_store_test_*")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// 1. 初始化主节点 Store 并写入初始数据
	primaryDataDir := filepath.Join(tmpDir, "primary_data")
	cfgPrimary := &config.Config{
		DataDir: primaryDataDir,
		Role:    "manager",
	}
	cfgPrimary.InitialAdmin.Username = "admin"
	cfgPrimary.InitialAdmin.Password = "admin123"

	stPrimary, err := store.NewStore(cfgPrimary)
	if err != nil {
		t.Fatalf("初始化主节点 Store 失败: %v", err)
	}
	defer stPrimary.Close()

	// 写入一条用户和一条规则
	testUser := &model.User{
		ID:       "usr_test_999",
		Username: "operator_jack",
		Role:     model.RoleUser,
		Status:   "active",
	}
	_ = stPrimary.SaveUser(testUser)

	testRule := &model.Rule{
		ID:          "rule_ceph_osd_down",
		Name:        "Ceph OSD 异常下线故障",
		StorageType: "Ceph",
		Severity:    "CRITICAL",
		Pattern:     "osd.[0-9]+ is down",
		Enabled:     true,
	}
	_ = stPrimary.SaveRule(testRule)

	// 2. 导出主节点快照
	var snapshotBuf bytes.Buffer
	written, err := stPrimary.ExportSnapshot(&snapshotBuf)
	if err != nil {
		t.Fatalf("导出快照失败: %v", err)
	}
	if written <= 0 || snapshotBuf.Len() == 0 {
		t.Fatalf("导出的快照数据为空！")
	}

	// 3. 初始化备节点 Store 并通过快照重载
	backupDataDir := filepath.Join(tmpDir, "backup_data")
	cfgBackup := &config.Config{
		DataDir: backupDataDir,
		Role:    "manager",
	}
	cfgBackup.InitialAdmin.Username = "admin"
	cfgBackup.InitialAdmin.Password = "admin123"

	stBackup, err := store.NewStore(cfgBackup)
	if err != nil {
		t.Fatalf("初始化备节点 Store 失败: %v", err)
	}
	defer stBackup.Close()

	// 此时备机上应该没有 operator_jack
	uBefore, _ := stBackup.GetUserByUsername("operator_jack")
	if uBefore != nil {
		t.Fatalf("重载前备节点不应存在测试用户")
	}

	// 4. 从快照原子重载
	reloadedBytes, err := stBackup.ReloadFromSnapshot(&snapshotBuf)
	if err != nil {
		t.Fatalf("备节点快照重载失败: %v", err)
	}
	if reloadedBytes != written {
		t.Fatalf("重载大小不一致: 预期 %d, 实际 %d", written, reloadedBytes)
	}

	// 5. 校验备机上已成功获得主节点的数据
	uAfter, err := stBackup.GetUserByUsername("operator_jack")
	if err != nil || uAfter == nil {
		t.Fatalf("备节点重载后未检索到测试用户: %v", err)
	}
	if uAfter.Username != "operator_jack" {
		t.Errorf("用户名不匹配: %s", uAfter.Username)
	}

	ruleAfter, err := stBackup.GetRule("rule_ceph_osd_down")
	if err != nil || ruleAfter == nil {
		t.Fatalf("备节点重载后未检索到测试规则: %v", err)
	}
	if ruleAfter.Severity != "CRITICAL" {
		t.Errorf("规则严重级别不匹配: %s", ruleAfter.Severity)
	}
}

func TestHAManagerStateAndFailover(t *testing.T) {
	cfg := &config.Config{
		HAMode:               "backup",
		PeerURL:              "http://127.0.0.1:18080",
		HeartbeatIntervalSec: 1,
		FailoverTimeoutSec:   3,
		NodeName:             "test-backup-node",
	}

	ha := NewHAManager(cfg, nil)
	if ha.IsActive() {
		t.Fatalf("backup 模式初始角色应为 standby, 实际为 active")
	}

	status := ha.GetStatus()
	if status.Role != "standby" || status.Mode != "backup" {
		t.Fatalf("HA 状态初始化错误: %+v", status)
	}

	// 模拟连续失败达到故障切换阀值
	ha.markPeerFailure("测试模拟对端超时", 3)
	ha.markPeerFailure("测试模拟对端超时", 3)
	if ha.IsActive() {
		t.Fatalf("未达到 3 次超时不应晋升")
	}

	// 第 3 次触发自动晋升 Failover
	ha.markPeerFailure("测试模拟对端超时", 3)
	if !ha.IsActive() {
		t.Fatalf("达到 3 次超时后应自动触发 Failover 晋升为 active")
	}
	if ha.GetStatus().FailoverCount != 1 {
		t.Fatalf("故障切换计数器应增加为 1, 实际: %d", ha.GetStatus().FailoverCount)
	}

	// 测试手动降级
	_ = ha.DemoteToStandby("测试手动降级")
	if ha.IsActive() {
		t.Fatalf("降级后角色应为 standby")
	}

	// 测试手动晋升
	_ = ha.PromoteToActive("测试手动晋升")
	if !ha.IsActive() {
		t.Fatalf("手动晋升后角色应为 active")
	}
}

func TestStandaloneMode(t *testing.T) {
	cfg := &config.Config{
		HAMode:   "standalone",
		NodeName: "standalone-node",
	}
	ha := NewHAManager(cfg, nil)
	if !ha.IsActive() {
		t.Fatalf("单节点独立模式应默认为 active 运行")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	ha.Start(ctx)

	status := ha.GetStatus()
	if status.Mode != "standalone" {
		t.Fatalf("模式错误: %s", status.Mode)
	}
}

// TestGatewayDetectionAndSplitBrainBlock 测试网关孤岛防脑裂拦截机制
func TestGatewayDetectionAndSplitBrainBlock(t *testing.T) {
	// 配置一个不可达的虚拟黑洞网关 IP (RFC 5737 TEST-NET-1)
	cfg := &config.Config{
		HAMode:             "backup",
		PeerURL:            "http://127.0.0.1:18080",
		GatewayIP:          "192.0.2.254", // 故意设置为不可达 IP
		EnableGatewayCheck: true,
		EnableWorkerQuorum: false,
	}

	ha := NewHAManager(cfg, nil)
	if ha.IsActive() {
		t.Fatalf("备节点初始应为 standby")
	}

	// 模拟连续对端超时达到阀值 (3次)
	ha.markPeerFailure("主备心跳超时", 3)
	ha.markPeerFailure("主备心跳超时", 3)
	ha.markPeerFailure("主备心跳超时", 3)

	// 此时应被防脑裂机制成功拦截！禁止晋升为 active！
	if ha.IsActive() {
		t.Fatalf("在网关无法连通(网络孤岛)情况下，防脑裂机制应阻断晋升，但节点却晋升为了 active！")
	}

	status := ha.GetStatus()
	if !status.SplitBrainBlocked {
		t.Fatalf("状态中 SplitBrainBlocked 字段应为 true")
	}
	if status.GatewayOnline {
		t.Fatalf("网关状态应为离线 false")
	}
	t.Logf("防脑裂拦截生效测试通过！拦截说明: %s", status.BlockedReason)
}

// TestDetectDefaultGateway 验证能从本机路由表中成功解析默认网关
func TestDetectDefaultGateway(t *testing.T) {
	gw := DetectDefaultGateway()
	t.Logf("系统当前探测到的默认网关 IP: %q", gw)
}

// TestWorkerQuorumCheckProtection 测试 Worker 多数派不足阻断晋升防脑裂
func TestWorkerQuorumCheckProtection(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "ha_quorum_test_*")
	defer os.RemoveAll(tmpDir)

	cfg := &config.Config{
		DataDir:            tmpDir,
		HAMode:             "backup",
		PeerURL:            "http://127.0.0.1:18080",
		EnableGatewayCheck: false, // 本次单独测试 Worker 多数派仲裁
		EnableWorkerQuorum: true,
	}
	cfg.InitialAdmin.Username = "admin"
	cfg.InitialAdmin.Password = "admin123"

	st, err := store.NewStore(cfg)
	if err != nil {
		t.Fatalf("创建 Store 失败: %v", err)
	}
	defer st.Close()

	// 注册 3 个 Worker 业务节点
	for i := 1; i <= 3; i++ {
		_ = st.SaveNode(&model.Node{
			ID:   fmt.Sprintf("worker_0%d", i),
			Name: fmt.Sprintf("worker-%d", i),
			IP:   "127.0.0.1",
			Port: 29080 + i, // 未启动的测试端口
			Role: "worker",
		})
	}

	ha := NewHAManager(cfg, st)
	if ha.IsActive() {
		t.Fatalf("备节点初始应为 standby")
	}

	// 触发心跳超时
	ha.markPeerFailure("测试超时", 3)
	ha.markPeerFailure("测试超时", 3)
	ha.markPeerFailure("测试超时", 3)

	// 此时连通 Worker 数为 0/3，未达多数派，必须阻断晋升！
	if ha.IsActive() {
		t.Fatalf("在未能连接多数派 Worker 节点时，防脑裂机制应阻断晋升，但节点却晋升为了 active！")
	}

	status := ha.GetStatus()
	if !status.SplitBrainBlocked {
		t.Fatalf("SplitBrainBlocked 应为 true")
	}
	if status.QuorumPassed {
		t.Fatalf("多数派判定应为 false")
	}
	t.Logf("Worker 多数派仲裁防脑裂拦截成功！拦截说明: %s (在线 Worker: %d/%d)",
		status.BlockedReason, status.WorkerQuorumOnline, status.WorkerQuorumTotal)
}

func TestDynamicHAConfigAndGatewayUpdate(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ha_dynamic_cfg_*")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := config.DefaultConfig()
	cfg.DataDir = tmpDir
	cfg.HAMode = "standalone"

	st, err := store.NewStore(cfg)
	if err != nil {
		t.Fatalf("创建 Store 失败: %v", err)
	}
	defer st.Close()

	ha := NewHAManager(cfg, st)
	if ha.mode != "standalone" {
		t.Fatalf("初始模式应为 standalone")
	}

	// 模拟管理界面动态提交高可用与网关新配置
	newCfg := config.HAConfig{
		HAMode:               "primary",
		PeerURL:              "http://192.168.1.88:8080",
		GatewayIP:            "127.0.0.1", // 本机可通
		EnableGatewayCheck:   true,
		EnableWorkerQuorum:   true,
		VIP:                  "192.168.1.222/24",
		VIPInterface:         "lo",
		HeartbeatIntervalSec: 3,
		FailoverTimeoutSec:   9,
		SyncIntervalSec:      7,
	}

	// 1. 持久化到 Store
	if err := st.SaveHAConfig(&newCfg); err != nil {
		t.Fatalf("SaveHAConfig 失败: %v", err)
	}

	saved, err := st.GetHAConfig()
	if err != nil || saved == nil {
		t.Fatalf("GetHAConfig 失败: %v", err)
	}
	if saved.GatewayIP != "127.0.0.1" || saved.HAMode != "primary" || saved.PeerURL != "http://192.168.1.88:8080" {
		t.Fatalf("持久化读取的数据不匹配: %+v", saved)
	}

	// 2. 动态热更新到 HAManager
	if err := ha.UpdateConfig(newCfg); err != nil {
		t.Fatalf("UpdateConfig 失败: %v", err)
	}

	status := ha.GetStatus()
	if status.Mode != "primary" {
		t.Fatalf("动态更新后模式应为 primary, 实际为 %s", status.Mode)
	}
	if status.PeerURL != "http://192.168.1.88:8080" {
		t.Fatalf("动态更新后对端地址不匹配: %s", status.PeerURL)
	}
	if status.GatewayIP != "127.0.0.1" {
		t.Fatalf("动态更新后网关 IP 应为 127.0.0.1, 实际为 %s", status.GatewayIP)
	}
	if status.VIP != "192.168.1.222/24" {
		t.Fatalf("动态更新后 VIP 不匹配: %s", status.VIP)
	}

	t.Logf("高可用与网关配置动态热更新测试通过: 模式=%s, 网关=%s, 对端=%s, VIP=%s",
		status.Mode, status.GatewayIP, status.PeerURL, status.VIP)
}

