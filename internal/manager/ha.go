package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"dist-log-analyzer/internal/config"
	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/store"
)

// HAManager 高可用主备状态机与心跳故障切换管理器
type HAManager struct {
	cfg        *config.Config
	store      *store.Store
	syncEngine *HASyncEngine

	mu            sync.RWMutex
	mode          string // standalone | primary | backup
	role          string // active | standby | candidate
	peerURL       string
	peerOnline    bool
	peerLatencyMS int64
	lastHeartbeat time.Time
	failoverCount int
	vipActive     bool

	// 防脑裂状态
	gatewayIP          string
	gatewayOnline      bool
	workerQuorumTotal  int
	workerQuorumOnline int
	quorumPassed       bool
	splitBrainBlocked  bool
	blockedReason      string

	consecutiveFailures int
	client              *http.Client
}

func NewHAManager(cfg *config.Config, st *store.Store) *HAManager {
	mode := strings.ToLower(strings.TrimSpace(cfg.HAMode))
	if mode == "" {
		mode = "standalone"
	}

	role := "active"
	if mode == "backup" {
		role = "standby"
	}

	gw := cfg.GatewayIP
	if gw == "" && cfg.EnableGatewayCheck {
		gw = DetectDefaultGateway()
	}

	h := &HAManager{
		cfg:           cfg,
		store:         st,
		mode:          mode,
		role:          role,
		peerURL:       strings.TrimRight(cfg.PeerURL, "/"),
		client:        &http.Client{Timeout: 3 * time.Second},
		gatewayIP:     gw,
		gatewayOnline: true,
		quorumPassed:  true,
	}
	h.syncEngine = NewHASyncEngine(h, st)
	return h
}

// GetStatus 获取当前 HA 状态快照
func (h *HAManager) GetStatus() model.HAStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()

	syncStatus := "none"
	var lastSyncTime time.Time
	var lastSyncBytes int64
	if h.syncEngine != nil {
		syncStatus = h.syncEngine.syncStatus
		lastSyncTime = h.syncEngine.lastSyncTime
		lastSyncBytes = h.syncEngine.lastSyncBytes
	}

	localAddr := fmt.Sprintf("http://%s:%d", h.cfg.AdvertiseIP, h.cfg.Port)

	return model.HAStatus{
		Mode:               h.mode,
		Role:               h.role,
		NodeName:           h.cfg.NodeName,
		LocalAddr:          localAddr,
		PeerURL:            h.peerURL,
		PeerOnline:         h.peerOnline,
		PeerLatencyMS:      h.peerLatencyMS,
		LastHeartbeat:      h.lastHeartbeat,
		LastSyncTime:       lastSyncTime,
		LastSyncBytes:      lastSyncBytes,
		SyncStatus:         syncStatus,
		VIP:                h.cfg.VIP,
		VIPActive:          h.vipActive,
		FailoverCount:      h.failoverCount,
		GatewayIP:          h.gatewayIP,
		GatewayOnline:      h.gatewayOnline,
		WorkerQuorumTotal:  h.workerQuorumTotal,
		WorkerQuorumOnline: h.workerQuorumOnline,
		QuorumPassed:       h.quorumPassed,
		SplitBrainBlocked:  h.splitBrainBlocked,
		BlockedReason:      h.blockedReason,
	}
}

// IsActive 当前节点是否为主节点 (允许写与执行核心业务调度)
func (h *HAManager) IsActive() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.role == "active"
}

// Start 启动 HA 心跳探测与数据同步引擎
func (h *HAManager) Start(ctx context.Context) {
	if h.mode == "standalone" {
		log.Printf("[HA Manager] 当前处于单节点独立模式 (Standalone)")
		return
	}

	log.Printf("[HA Manager] 启动高可用主备模式 (模式: %s, 初始角色: %s, 对端: %s)", h.mode, h.role, h.peerURL)

	// 如果配置了 VIP 且初始为 Active，尝试绑定 VIP
	if h.role == "active" && h.cfg.VIP != "" {
		h.bindVIP()
	}

	// 启动数据同步引擎 (在 Standby 模式下定时从 Active 拉取快照)
	go h.syncEngine.Start(ctx)

	// 启动心跳探测主循环
	go h.heartbeatLoop(ctx)
}

// heartbeatLoop 定时探测对端健康状态
func (h *HAManager) heartbeatLoop(ctx context.Context) {
	interval := time.Duration(h.cfg.HeartbeatIntervalSec) * time.Second
	if interval <= 0 {
		interval = 2 * time.Second
	}
	failoverLimit := h.cfg.FailoverTimeoutSec / h.cfg.HeartbeatIntervalSec
	if failoverLimit < 2 {
		failoverLimit = 3
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if h.vipActive {
				h.releaseVIP()
			}
			return
		case <-ticker.C:
			h.probePeer(failoverLimit)
		}
	}
}

