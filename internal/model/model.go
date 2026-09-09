package model

import "time"

// User 角色定义
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// User 用户实体
type User struct {
	ID               string    `json:"id"`
	Username         string    `json:"username"`
	PasswordHash     string    `json:"password_hash"`
	Role             string    `json:"role"` // admin | user
	SpaceQuotaBytes  int64     `json:"space_quota_bytes"`  // 存储空间配额（字节），0为不限
	UsedStorageBytes int64     `json:"used_storage_bytes"` // 已使用空间
	Status           string    `json:"status"`             // active | disabled
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// SystemResource 节点系统资源上报
type SystemResource struct {
	CPUPercent      float64 `json:"cpu_percent"`
	MemTotalMB      int64   `json:"mem_total_mb"`
	MemUsedMB       int64   `json:"mem_used_mb"`
	DiskTotalMB     int64   `json:"disk_total_mb"`     // 磁盘总空间 (MB)
	DiskUsedMB      int64   `json:"disk_used_mb"`      // 磁盘已用空间 (MB)
	DiskFreeMB      int64   `json:"disk_free_mb"`      // 磁盘剩余可用空间 (MB)
	DiskUsedPercent float64 `json:"disk_used_percent"` // 磁盘已使用率 (0-100%)
	OS              string  `json:"os"`
	Arch            string  `json:"arch"`
}

// DiskInfo 远程物理磁盘信息
type DiskInfo struct {
	Name       string   `json:"name"`        // sdb, nvme0n1
	Path       string   `json:"path"`        // /dev/sdb
	Size       string   `json:"size"`        // 100G
	SizeBytes  int64    `json:"size_bytes"`  // 字节数
	Type       string   `json:"type"`        // disk, part
	MountPoint string   `json:"mount_point"` // 挂载点，如 /mnt/data
	FSType     string   `json:"fs_type"`     // ext4, xfs
	Model      string   `json:"model"`       // 硬件型号
	HasFS      bool     `json:"has_fs"`      // 是否已存在文件系统或分区（防呆判断依据）
	IsSystem   bool     `json:"is_system"`   // 是否包含系统分区（/, /boot等）
	Partitions []string `json:"partitions"`  // 包含的子分区详情说明
	StatusText string   `json:"status_text"` // 界面防呆状态文本
	CanFormat  bool     `json:"can_format"`  // 是否允许格式化（只有未格式化裸盘为 true）
}

// Node 节点实体
type Node struct {
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	IP               string         `json:"ip"`
	Port             int            `json:"port"`
	Role             string         `json:"role"` // manager | worker
	Status           string         `json:"status"` // online | offline | installing | failed
	Resource         SystemResource `json:"resource"`
	StorageUsedBytes int64          `json:"storage_used_bytes"` // 该节点上已保存的日志包总大小(字节)
	DiskDevice       string         `json:"disk_device,omitempty"` // 格式化挂载的磁盘设备，如 /dev/sdb
	MountPoint       string         `json:"mount_point,omitempty"` // 挂载路径，如 /data/dist-log-storage
	FSType           string         `json:"fs_type,omitempty"`     // 文件系统类型 ext4/xfs
	ActiveTasks      int            `json:"active_tasks"`
	InstallLog       string         `json:"install_log,omitempty"`
	LastHeartbeat    time.Time      `json:"last_heartbeat"`
	JoinedAt         time.Time      `json:"joined_at"`
}

// LogArchive 用户上传的压缩包日志记录
type LogArchive struct {
	ID              string    `json:"id"`
	UserID          string    `json:"user_id"`
	Username        string    `json:"username"`
	Filename        string    `json:"filename"`
	Size            int64     `json:"size"`
	Format          string    `json:"format"` // tar.gz, zip, tgz, tar, gz, bz2
	Status          string    `json:"status"` // uploading, extracting, analyzing, ready, failed
	ErrorMsg        string    `json:"error_msg,omitempty"`
	FileCount       int       `json:"file_count"`
	TotalLines      int64     `json:"total_lines"`
	ExtractPath     string    `json:"extract_path"`
	StorageNodeID   string    `json:"storage_node_id"`             // 存放该日志包的目标业务节点ID
	StorageNodeName string    `json:"storage_node_name,omitempty"` // 存放业务节点名称
	StorageNodeIP   string    `json:"storage_node_ip,omitempty"`   // 存放业务节点IP
	StorageNodePort int       `json:"storage_node_port,omitempty"` // 存放业务节点端口
	AssignedWorker  string    `json:"assigned_worker,omitempty"`
	UploadTime      time.Time `json:"upload_time"`
	FinishTime      time.Time `json:"finish_time"`
}

// LogFileItem 日志解压后的单文件结构
type LogFileItem struct {
	ArchiveID    string    `json:"archive_id"`
	RelativePath string    `json:"relative_path"`
	Size         int64     `json:"size"`
	ModTime      time.Time `json:"mod_time"`
	IsDirectory  bool      `json:"is_directory"`
	LineCount    int64     `json:"line_count"`
}

// FaultSeverity 故障等级
const (
	SeverityFatal    = "FATAL"
	SeverityCritical = "CRITICAL"
	SeverityWarning  = "WARNING"
	SeverityInfo     = "INFO"
)

// Rule 故障匹配规则
type Rule struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	StorageType string    `json:"storage_type"` // Ceph, HDFS, MinIO, GlusterFS, Generic
	Severity    string    `json:"severity"`     // FATAL, CRITICAL, WARNING, INFO
	Pattern     string    `json:"pattern"`      // 正则或关键词
	IsRegex     bool      `json:"is_regex"`
	Description string    `json:"description"`
	Suggestion  string    `json:"suggestion"`   // 专家排查建议
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// DiagnosisEvent 诊断命中的故障事件
type DiagnosisEvent struct {
	ID             string    `json:"id"`
	ArchiveID      string    `json:"archive_id"`
	RuleID         string    `json:"rule_id"`
	RuleName       string    `json:"rule_name"`
	Severity       string    `json:"severity"`
	StorageType    string    `json:"storage_type"`
	FilePath       string    `json:"file_path"`
	LineNumber     int64     `json:"line_number"`
	MatchedContent string    `json:"matched_content"`
	Suggestion     string    `json:"suggestion"`
	Timestamp      string    `json:"timestamp"`
}

// DiagnosisReport 压缩包自动诊断报告
type DiagnosisReport struct {
	ArchiveID       string           `json:"archive_id"`
	UserID          string           `json:"user_id"`
	ArchiveName     string           `json:"archive_name"`
	Status          string           `json:"status"` // pending, running, completed, failed
	TotalEvents     int              `json:"total_events"`
	SeveritySummary map[string]int   `json:"severity_summary"`
	StorageSummary  map[string]int   `json:"storage_summary"`
	Events          []DiagnosisEvent `json:"events"`
	HealthScore     int              `json:"health_score"` // 0-100
	SummaryText     string           `json:"summary_text"`
	AnalyzedAt      time.Time        `json:"analyzed_at"`
}

// SearchQuery 日志检索请求
type SearchQuery struct {
	ArchiveID    string `json:"archive_id"`
	FilePath     string `json:"file_path"`
	Keyword      string `json:"keyword"`
	IsRegex      bool   `json:"is_regex"`
	CaseSensitive bool  `json:"case_sensitive"`
	Level        string `json:"level"` // ALL, INFO, WARN, ERROR, etc.
	StartTime    string `json:"start_time"`
	EndTime      string `json:"end_time"`
	Page         int    `json:"page"`
	PageSize     int    `json:"page_size"`
	ContextLines int    `json:"context_lines"` // 上下文前后行数，如 2
}

// SearchHit 单条搜索命中
type SearchHit struct {
	FilePath      string   `json:"file_path"`
	LineNumber    int64    `json:"line_number"`
	Content       string   `json:"content"`
	Level         string   `json:"level"`
	Timestamp     string   `json:"timestamp"`
	ContextBefore []string `json:"context_before"`
	ContextAfter  []string `json:"context_after"`
}

// SearchResponse 检索响应
type SearchResponse struct {
	TotalHits int64       `json:"total_hits"`
	Page      int         `json:"page"`
	PageSize  int         `json:"page_size"`
	Hits      []SearchHit `json:"hits"`
	CostMS    int64       `json:"cost_ms"`
}

// HAStatus 高可用主备状态
type HAStatus struct {
	Mode          string    `json:"mode"`            // standalone | primary | backup
	Role          string    `json:"role"`            // active (主) | standby (备) | candidate (竞选晋升中)
	NodeName      string    `json:"node_name"`       // 本节点名称
	LocalAddr     string    `json:"local_addr"`      // 本地管理地址
	PeerURL       string    `json:"peer_url"`        // 对端地址
	PeerOnline    bool      `json:"peer_online"`     // 对端是否在线
	PeerLatencyMS int64     `json:"peer_latency_ms"` // 对端心跳往返延迟 (毫秒)
	LastHeartbeat time.Time `json:"last_heartbeat"` // 最后心跳探测时间
	LastSyncTime  time.Time `json:"last_sync_time"`  // 备机最后数据同步时间
	LastSyncBytes int64     `json:"last_sync_bytes"` // 最后一次快照同步数据量 (字节)
	SyncStatus    string    `json:"sync_status"`     // synced (已同步) | syncing (同步中) | error (同步异常) | none
	VIP           string    `json:"vip"`             // 绑定的虚拟高可用 IP
	VIPActive     bool      `json:"vip_active"`      // 本机当前是否持有 VIP
	FailoverCount int       `json:"failover_count"`  // 发生故障切换次数

	// 防脑裂指标
	GatewayIP          string `json:"gateway_ip"`            // 探测的网关 IP
	GatewayOnline      bool   `json:"gateway_online"`        // 网关连通性自检状态
	WorkerQuorumTotal  int    `json:"worker_quorum_total"`   // 集群 Worker 总数
	WorkerQuorumOnline int    `json:"worker_quorum_online"`  // 本机连通的 Worker 数
	QuorumPassed       bool   `json:"quorum_passed"`         // 多数派仲裁是否通过
	SplitBrainBlocked  bool   `json:"split_brain_blocked"`   // 是否触发防脑裂拦截阻止晋升
	BlockedReason      string `json:"blocked_reason"`        // 拦截原因说明
}

// ================= 告警系统模型 =================

// 告警生命周期状态
const (
	AlarmStatusActive       = "active"       // 活跃中 (未解除/未恢复)
	AlarmStatusAcknowledged = "acknowledged" // 已确认 (已知晓，等待处理或恢复)
	AlarmStatusResolved     = "resolved"     // 已解除 / 已自动恢复
)

// 告警特征类型
const (
	AlarmTypeNodeOffline  = "NODE_OFFLINE"  // 业务计算节点离线失联 (心跳丢失)
	AlarmTypeDiskFull     = "DISK_FULL"     // 业务存储挂载盘空间告急 / 耗尽
	AlarmTypeDiskReadOnly = "DISK_READONLY" // 存储文件系统只读或 I/O 挂载异常
	AlarmTypeHighMemory   = "HIGH_MEMORY"   // 内存使用率超过危险阈值 (>90%)
	AlarmTypeTaskFailed   = "TASK_FAILED"   // 核心日志解包/分析/诊断任务严重执行失败
	AlarmTypeNetworkError = "NETWORK_ERROR" // 业务节点网络通信异常
)

// Alarm 告警记录实体
type Alarm struct {
	ID           string    `json:"id"`
	NodeID       string    `json:"node_id"`                 // 产生告警的节点 ID
	NodeName     string    `json:"node_name"`               // 节点名称
	NodeIP       string    `json:"node_ip"`                 // 节点 IP 地址
	Component    string    `json:"component"`               // 组件名称 (如 worker)
	AlarmType    string    `json:"alarm_type"`              // 告警类型 (NODE_OFFLINE, DISK_FULL 等)
	Severity     string    `json:"severity"`                // CRITICAL | WARNING | INFO
	Title        string    `json:"title"`                   // 告警标题
	Message      string    `json:"message"`                 // 告警详细信息与排查线索
	Status       string    `json:"status"`                  // active | acknowledged | resolved
	Count        int       `json:"count"`                   // 连续发生频次 (告警聚合防风暴)
	FirstOccurAt time.Time `json:"first_occur_at"`           // 首次触发时间
	LastOccurAt  time.Time `json:"last_occur_at"`            // 最近触发时间
	ResolvedAt   *time.Time `json:"resolved_at,omitempty"`  // 自动或手动恢复时间
}

// AlarmReportReq 业务组件主动上报告警的请求载荷
type AlarmReportReq struct {
	NodeID    string `json:"node_id"`
	AlarmType string `json:"alarm_type"`
	Severity  string `json:"severity"`
	Title     string `json:"title"`
	Message   string `json:"message"`
}

// AlarmSummary 告警全局指标统计
type AlarmSummary struct {
	TotalActive   int `json:"total_active"`
	CriticalCount int `json:"critical_count"`
	WarningCount  int `json:"warning_count"`
	InfoCount     int `json:"info_count"`
}
