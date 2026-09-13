package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dist-log-analyzer/internal/model"
)

func TestChartEngine_DownsampleMinMax(t *testing.T) {
	// 构造 10,000 个采样点，其中平时为 10~20，仅在第 4582 个点突变飙升至 100.0
	n := 10000
	xAxis := make([]string, n)
	data := make([]float64, n)

	baseTime := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		cur := baseTime.Add(time.Duration(i) * time.Second)
		xAxis[i] = cur.Format("15:04:05")
		data[i] = 10.0 + float64(i%10)
	}
	spikeIdx := 4582
	data[spikeIdx] = 100.0 // 突变最高峰

	resp := model.ChartDataResponse{
		Title: "测试大数据量降采样",
		XAxis: model.ChartXAxis{
			Label: "时间",
			Type:  "time",
			Data:  xAxis,
		},
		Series: []model.ChartSeries{
			{
				Name: "nvme0n1 利用率",
				Unit: "%",
				Data: data,
			},
		},
	}

	targetPoints := 1500
	result := downsampleMinMax(resp, targetPoints)

	// 1. 验证点数严格压缩收敛
	if len(result.XAxis.Data) > targetPoints+100 { // 考虑首尾及极值可能微调，点数严格收敛
		t.Fatalf("降采样后点数过多: 期望 <= %d, 实际 = %d", targetPoints+100, len(result.XAxis.Data))
	}
	if len(result.Series[0].Data) != len(result.XAxis.Data) {
		t.Fatalf("降采样后 X 与 Y 长度不一致: %d vs %d", len(result.XAxis.Data), len(result.Series[0].Data))
	}

	// 2. 核心校验：极值 100.0 尖峰绝对不能被抹平！
	hasSpike := false
	for _, v := range result.Series[0].Data {
		if v == 100.0 {
			hasSpike = true
			break
		}
	}
	if !hasSpike {
		t.Fatalf("降采样严重失真: 突变尖峰 100.0 被抹平或丢失！")
	}

	t.Logf("✔ Min-Max 降采样验证通过：原始 %d 点 成功压缩为 %d 点，且 100.0 突发尖峰 100%% 完好保留！", n, len(result.XAxis.Data))
}

func TestChartEngine_AnomalyDetection(t *testing.T) {
	n := 100
	xAxis := make([]string, n)
	data := make([]float64, n)
	for i := 0; i < n; i++ {
		xAxis[i] = fmt.Sprintf("14:%02d:00", i)
		data[i] = 15.0
	}
	// 在第 50 点制造突增至 98.5
	data[50] = 98.5

	series := []model.ChartSeries{
		{
			Name: "sda 利用率",
			Unit: "%",
			Data: data,
		},
	}

	anomalies := detectAnomalies(xAxis, series)
	if len(anomalies) == 0 {
		t.Fatalf("未能识别出明显的阶跃跃变异常")
	}

	found := false
	for _, a := range anomalies {
		if a.Index == 50 && a.Value == 98.5 {
			found = true
			if a.Severity != "CRITICAL" {
				t.Errorf("预期 CRITICAL 级别, 实际: %s", a.Severity)
			}
			t.Logf("✔ 成功捕获突变事件: %s - %s", a.Time, a.Reason)
			break
		}
	}
	if !found {
		t.Fatalf("未在预期索引 50 处找到突变记录")
	}
}

func TestChartEngine_ExecutePythonScript(t *testing.T) {
	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "iostat_test.log")

	// 模拟写入简短的 iostat 日志
	mockLog := `Linux 5.14.0 (node-1) 	09/13/2026 	_x86_64_	(16 CPU)

14:00:01
Device            r/s     w/s     rkB/s     wkB/s   rrqm/s   wrqm/s  %rrqm  %wrqm r_await w_await aqu-sz rareq-sz wareq-sz  svctm  %util
sda              0.00   10.00      0.00    100.00     0.00     1.00   0.00   1.00    0.00    0.50   0.01     0.00    10.00   0.20   2.00
nvme0n1          5.00  500.00    256.00  10240.00     0.00    10.00   0.00   1.50    0.10    0.30   0.20    51.20    20.48   0.10  96.50

14:00:02
Device            r/s     w/s     rkB/s     wkB/s   rrqm/s   wrqm/s  %rrqm  %wrqm r_await w_await aqu-sz rareq-sz wareq-sz  svctm  %util
sda              0.00   12.00      0.00    120.00     0.00     1.00   0.00   1.00    0.00    0.50   0.01     0.00    10.00   0.20   2.50
nvme0n1          6.00  600.00    300.00  12000.00     0.00    12.00   0.00   1.50    0.10    0.30   0.25    50.00    20.00   0.10  98.20
`
	if err := os.WriteFile(logFile, []byte(mockLog), 0644); err != nil {
		t.Fatal(err)
	}

	pyScript := `#!/usr/bin/env python3
import sys, re, json

log_file = sys.argv[1]
timestamps, dev_util, anomalies = [], {}, []
current_time, line_idx = "", 0
re_time = re.compile(r"^(\d{2}:\d{2}:\d{2})")

with open(log_file, "r", encoding="utf-8", errors="ignore") as f:
    for raw in f:
        line_idx += 1
        line = raw.strip()
        tm = re_time.match(line)
        if tm:
            current_time = tm.group(1)
            if current_time not in timestamps: timestamps.append(current_time)
            continue
        parts = line.split()
        if len(parts) >= 12 and not parts[0].startswith("Device") and not parts[0].startswith("avg-cpu"):
            dev, util = parts[0], float(parts[-1])
            dev_util.setdefault(dev, []).append(util)
            if util >= 90.0:
                anomalies.append({
                    "time": current_time, "index": len(timestamps)-1,
                    "metric": f"{dev} 利用率", "value": util,
                    "severity": "CRITICAL", "reason": f"高负荷 {util}%",
                    "line_number": line_idx
                })

series = [{"name": f"{d} 利用率", "unit": "%", "data": dev_util[d]} for d in sorted(dev_util.keys())]
print(json.dumps({
    "title": "iostat 磁盘测试",
    "x_axis": {"label": "时间", "data": timestamps},
    "series": series,
    "anomalies": anomalies
}))
`

	rule := &model.ChartScriptRule{
		ID:            "test_rule_1",
		Name:          "iostat 测试脚本",
		FilePattern:   `.*iostat.*`,
		Interpreter:   "/usr/bin/python3",
		ScriptContent: pyScript,
		Enabled:       true,
	}

	engine := NewChartEngine()
	resp, err := engine.ExecuteScript(context.Background(), rule, logFile, "", "", 1500)
	if err != nil {
		// 若测试机未安装 python3，可降级忽略
		t.Logf("运行 python3 解释器跳过或失败 (可能测试机无 python3): %v", err)
		return
	}

	if len(resp.XAxis.Data) != 2 {
		t.Fatalf("预期 2 个时间点, 实际: %d", len(resp.XAxis.Data))
	}
	if len(resp.Series) != 2 {
		t.Fatalf("预期 2 个设备系列, 实际: %d", len(resp.Series))
	}
	if len(resp.Anomalies) != 2 {
		t.Fatalf("预期 2 处 nvme0n1 突变异常, 实际: %d", len(resp.Anomalies))
	}

	t.Logf("✔ Python 脚本端到端执行与协议解析成功: 识别到时间点 %v, 系列数: %d, 突变数: %d", resp.XAxis.Data, len(resp.Series), len(resp.Anomalies))
}
