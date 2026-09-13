package ai

import (
	"time"

	"dist-log-analyzer/internal/model"
)

// GetDefaultGlossary 提供开箱即用的企业级自研分布式存储专有术语词典
func GetDefaultGlossary() []model.AIGlossaryTerm {
	return []model.AIGlossaryTerm{
		{
			Term:          "CHUNK_SEAL",
			FullName:      "数据分块定稿封装 (Chunk Seal)",
			Module:        "StorageEngine/ChunkServer",
			Definition:    "客户端写满一个标准分块（通常64MB）或主动Close文件时触发。将分块状态由追加态(APPENDING)置为只读定稿态(SEALED)，并计算全局校验和提交至元数据服务。",
			FailureImpact: "定稿失败会导致元数据状态不一致，客户端产生写悬挂 (Write Hang) 或返回 EIO。",
			CommonPatterns: []string{
				"failed to seal chunk",
				"seal chunk timeout",
				"CHUNK_SEAL_TIMEOUT",
				"chunk seal failed",
			},
		},
		{
			Term:          "BUMP_VER",
			FullName:      "集群视图拓扑版本递增 (Epoch Version Bumping)",
			Module:        "Coordinator/MDS/Monitor",
			Definition:    "分布式集群拓扑视图或心跳租约发生变更（如检测到节点失联、宕机或网络分区）时，协调节点自增集群版本号并向全网广播最新 Map。",
			FailureImpact: "频繁触发 BUMP_VER 说明集群存在网络抖动或节点震荡（Flapping），会导致客户端频繁刷新路由并重发 RPC。",
			CommonPatterns: []string{
				"bump epoch",
				"bump map version",
				"BUMP_VER",
				"epoch bumped to",
			},
		},
		{
			Term:          "PEER_WAL_COMMIT",
			FullName:      "多副本对端预写日志落盘 (Peer WAL Commit)",
			Module:        "Replication/Raft",
			Definition:    "主节点将写请求并发扇出（Fan-out）复制到从节点时，从节点将数据或操作日志写入本地 WAL/RocksDB 成功的确认确认阶段。",
			FailureImpact: "该阶段超时通常由从副本节点物理磁盘 I/O 夯死、写入延迟飙升或从节点 GC 停顿引起。",
			CommonPatterns: []string{
				"peer wal commit timeout",
				"waiting for peer wal",
				"PEER_WAL_COMMIT",
				"peer ack timeout",
			},
		},
		{
			Term:          "MDS_LEASE",
			FullName:      "元数据主备同步租约 (MDS Lease)",
			Module:        "MetadataService",
			Definition:    "分布式元数据目录树或子树分区在多个 MDS 节点间动态负载均衡时分配的排他访问租约，包含心跳保活机制，超时通常为 3~5 秒。",
			FailureImpact: "租约超时会触发子树迁移紧急回滚，客户端在此期间对该目录的元数据请求将被阻塞等待。",
			CommonPatterns: []string{
				"mds lease expired",
				"lease renew timeout",
				"MDS_LEASE_LOST",
				"subtree lease lost",
			},
		},
		{
			Term:          "QUORUM_ACK",
			FullName:      "多数派达成确认 (Quorum Acknowledge)",
			Module:        "Consensus/Replication",
			Definition:    "强一致性写入流程中，主节点等待超过半数（N/2+1）副本完成落盘后，判定写入成功并向客户端返回 ACK 的阶段。",
			FailureImpact: "未能达成 Quorum 会导致写请求阻塞直至客户端产生 15s/30s 超时报错。",
			CommonPatterns: []string{
				"quorum ack failed",
				"waiting for quorum",
				"quorum not reached",
				"QUORUM_TIMEOUT",
			},
		},
	}
}

