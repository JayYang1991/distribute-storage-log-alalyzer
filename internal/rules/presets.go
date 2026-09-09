package rules

import (
	"time"

	"dist-log-analyzer/internal/model"
)

// DefaultPresets 返回系统内置的专业存储故障预设规则库
func DefaultPresets() []*model.Rule {
	now := time.Now()
	return []*model.Rule{
		// Ceph 存储规则
		{
			ID:          "rule_ceph_001",
			Name:        "Ceph OSD 进程异常崩溃 (OSD Crash)",
			StorageType: "Ceph",
			Severity:    model.SeverityFatal,
			Pattern:     `(?i)(osd down|osd\.[0-9]+ marked down|Assertion failure|SIGSEGV|Segmentation fault|heartbeat_check: no reply)`,
			IsRegex:     true,
			Description: "检测到 Ceph OSD 守护进程由于内存段错误、断言失败或心跳超时被集群标记为 down 状态",
			Suggestion:  "1. 检查对应节点系统 /var/log/ceph/ceph-osd.{id}.log；2. 使用 ceph crash ls 查看 crash 堆栈；3. 检查底层磁盘 SMART 状态与 dmesg 日志是否存在硬件错误；4. 必要时执行 ceph osd mark in 恢复或更换损坏磁盘。",
			Enabled:     true,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
		{
			ID:          "rule_ceph_002",
			Name:        "Ceph 慢请求超时 (Slow Requests / Blocked IO)",
			StorageType: "Ceph",
			Severity:    model.SeverityCritical,
			Pattern:     `(?i)([0-9]+ slow requests|slow request [0-9]+\.[0-9]+s|requests are blocked|slow request.*initiated.*currently)`,
			IsRegex:     true,
			Description: "Ceph 客户端或 OSD 内部请求响应延迟严重超过阈值（如 >30s），导致客户端 IO 挂起",
			Suggestion:  "1. 运行 ceph health detail 定位卡住的 PG 与 OSD；2. 排查是否有网络丢包、高延迟或交换机背板拥塞；3. 检查对应 OSD 所在磁盘是否存在巨额 IO 排队或高负载利用率（通过 iostat -xz 1 查看 %util）；4. 检查是否有数据深洗（Deep Scrub）占用过多带宽。",
			Enabled:     true,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
		{
			ID:          "rule_ceph_003",
			Name:        "Ceph 归置组未就绪 (PG Peering / Degradation)",
			StorageType: "Ceph",
			Severity:    model.SeverityWarning,
			Pattern:     `(?i)(pgs? (degraded|stuck|peering|undersized|inconsistent|incomplete))`,
			IsRegex:     true,
			Description: "Ceph 归置组 PG 处于降级、对等超时或不一致状态，冗余副本不足",
			Suggestion:  "1. 执行 ceph pg dump_stuck [unclean|inactive|stale|undersized]；2. 若 PG 不一致，执行 ceph pg repair <pg_id> 进行数据自愈修复；3. 确认所有 OSD 是否均处于 up/in 状态。",
			Enabled:     true,
			CreatedAt:   now,
			UpdatedAt:   now,
		},

		// HDFS 存储规则
		{
			ID:          "rule_hdfs_001",
			Name:        "HDFS DataNode 失去心跳 (Dead Node)",
			StorageType: "HDFS",
			Severity:    model.SeverityCritical,
			Pattern:     `(?i)(DataNode.*is dead|Heartbeat expired|Lost contact with DataNode|Failed to connect to.*DataNode)`,
			IsRegex:     true,
			Description: "NameNode 检测到某个 DataNode 节点心跳超时，已将其标记为死亡节点",
			Suggestion:  "1. 登录该 DataNode 机器检查 hadoop-hdfs-datanode 进程是否存活及 JVM GC 日志（是否发生长时间 Full GC）；2. 检查节点网络与防火墙配置；3. 检查磁盘根目录或数据卷是否写满导致进程挂起。",
			Enabled:     true,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
		{
			ID:          "rule_hdfs_002",
			Name:        "HDFS 数据块损坏或丢失 (Block Corruption)",
			StorageType: "HDFS",
			Severity:    model.SeverityFatal,
			Pattern:     `(?i)(Corrupt block|MISSING BLOCKS|Block.*is CORRUPT|ChecksumException|Checksum mismatch)`,
			IsRegex:     true,
			Description: "HDFS 文件系统读取或写入数据时检测到数据校验和不匹配，数据块损坏或永久丢失",
			Suggestion:  "1. 执行 hdfs fsck / -list-corruptfileblocks 定位损坏文件；2. 若有其它健康副本，NameNode 会自动复制修复；若副本全部丢失，需评估从冷备还原或执行 hdfs fsck -delete 清理坏块。",
			Enabled:     true,
			CreatedAt:   now,
			UpdatedAt:   now,
		},

		// MinIO 对象存储规则
		{
			ID:          "rule_minio_001",
			Name:        "MinIO 纠删码驱动器脱机 (Drive Offline)",
			StorageType: "MinIO",
			Severity:    model.SeverityCritical,
			Pattern:     `(?i)(drive.*offline|Disk.*unresponsive|Unable to read xl\.meta|Erasure quorum compromised|corrupted backend)`,
			IsRegex:     true,
			Description: "MinIO 纠删码集合中的底层磁盘卷脱机或元数据文件 xl.meta 读取失败",
			Suggestion:  "1. 使用 mc admin info / mc admin drive info 查看磁盘状态；2. 检查故障盘挂载点和物理硬件；3. 更换坏盘后 MinIO 会自动后台通过自动自愈（Heal）重新计算并写入纠删数据。",
			Enabled:     true,
			CreatedAt:   now,
			UpdatedAt:   now,
		},

		// 通用 Linux / 存储硬件 I/O 规则
		{
			ID:          "rule_io_001",
			Name:        "物理磁盘底层 I/O 错误 (Disk I/O Error)",
			StorageType: "Generic",
			Severity:    model.SeverityFatal,
			Pattern:     `(?i)(Buffer I/O error|I/O error, dev|blk_update_request: I/O error|SCSI error.*medium or hardware error|ext4_lookup.*error|XFS.*error 5)`,
			IsRegex:     true,
			Description: "Linux 内核在向物理驱动器下发读写请求时收到 SCSI / SATA 介质损坏或控制器硬件错误",
			Suggestion:  "1. 立即执行 smartctl -a /dev/sdX 查看 Reallocated_Sector_Ct 等关键坏道指标；2. 对该磁盘执行只读下线隔离操作；3. 安排换盘维护，避免二次故障导致阵列或分布式副本雪崩。",
			Enabled:     true,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
		{
			ID:          "rule_io_002",
			Name:        "文件系统变为只读模式 (Read-only Filesystem)",
			StorageType: "Generic",
			Severity:    model.SeverityCritical,
			Pattern:     `(?i)(Read-only file system|Remounting filesystem read-only|Journal commit I/O error|EXT4-fs error)`,
			IsRegex:     true,
			Description: "由于文件系统日志损坏或元数据 I/O 连续失败，Linux 内核为保护数据安全已将存储卷强制 remount 为只读",
			Suggestion:  "1. 检查存储阵列与 SAN/NAS 链路光纤是否抖动断连；2. 停止相关写业务，尝试运行 fsck / xfs_repair 修复文件系统一致性；3. 检查系统盘与数据盘空间是否彻底耗尽。",
			Enabled:     true,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
		{
			ID:          "rule_io_003",
			Name:        "存储网络分区与心跳丢失 (Split-brain / Network Partition)",
			StorageType: "Generic",
			Severity:    model.SeverityWarning,
			Pattern:     `(?i)(split-brain|network partition|heartbeat lost|connection reset by peer|no route to host|quorum lost)`,
			IsRegex:     true,
			Description: "分布式存储集群内部节点间网络通信中断，导致仲裁仲裁组（Quorum）丢失或脑裂风险",
			Suggestion:  "1. 检查各存储管理网和数据网交换机端口链路状态；2. 检查 Linux 节点 MTU 大小配置（是否因 Jumbo Frame 巨型帧不一致导致大包被丢）；3. 运行 ping -s 8972 与 traceroute 排查 MTU 与链路抖动。",
			Enabled:     true,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
	}
}
