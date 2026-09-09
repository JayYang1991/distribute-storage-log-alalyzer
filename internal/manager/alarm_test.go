package manager

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"dist-log-analyzer/internal/config"
	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/store"
)

func TestAlarmLifecycleAndAggregation(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "alarm_test_*")
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

	nodeID := "worker_node_test_01"

	// 1. 模拟业务组件首次上报磁盘满异常 (DISK_FULL)
	alm1 := &model.Alarm{
		NodeID:    nodeID,
		NodeName:  "worker-node-test-01",
		NodeIP:    "192.168.1.50",
		Component: "worker",
		AlarmType: model.AlarmTypeDiskFull,
		Severity:  model.SeverityCritical,
		Title:     "业务存储磁盘空间严重不足",
		Message:   "可用磁盘空间剩余 500 MB (< 1GB)",
	}

	created1, err := st.CreateOrAggregateAlarm(alm1)
	if err != nil {
		t.Fatalf("首次创建告警失败: %v", err)
	}
	if created1.Count != 1 || created1.Status != model.AlarmStatusActive {
		t.Fatalf("首次告警状态或计数异常: count=%d, status=%s", created1.Count, created1.Status)
	}

	// 2. 模拟业务组件 5 秒后再次上报同类型告警 (聚合防风暴测试)
	alm2 := &model.Alarm{
		NodeID:    nodeID,
		AlarmType: model.AlarmTypeDiskFull,
		Severity:  model.SeverityCritical,
		Title:     "业务存储磁盘空间严重不足",
		Message:   "可用磁盘空间剩余 480 MB (< 1GB)",
	}
	aggregated, err := st.CreateOrAggregateAlarm(alm2)
	if err != nil {
		t.Fatalf("聚合告警失败: %v", err)
	}
	if aggregated.ID != created1.ID {
		t.Fatalf("同类活跃告警应该复用相同告警ID进行聚合，但生成了不同ID: %s != %s", aggregated.ID, created1.ID)
	}
	if aggregated.Count != 2 {
		t.Fatalf("告警频次应累加为 2，实际为: %d", aggregated.Count)
	}
	if aggregated.Message != "可用磁盘空间剩余 480 MB (< 1GB)" {
		t.Fatalf("告警信息应更新为最新说明，实际为: %s", aggregated.Message)
	}

	// 3. 上报另一个类型的告警 (HIGH_MEMORY)
	almMem := &model.Alarm{
		NodeID:    nodeID,
		NodeName:  "worker-node-test-01",
		NodeIP:    "192.168.1.50",
		Component: "worker",
		AlarmType: model.AlarmTypeHighMemory,
		Severity:  model.SeverityWarning,
		Title:     "业务计算节点内存占用过高 (95%)",
		Message:   "内存使用超高",
	}
	_, _ = st.CreateOrAggregateAlarm(almMem)

	// 4. 统计告警指标
	sum := st.GetAlarmSummary()
	if sum.TotalActive != 2 || sum.CriticalCount != 1 || sum.WarningCount != 1 {
		t.Fatalf("告警指标统计不符: %+v", sum)
	}

	// 5. 自动解除 DISK_FULL 告警
	resolved, err := st.ResolveAlarm(nodeID, model.AlarmTypeDiskFull)
	if err != nil {
		t.Fatalf("ResolveAlarm 失败: %v", err)
	}
	if resolved.Status != model.AlarmStatusResolved || resolved.ResolvedAt == nil {
		t.Fatalf("告警状态应变为 resolved 且有 ResolvedAt: %+v", resolved)
	}

	// 再次统计，活跃告警应仅剩 1 个
	sumAfter := st.GetAlarmSummary()
	if sumAfter.TotalActive != 1 {
		t.Fatalf("消警后活跃告警应为 1，实际为: %d", sumAfter.TotalActive)
	}

	// 6. 管理员确认与手动消警
	list, _ := st.ListAlarms("active")
	if len(list) != 1 {
		t.Fatalf("活跃告警列表数量应为 1，实际为: %d", len(list))
	}
	if err := st.AcknowledgeAlarm(list[0].ID); err != nil {
		t.Fatalf("AcknowledgeAlarm 失败: %v", err)
	}
	if err := st.ManualResolveAlarm(list[0].ID); err != nil {
		t.Fatalf("ManualResolveAlarm 失败: %v", err)
	}

	sumEnd := st.GetAlarmSummary()
	if sumEnd.TotalActive != 0 {
		t.Fatalf("全部解除后活跃告警应为 0，实际为: %d", sumEnd.TotalActive)
	}

	// 清理已解决历史告警
	cleared, err := st.ClearResolvedAlarms()
	if err != nil || cleared != 2 {
		t.Fatalf("ClearResolvedAlarms 预期清理 2 条，实际: %d, err: %v", cleared, err)
	}
}

