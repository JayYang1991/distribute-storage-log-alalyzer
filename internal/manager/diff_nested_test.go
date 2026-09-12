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

func TestDetectSubArchives(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "test_sub_archives_*")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// 构造子包 1: node-01 (含多层嵌套目录)
	node1Dir := filepath.Join(tempDir, "node-01", "logs", "ceph")
	_ = os.MkdirAll(node1Dir, 0755)
	_ = os.WriteFile(filepath.Join(node1Dir, "ceph.log"), []byte("2026-09-13 10:00:00 INFO cluster healthy\n"), 0644)
	_ = os.WriteFile(filepath.Join(node1Dir, "ceph-osd.0.log"), []byte("2026-09-13 10:00:01 INFO osd.0 ok\n"), 0644)

	// 构造子包 2: node-02 (含多层嵌套目录)
	node2Dir := filepath.Join(tempDir, "node-02", "logs", "ceph")
	_ = os.MkdirAll(node2Dir, 0755)
	_ = os.WriteFile(filepath.Join(node2Dir, "ceph.log"), []byte("2026-09-13 10:00:00 ERROR osd down\n"), 0644)

	subs, err := DetectSubArchives(tempDir)
	if err != nil {
		t.Fatalf("DetectSubArchives 失败: %v", err)
	}

	if len(subs) != 2 {
		t.Fatalf("预期探测出 2 个子包，实际: %d", len(subs))
	}

	if subs[0].Name != "node-01" || subs[0].TotalFiles != 2 || !subs[0].HasNested {
		t.Errorf("node-01 元数据不符合预期: %+v", subs[0])
	}
	if subs[1].Name != "node-02" || subs[1].TotalFiles != 1 || !subs[1].HasNested {
		t.Errorf("node-02 元数据不符合预期: %+v", subs[1])
	}
	t.Logf("✔ 子包智能探测测试通过: %+v", subs)
}

func TestCompareArchiveScopes_IntraArchiveNested(t *testing.T) {
	tempDataDir, err := os.MkdirTemp("", "test_intra_diff_data_*")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tempDataDir)

	cfg := config.DefaultConfig()
	cfg.DataDir = tempDataDir
	st, err := store.NewStore(cfg)
	if err != nil {
		t.Fatalf("初始化 store 失败: %v", err)
	}
	defer st.Close()

	server := &Server{
		cfg:   cfg,
		store: st,
	}

	// 构造大总包解压目录
	extractDir := filepath.Join(tempDataDir, "extracted", "arc_cluster_sosreport")
	_ = os.MkdirAll(extractDir, 0755)

	// 内部子包 A (正常基准节点): 含多层嵌套目录 logs/ceph/ 与 syslog/
	subDirA := filepath.Join(extractDir, "node-ceph-healthy", "logs")
	_ = os.MkdirAll(filepath.Join(subDirA, "ceph"), 0755)
	_ = os.MkdirAll(filepath.Join(subDirA, "sys"), 0755)
	_ = os.WriteFile(filepath.Join(subDirA, "ceph", "ceph.log"), []byte(
		"2026-09-13 10:00:00 [INF] mon.a active and healthy\n"+
			"2026-09-13 10:00:01 [INF] osd.0 heartbeat check ok\n"+
			"2026-09-13 10:00:02 [INF] pg 1.0 active+clean\n",
	), 0644)
	_ = os.WriteFile(filepath.Join(subDirA, "sys", "syslog.log"), []byte("2026-09-13 10:00:00 kernel: system started\n"), 0644)

	// 内部子包 B (故障待测节点): 相同结构但有新增崩溃日志 crash.dump 与全新异质致命异常模式
	subDirB := filepath.Join(extractDir, "node-ceph-faulty", "logs")
	_ = os.MkdirAll(filepath.Join(subDirB, "ceph"), 0755)
	_ = os.MkdirAll(filepath.Join(subDirB, "sys"), 0755)
	_ = os.WriteFile(filepath.Join(subDirB, "ceph", "ceph.log"), []byte(
		"2026-09-13 10:00:00 [INF] mon.a active and healthy\n"+
			"2026-09-13 10:00:01 [ERR] osd.1 marked down by monitor\n"+
			"2026-09-13 10:00:02 [FATAL] heartbeat lost to peer osd.1\n"+
			"2026-09-13 10:00:03 [FATAL] heartbeat lost to peer osd.1\n",
	), 0644)
	_ = os.WriteFile(filepath.Join(subDirB, "sys", "syslog.log"), []byte("2026-09-13 10:00:00 kernel: system started\n"), 0644)
	// 故障节点独有新增文件
	_ = os.WriteFile(filepath.Join(subDirB, "ceph", "osd.1.crash.dump"), []byte("stack trace: segfault at 0x0\n"), 0644)

	// 注册归档包
	arc := &model.LogArchive{
		ID:          "arc_cluster_sosreport",
		Filename:    "cluster-sosreport-20260913.tar.gz",
		ExtractPath: extractDir,
		Status:      "ready",
		UploadTime:  time.Now(),
		Username:    "admin",
	}
	_ = st.SaveArchive(arc)

	// 执行同一个压缩包内的多子包差分比对！
	report, err := server.CompareArchiveScopes("arc_cluster_sosreport", "node-ceph-healthy", "arc_cluster_sosreport", "node-ceph-faulty")
	if err != nil {
		t.Fatalf("同一压缩包内部子包差分对比失败: %v", err)
	}

	if report.ScopeType != "intra_archive" {
		t.Fatalf("ScopeType 预期 intra_archive，实际: %s", report.ScopeType)
	}

	// 验证文件数
	// 基准 A: 2 个文件 (ceph.log, syslog.log)
	// 待测 B: 3 个文件 (ceph.log, syslog.log, osd.1.crash.dump)
	if report.TotalFilesA != 2 || report.TotalFilesB != 3 {
		t.Errorf("文件数对齐统计错误: A=%d, B=%d", report.TotalFilesA, report.TotalFilesB)
	}

	// 验证新增文件识别（归一化相对路径）
	foundCrashDump := false
	for _, f := range report.AddedFiles {
		if filepath.Base(f) == "osd.1.crash.dump" {
			foundCrashDump = true
			break
		}
	}
	if !foundCrashDump {
		t.Errorf("未能识别出待测子包新增的 crash.dump 文件: %+v", report.AddedFiles)
	}

	// 验证 Drain 零样本突发异质模式挖掘
	if len(report.NewTemplates) == 0 {
		t.Errorf("未能基于 Drain 挖掘出故障子包 B 特有的异质异常模式")
	} else {
		t.Logf("✔ 成功挖掘出待测子包特有模式 (%d 个):", len(report.NewTemplates))
		for _, tpl := range report.NewTemplates {
			t.Logf("   -> [%s] %s (count: %d)", tpl.Level, tpl.Pattern, tpl.Count)
		}
	}

	t.Logf("✔ 同一压缩包内多子包基准差分诊断报告:\n%s", report.SummaryText)
}
