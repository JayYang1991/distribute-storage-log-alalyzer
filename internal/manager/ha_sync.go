package manager

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"dist-log-analyzer/internal/store"
)

// HASyncEngine 负责主备节点间数据库快照与元数据的在线同步
type HASyncEngine struct {
	ha    *HAManager
	store *store.Store

	lastTxID      int
	lastSyncTime  time.Time
	lastSyncBytes int64
	syncStatus    string // synced | syncing | error | none
	syncErr       error
}

func NewHASyncEngine(ha *HAManager, st *store.Store) *HASyncEngine {
	return &HASyncEngine{
		ha:         ha,
		store:      st,
		syncStatus: "none",
	}
}

// Start 启动后台定时同步任务（仅在备节点 Standby 下周期性从对端 Active 拉取）
func (e *HASyncEngine) Start(ctx context.Context) {
	interval := time.Duration(e.ha.cfg.SyncIntervalSec) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// 启动后先尝试首次同步
	if !e.ha.IsActive() && e.ha.peerURL != "" {
		e.TriggerSyncNow()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !e.ha.IsActive() && e.ha.peerURL != "" {
				e.syncOnce()
			}
		}
	}
}

// TriggerSyncNow 异步触发一次立即同步
func (e *HASyncEngine) TriggerSyncNow() {
	go e.syncOnce()
}

// syncOnce 执行单次快照拉取与原子热重载
func (e *HASyncEngine) syncOnce() {
	if e.ha.IsActive() || e.ha.peerURL == "" {
		return
	}

	e.syncStatus = "syncing"
	snapshotURL := fmt.Sprintf("%s/api/ha/snapshot", e.ha.peerURL)
	req, err := http.NewRequest("GET", snapshotURL, nil)
	if err != nil {
		e.syncStatus = "error"
		e.syncErr = err
		return
	}
	req.Header.Set("X-Cluster-Token", e.ha.cfg.ClusterToken)
	if e.lastTxID > 0 {
		req.Header.Set("If-None-Match", fmt.Sprintf(`W/"tx-%d"`, e.lastTxID))
		req.Header.Set("X-Last-TxID", strconv.Itoa(e.lastTxID))
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		e.syncStatus = "error"
		e.syncErr = err
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		// 主节点数据事务版本无更新，跳过全量文件下载与数据库热重载
		e.syncStatus = "synced"
		e.syncErr = nil
		e.lastSyncTime = time.Now()
		return
	}

	if resp.StatusCode != http.StatusOK {
		e.syncStatus = "error"
		e.syncErr = fmt.Errorf("主节点响应异常状态码: %d", resp.StatusCode)
		return
	}

	// 原子重载快照
	bytesWritten, err := e.store.ReloadFromSnapshot(resp.Body)
	if err != nil {
		e.syncStatus = "error"
		e.syncErr = fmt.Errorf("快照重载失败: %w", err)
		log.Printf("[HA Sync Error] 备节点同步失败: %v", err)
		return
	}

	// 更新同步后的最新 TxID
	if headerTxID := resp.Header.Get("X-DB-TxID"); headerTxID != "" {
		if id, pErr := strconv.Atoi(headerTxID); pErr == nil {
			e.lastTxID = id
		}
	} else if currentTxID, cErr := e.store.CurrentTxID(); cErr == nil {
		e.lastTxID = currentTxID
	}

	e.syncStatus = "synced"
	e.syncErr = nil
	e.lastSyncTime = time.Now()
	e.lastSyncBytes = bytesWritten

	log.Printf("[HA Sync] 备机成功从主节点同步最新数据快照 (重载大小: %d 字节, TxID: %d)", bytesWritten, e.lastTxID)
}