func TestAlarmAPIReportAndHeartbeatAutoResolve(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "alarm_api_test_*")
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

	srv := NewServer(cfg, st, nil)

	// 1. 注册一个 Worker 节点
	workerNode := &model.Node{
		ID:            "worker_01",
		Name:          "storage-worker-01",
		IP:            "10.0.0.8",
		Port:          8081,
		Role:          "worker",
		Status:        "online",
		LastHeartbeat: time.Now(),
	}
	_ = st.SaveNode(workerNode)

	// 2. 模拟业务组件调用 /api/alarms/report 上报挂载盘只读故障
	reportPayload := model.AlarmReportReq{
		NodeID:    "worker_01",
		AlarmType: model.AlarmTypeDiskReadOnly,
		Severity:  model.SeverityCritical,
		Title:     "存储磁盘发生只读文件系统故障",
		Message:   "写测试失败: Read-only file system",
	}
	body, _ := json.Marshal(reportPayload)
	req := httptest.NewRequest(http.MethodPost, "/api/alarms/report", bytes.NewReader(body))
	req.Header.Set("X-Cluster-Token", cfg.ClusterToken)
	w := httptest.NewRecorder()

	srv.handleAlarmReport(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("上报告警接口响应异常: code=%d, body=%s", w.Code, w.Body.String())
	}

	// 验证告警已入库并记录了节点名称
	alarms, err := st.ListAlarms("active")
	if err != nil || len(alarms) != 1 {
		t.Fatalf("活跃告警列表数量应为 1, err=%v", err)
	}
	if alarms[0].NodeName != "storage-worker-01" || alarms[0].Severity != model.SeverityCritical {
		t.Fatalf("告警详情不符合预期: %+v", alarms[0])
	}

	// 3. 模拟节点离线，生成失联告警
	_, _ = st.CreateOrAggregateAlarm(&model.Alarm{
		NodeID:    "worker_01",
		NodeName:  "storage-worker-01",
		AlarmType: model.AlarmTypeNodeOffline,
		Severity:  model.SeverityCritical,
		Title:     "业务节点离线失联",
		Message:   "心跳超时",
	})

	activeOffline, _ := st.FindActiveAlarm("worker_01", model.AlarmTypeNodeOffline)
	if activeOffline == nil {
		t.Fatalf("预期存在未解除的 NODE_OFFLINE 告警")
	}

	// 4. 模拟 Worker 节点网络恢复重新上报心跳
	hbBody, _ := json.Marshal(workerNode)
	hbReq := httptest.NewRequest(http.MethodPost, "/api/cluster/heartbeat", bytes.NewReader(hbBody))
	hbReq.Header.Set("X-Cluster-Token", cfg.ClusterToken)
	hbW := httptest.NewRecorder()

	srv.handleClusterHeartbeat(hbW, hbReq)
	if hbW.Code != http.StatusOK {
		t.Fatalf("心跳接口响应异常: code=%d", hbW.Code)
	}

	// 验证 NODE_OFFLINE 告警已被心跳自动消警
	resolvedOffline, _ := st.FindActiveAlarm("worker_01", model.AlarmTypeNodeOffline)
	if resolvedOffline != nil {
		t.Fatalf("心跳恢复后，NODE_OFFLINE 活跃告警应该被自动解除，但依然存在: %+v", resolvedOffline)
	}
}