// probePeer 单次探测对端状态
func (h *HAManager) probePeer(failoverLimit int) {
	if h.peerURL == "" {
		return
	}

	heartbeatURL := fmt.Sprintf("%s/api/ha/heartbeat", h.peerURL)
	req, err := http.NewRequest("GET", heartbeatURL, nil)
	if err != nil {
		h.markPeerFailure("创建心跳请求失败", failoverLimit)
		return
	}
	req.Header.Set("X-Cluster-Token", h.cfg.ClusterToken)

	start := time.Now()
	resp, err := h.client.Do(req)
	latency := time.Since(start).Milliseconds()

	if err != nil {
		h.markPeerFailure(fmt.Sprintf("连接失败: %v", err), failoverLimit)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		h.markPeerFailure(fmt.Sprintf("状态码异常: %d", resp.StatusCode), failoverLimit)
		return
	}

	var res struct {
		Role     string `json:"role"`
		NodeName string `json:"node_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		h.markPeerFailure("解析心跳响应失败", failoverLimit)
		return
	}

	// 心跳成功
	h.mu.Lock()
	h.peerOnline = true
	h.peerLatencyMS = latency
	h.lastHeartbeat = time.Now()
	h.consecutiveFailures = 0

	// 脑裂保护机制：
	// 如果本节点原是 primary/active，但重启上线后发现对端已经是 active，自适应降级为 standby！
	if h.role == "active" && res.Role == "active" && h.mode == "primary" {
		log.Printf("[HA Manager] 脑裂防护触发：检测到对端 %s 已经是 Active 状态，本节点自适应降级为 Standby", h.peerURL)
		h.role = "standby"
		h.releaseVIPLocked()
		h.mu.Unlock()
		// 触发立即全量同步
		go h.syncEngine.TriggerSyncNow()
		return
	}
	h.mu.Unlock()
}

func (h *HAManager) markPeerFailure(reason string, failoverLimit int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.peerOnline = false
	h.consecutiveFailures++

	// 1. 若本节点为 Standby 备机，且连续探测超时超过阀值，执行防脑裂校验后再决定是否晋升
	if h.role == "standby" && h.consecutiveFailures >= failoverLimit {
		safe, blockReason, gwOnline, wTotal, wOnline := CheckSplitBrainSafety(
			h.gatewayIP,
			h.cfg.EnableGatewayCheck,
			h.cfg.EnableWorkerQuorum,
			h.store,
		)

		h.gatewayOnline = gwOnline
		h.workerQuorumTotal = wTotal
		h.workerQuorumOnline = wOnline
		h.quorumPassed = safe

		if !safe {
			h.splitBrainBlocked = true
			h.blockedReason = blockReason
			log.Printf("[HA 防脑裂拦截] %s，已阻断备机晋升以防双主脑裂！(网关正常: %v, Worker 多数派: %d/%d)",
				blockReason, gwOnline, wOnline, wTotal)
			return
		}

		// 双重防脑裂校验通过，允许安全晋升为 Active
		h.splitBrainBlocked = false
		h.blockedReason = ""
		log.Printf("[HA Failover] 对端主节点连续 %d 次无响应 (%s)，防脑裂校验通过 (网关正常, Worker 多数派 %d/%d)，触发自动晋升为 Active 主节点！",
			h.consecutiveFailures, reason, wOnline, wTotal)
		h.role = "active"
		h.failoverCount++
		h.consecutiveFailures = 0
		h.bindVIPLocked()
		return
	}

	// 2. 若本节点为 Active 主节点，但与对端长时间失联，执行自省检查是否被孤立在小分区中
	if h.role == "active" && h.cfg.EnableWorkerQuorum && h.consecutiveFailures >= failoverLimit {
		total, online, passed := CheckWorkerQuorum(h.store, 1500*time.Millisecond)
		h.workerQuorumTotal = total
		h.workerQuorumOnline = online
		h.quorumPassed = passed
		if total > 0 && !passed {
			// 主节点连通的 Worker 不足半数，说明主节点已被隔离在网络小分区！主动退位让贤，防止双主
			log.Printf("[HA 防脑裂自省] 主节点与对端失联且连通 Worker (%d/%d) 未达到多数派 (> 50%%)，主动自适应降级为 Standby 并释放 VIP！", online, total)
			h.role = "standby"
			h.splitBrainBlocked = true
			h.blockedReason = fmt.Sprintf("主节点处于小分区，连通 Worker (%d/%d) 不足半数，已主动降级", online, total)
			h.releaseVIPLocked()
		}
	}
}

// PromoteToActive 手动或自动提升为主节点
func (h *HAManager) PromoteToActive(reason string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.role == "active" {
		return nil
	}
	log.Printf("[HA Manager] 节点晋升为 Active 主节点 (原因: %s)", reason)
	h.role = "active"
	h.failoverCount++
	h.bindVIPLocked()
	return nil
}

// DemoteToStandby 降级为 Standby 备用节点
func (h *HAManager) DemoteToStandby(reason string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.role == "standby" {
		return nil
	}
	log.Printf("[HA Manager] 节点降级为 Standby 备节点 (原因: %s)", reason)
	h.role = "standby"
	h.releaseVIPLocked()
	return nil
}

// Switchover 管理员在 Web 控制台一键发起主备平滑倒换
func (h *HAManager) Switchover() error {
	h.mu.RLock()
	isAct := h.role == "active"
	peerURL := h.peerURL
	peerOnline := h.peerOnline
	h.mu.RUnlock()

	if h.mode == "standalone" {
		return fmt.Errorf("单节点模式不支持主备倒换")
	}
	if !peerOnline {
		return fmt.Errorf("对端节点当前不在线，无法执行平滑倒换")
	}

	if isAct {
		// 主节点发起倒换：通知备节点晋升，然后自身降级
		promoteURL := fmt.Sprintf("%s/api/ha/promote", peerURL)
		req, _ := http.NewRequest("POST", promoteURL, nil)
		req.Header.Set("X-Cluster-Token", h.cfg.ClusterToken)
		resp, err := h.client.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			return fmt.Errorf("通知对端备节点晋升失败: %v", err)
		}
		resp.Body.Close()

		// 自身降级
		return h.DemoteToStandby("管理员手动发起主备倒换")
	} else {
		// 备节点发起倒换：通知主节点降级，然后自身晋升
		demoteURL := fmt.Sprintf("%s/api/ha/demote", peerURL)
		req, _ := http.NewRequest("POST", demoteURL, nil)
		req.Header.Set("X-Cluster-Token", h.cfg.ClusterToken)
		resp, err := h.client.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			return fmt.Errorf("通知对端主节点降级失败: %v", err)
		}
		resp.Body.Close()

		// 自身晋升
		return h.PromoteToActive("管理员手动发起主备倒换")
	}
}

// ================= VIP 虚拟 IP 漂移管理 =================

func (h *HAManager) bindVIP() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bindVIPLocked()
}

func (h *HAManager) bindVIPLocked() {
	if h.cfg.VIP == "" {
		return
	}
	iface := h.cfg.VIPInterface
	if iface == "" {
		iface = detectDefaultInterface()
	}
	if iface == "" {
		log.Printf("[HA VIP] 未找到可用网络接口，跳过 VIP 绑定")
		return
	}

	log.Printf("[HA VIP] 正在将虚拟高可用 IP %s 绑定到网卡 %s...", h.cfg.VIP, iface)
	// 执行 ip addr add
	cmd := exec.Command("ip", "addr", "add", h.cfg.VIP, "dev", iface)
	if out, err := cmd.CombinedOutput(); err != nil {
		if !strings.Contains(string(out), "File exists") {
			log.Printf("[HA VIP] 绑定 VIP 警告: %v (%s)", err, strings.TrimSpace(string(out)))
		}
	}

	// 发送免费 ARP 广播
	vipIP := strings.Split(h.cfg.VIP, "/")[0]
	_ = exec.Command("arping", "-c", "2", "-U", "-I", iface, vipIP).Run()

	h.vipActive = true
	log.Printf("[HA VIP] 虚拟 IP %s 绑定成功并已广播免费 ARP！", h.cfg.VIP)
}

func (h *HAManager) releaseVIP() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.releaseVIPLocked()
}

func (h *HAManager) releaseVIPLocked() {
	if h.cfg.VIP == "" || !h.vipActive {
		return
	}
	iface := h.cfg.VIPInterface
	if iface == "" {
		iface = detectDefaultInterface()
	}
	if iface == "" {
		return
	}

	log.Printf("[HA VIP] 正在释放虚拟高可用 IP %s (网卡: %s)...", h.cfg.VIP, iface)
	_ = exec.Command("ip", "addr", "del", h.cfg.VIP, "dev", iface).Run()
	h.vipActive = false
}

// detectDefaultInterface 自动探测默认出口网卡
func detectDefaultInterface() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "eth0"
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagLoopback == 0 {
			addrs, _ := iface.Addrs()
			if len(addrs) > 0 {
				return iface.Name
			}
		}
	}
	return "eth0"
}