// GetDefaultWorkflows 提供开箱即用的核心分布式业务流程状态机
func GetDefaultWorkflows() []model.AIWorkflowDefinition {
	return []model.AIWorkflowDefinition{
		{
			ID:          "WRITE_DATA_FLOW",
			Name:        "多副本数据分块写入与强一致提交流程",
			Description: "客户端发起数据写入至全副本落盘并响应确认的标准分布式写事务时序流程",
			Stages: []model.AIWorkflowStage{
				{
					Step:           1,
					Name:           "ALLOC_CHUNK",
					Description:    "客户端向 MDS 申请或定位写入分块及副本分布列表",
					SourceModule:   "Client",
					TargetModule:   "MDS",
					SuccessPattern: `(?i)(allocated? chunk|locate chunk.*success)`,
					TimeoutMs:      2000,
				},
				{
					Step:           2,
					Name:           "FORWARD_PRIMARY",
					Description:    "客户端将写数据包传输给主副本存储节点 (Primary Node)",
					SourceModule:   "Client",
					TargetModule:   "PrimaryNode",
					SuccessPattern: `(?i)(dispatch.*to primary|primary received write)`,
					TimeoutMs:      3000,
				},
				{
					Step:           3,
					Name:           "PEER_WAL_COMMIT",
					Description:    "主节点并发复制数据至所有从节点，并等待从节点写入本地预写日志(WAL)",
					SourceModule:   "PrimaryNode",
					TargetModule:   "ReplicaNode",
					SuccessPattern: `(?i)(peer wal.*done|replica.*commit.*ok)`,
					TimeoutMs:      5000,
				},
				{
					Step:           4,
					Name:           "QUORUM_ACK",
					Description:    "主节点确认多数派从节点返回成功，并完成本地提交",
					SourceModule:   "PrimaryNode",
					TargetModule:   "ConsensusEngine",
					SuccessPattern: `(?i)(quorum reached|quorum ack|majority committed)`,
					TimeoutMs:      2000,
				},
				{
					Step:           5,
					Name:           "ACK_CLIENT",
					Description:    "主节点向客户端返回写成功 ACK，流程圆满结束",
					SourceModule:   "PrimaryNode",
					TargetModule:   "Client",
					SuccessPattern: `(?i)(client write.*success|send ack to client)`,
					TimeoutMs:      1000,
				},
			},
		},
		{
			ID:          "FAILOVER_FLOW",
			Name:        "节点心跳故障转移与视图版本演进流程",
			Description: "节点心跳丢失引发故障探测器告警并触发集群版本递增与主备切换的流程",
			Stages: []model.AIWorkflowStage{
				{
					Step:           1,
					Name:           "HEARTBEAT_TIMEOUT",
					Description:    "监控或协调节点连续探测目标节点心跳失败",
					SourceModule:   "Monitor",
					TargetModule:   "TargetNode",
					SuccessPattern: `(?i)(heartbeat lost|heartbeat timeout|ping failed)`,
					TimeoutMs:      6000,
				},
				{
					Step:           2,
					Name:           "BUMP_VER",
					Description:    "协调者将目标节点置为下线状态并递增全网拓扑版本号",
					SourceModule:   "Monitor",
					TargetModule:   "AllNodes",
					SuccessPattern: `(?i)(bump.*epoch|bump.*version|mark.*down)`,
					TimeoutMs:      2000,
				},
				{
					Step:           3,
					Name:           "TRIGGER_RECOVERY",
					Description:    "触发降级副本的自动补全与数据重新平衡",
					SourceModule:   "Coordinator",
					TargetModule:   "StorageEngine",
					SuccessPattern: `(?i)(trigger recovery|start backfill|rebalance)`,
					TimeoutMs:      10000,
				},
			},
		},
	}
}

// GetDefaultNoiseRules 提供开箱即用的误打 WARN/ERR 抑制白名单规则
func GetDefaultNoiseRules() []model.AINoiseRule {
	now := time.Now()
	return []model.AINoiseRule{
		{
			ID:          "NOISE-001",
			Pattern:     `(?i)(lease file not found.*creating new one|config.*not found.*using defaults?)`,
			Reason:      "初始化检测分支，不存在时自动按默认值新建，属于正常冷启动逻辑而非真实报错",
			StorageType: "ALL",
			Enabled:     true,
			CreatedAt:   now,
		},
		{
			ID:          "NOISE-002",
			Pattern:     `(?i)(connection reset by peer.*during (health_?check|probe)|probe disconnected)`,
			Reason:      "外部负载均衡或健康探针在探测TCP端口存活后主动Reset断开，属于正常探活现象",
			StorageType: "ALL",
			Enabled:     true,
			CreatedAt:   now,
		},
		{
			ID:          "NOISE-003",
			Pattern:     `(?i)(broken pipe.*closing idle connection|idle connection closed)`,
			Reason:      "连接池回收长时间空闲的长连接，正常资源释放",
			StorageType: "ALL",
			Enabled:     true,
			CreatedAt:   now,
		},
		{
			ID:          "NOISE-004",
			Pattern:     `(?i)(metric stat directory /tmp/.* does not exist|temp file.*already removed)`,
			Reason:      "临时统计缓存目录或临时文件在定时清理后已被删除，属于良性静默清理",
			StorageType: "ALL",
			Enabled:     true,
			CreatedAt:   now,
		},
	}
}
