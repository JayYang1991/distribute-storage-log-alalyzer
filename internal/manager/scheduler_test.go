package manager

import (
	"os"
	"testing"
	"time"

	"dist-log-analyzer/internal/config"
	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/store"
)

func TestPickLowestUsageWorker(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "scheduler_test_*")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := config.DefaultConfig()
	cfg.DataDir = tmpDir
	st, err := store.NewStore(cfg)
	if err != nil {
		t.Fatalf("创建 Store 失败: %v", err)
	}
	defer st.Close()

	sc := NewScheduler(st)

	now := time.Now()

	// 创建3个在线 Worker 节点
	// Node 1: 已用 5000 MB，剩余 20000 MB
	w1 := &model.Node{
		ID:            "worker-1",
		Name:          "worker-node-1",
		Role:          "worker",
		Status:        "online",
		IP:            "192.168.1.101",
		Port:          9002,
		LastHeartbeat: now,
		Resource: model.SystemResource{
			DiskTotalMB:     25000,
			DiskUsedMB:      5000,
			DiskFreeMB:      20000,
			DiskUsedPercent: 20.0,
		},
	}

	// Node 2: 已用 1000 MB，剩余 30000 MB (已用容量最低)
	w2 := &model.Node{
		ID:            "worker-2",
		Name:          "worker-node-2",
		Role:          "worker",
		Status:        "online",
		IP:            "192.168.1.102",
		Port:          9002,
		LastHeartbeat: now,
		Resource: model.SystemResource{
			DiskTotalMB:     31000,
			DiskUsedMB:      1000,
			DiskFreeMB:      30000,
			DiskUsedPercent: 3.2,
		},
	}

	// Node 3: 已用 15000 MB，剩余 5000 MB
	w3 := &model.Node{
		ID:            "worker-3",
		Name:          "worker-node-3",
		Role:          "worker",
		Status:        "online",
		IP:            "192.168.1.103",
		Port:          9002,
		LastHeartbeat: now,
		Resource: model.SystemResource{
			DiskTotalMB:     20000,
			DiskUsedMB:      15000,
			DiskFreeMB:      5000,
			DiskUsedPercent: 75.0,
		},
	}

	_ = st.SaveNode(w1)
	_ = st.SaveNode(w2)
	_ = st.SaveNode(w3)

	// 1. 验证优先挑选已使用容量最低的节点 (应选中 worker-2)
	best := sc.PickLowestUsageWorker(0)
	if best == nil || best.ID != "worker-2" {
		t.Fatalf("预期选择已用容量最低的 worker-2，但实际得到: %v", best)
	}

	// 2. 模拟 worker-2 发生磁盘只读告警 (只读故障保护)，应排除 worker-2，选择次低的 worker-1
	err = st.SaveAlarm(&model.Alarm{
		ID:        "alarm-ro-worker-2",
		NodeID:    "worker-2",
		NodeName:  "worker-node-2",
		Component: "worker",
		AlarmType: model.AlarmTypeDiskReadOnly,
		Severity:  model.SeverityCritical,
		Title:     "文件系统只读",
		Message:   "磁盘变为只读模式",
		Status:    model.AlarmStatusActive,
	})
	if err != nil {
		t.Fatalf("保存告警失败: %v", err)
	}

	best = sc.PickLowestUsageWorker(0)
	if best == nil || best.ID != "worker-1" {
		t.Fatalf("worker-2 出现只读告警后，预期回退至 worker-1，但实际得到: %v", best)
	}

	// 清除告警
	_, _ = st.ResolveAlarm("worker-2", model.AlarmTypeDiskReadOnly)

	// 3. 验证空间需求检查：若上传一个 25GB (25600MB) 的超大日志包
	// worker-2 (剩余 30000MB) 满足条件
	best = sc.PickLowestUsageWorker(25600 * 1024 * 1024)
	if best == nil || best.ID != "worker-2" {
		t.Fatalf("25GB 日志包应分配至剩余 30GB 的 worker-2，实际得到: %v", best)
	}

	// 若上传一个 28GB (28672MB) 的日志包，只有 worker-2 容得下
	best = sc.PickLowestUsageWorker(28672 * 1024 * 1024)
	if best == nil || best.ID != "worker-2" {
		t.Fatalf("28GB 日志包应只能由 worker-2 承载，实际得到: %v", best)
	}

	// 若上传一个 35GB 的日志包，所有节点剩余容量都不足，应返回 nil
	best = sc.PickLowestUsageWorker(35840 * 1024 * 1024)
	if best != nil {
		t.Fatalf("空间均不足时应返回 nil，实际得到: %v", best)
	}

	// 4. 验证心跳超时过滤：若 worker-2 心跳超时（>15秒）
	w2.LastHeartbeat = now.Add(-30 * time.Second)
	_ = st.SaveNode(w2)

	best = sc.PickLowestUsageWorker(0)
	if best == nil || best.ID != "worker-1" {
		t.Fatalf("worker-2 离线后，预期选择在线且容量最低的 worker-1，实际得到: %v", best)
	}
}

func TestIsWorkerCapacityLower(t *testing.T) {
	// 测试容量比较逻辑
	// 情况 A: 物理磁盘已用 MB 不同
	nA := &model.Node{ID: "A", Resource: model.SystemResource{DiskUsedMB: 200, DiskUsedPercent: 10}}
	nB := &model.Node{ID: "B", Resource: model.SystemResource{DiskUsedMB: 500, DiskUsedPercent: 25}}
	if !isWorkerCapacityLower(nA, nB) {
		t.Errorf("nA(200MB) 应该比 nB(500MB) 容量更低")
	}
	if isWorkerCapacityLower(nB, nA) {
		t.Errorf("nB(500MB) 不应该比 nA(200MB) 容量更低")
	}

	// 情况 B: 物理磁盘已用 MB 相同，比较百分比
	nC := &model.Node{ID: "C", Resource: model.SystemResource{DiskUsedMB: 200, DiskUsedPercent: 10}}
	nD := &model.Node{ID: "D", Resource: model.SystemResource{DiskUsedMB: 200, DiskUsedPercent: 20}}
	if !isWorkerCapacityLower(nC, nD) {
		t.Errorf("nC(10%%) 应该比 nD(20%%) 优先")
	}

	// 情况 C: 未上报物理磁盘，比较应用日志存储 StorageUsedBytes
	nE := &model.Node{ID: "E", StorageUsedBytes: 1024}
	nF := &model.Node{ID: "F", StorageUsedBytes: 2048}
	if !isWorkerCapacityLower(nE, nF) {
		t.Errorf("nE(1024B) 应该比 nF(2048B) 容量更低")
	}

	// 情况 D: 容量一致，比较可用空间 (可用空间大的优先)
	nG := &model.Node{ID: "G", Resource: model.SystemResource{DiskFreeMB: 8000}}
	nH := &model.Node{ID: "H", Resource: model.SystemResource{DiskFreeMB: 5000}}
	if !isWorkerCapacityLower(nG, nH) {
		t.Errorf("nG(8000MB free) 应该比 nH(5000MB free) 更优先")
	}
}
