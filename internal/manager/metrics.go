package manager

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"sync/atomic"
	"time"
)

// RecordSearchMetrics 记录检索请求指标
func (s *Server) RecordSearchMetrics(duration time.Duration) {
	atomic.AddUint64(&s.searchRequestsTotal, 1)
	atomic.AddUint64(&s.searchLatencySumMs, uint64(duration.Milliseconds()))
}

// handleHealthz 存活探针 (Liveness Probe)
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "ok",
		"role":   "manager",
		"time":   time.Now().Format(time.RFC3339),
	})
}

// handleReadyz 就绪探针 (Readiness Probe)
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	// 检查元数据数据库是否可用
	if _, err := s.store.ListNodes(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "not_ready",
			"error":  fmt.Sprintf("db check failed: %v", err),
		})
		return
	}

	haRole := "standalone"
	if s.ha != nil {
		haRole = s.ha.GetStatus().Role
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":  "ready",
		"role":    "manager",
		"db":      "ok",
		"ha_role": haRole,
	})
}

// handleMetrics 导出标准 Prometheus Text 0.0.4 格式指标
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	nodes, _ := s.store.ListNodes()
	var onlineNodes, totalCapacity, totalUsed int64
	for _, n := range nodes {
		if n.Status == "online" {
			onlineNodes++
		}
		totalCapacity += n.Resource.DiskTotalMB * 1024 * 1024
		totalUsed += n.Resource.DiskUsedMB * 1024 * 1024
	}

	archives, _ := s.store.ListArchives("", true)
	var totalArchiveBytes int64
	for _, arc := range archives {
		totalArchiveBytes += arc.Size
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	reqTotal := atomic.LoadUint64(&s.searchRequestsTotal)
	latencyMs := atomic.LoadUint64(&s.searchLatencySumMs)
	latencySec := float64(latencyMs) / 1000.0

	haRole := "standalone"
	if s.ha != nil {
		haRole = s.ha.GetStatus().Role
	}

	fmt.Fprintf(w, "# HELP dist_log_info Build and version info\n")
	fmt.Fprintf(w, "# TYPE dist_log_info gauge\n")
	fmt.Fprintf(w, "dist_log_info{role=\"manager\",ha_role=\"%s\"} 1\n", haRole)

	fmt.Fprintf(w, "# HELP dist_log_nodes_total Total number of registered worker nodes\n")
	fmt.Fprintf(w, "# TYPE dist_log_nodes_total gauge\n")
	fmt.Fprintf(w, "dist_log_nodes_total %d\n", len(nodes))

	fmt.Fprintf(w, "# HELP dist_log_nodes_online Current number of online worker nodes\n")
	fmt.Fprintf(w, "# TYPE dist_log_nodes_online gauge\n")
	fmt.Fprintf(w, "dist_log_nodes_online %d\n", onlineNodes)

	fmt.Fprintf(w, "# HELP dist_log_storage_capacity_bytes Total cluster storage capacity in bytes\n")
	fmt.Fprintf(w, "# TYPE dist_log_storage_capacity_bytes gauge\n")
	fmt.Fprintf(w, "dist_log_storage_capacity_bytes %d\n", totalCapacity)

	fmt.Fprintf(w, "# HELP dist_log_storage_used_bytes Total cluster storage used in bytes\n")
	fmt.Fprintf(w, "# TYPE dist_log_storage_used_bytes gauge\n")
	fmt.Fprintf(w, "dist_log_storage_used_bytes %d\n", totalUsed)

	fmt.Fprintf(w, "# HELP dist_log_archives_count Total number of stored log archives\n")
	fmt.Fprintf(w, "# TYPE dist_log_archives_count gauge\n")
	fmt.Fprintf(w, "dist_log_archives_count %d\n", len(archives))

	fmt.Fprintf(w, "# HELP dist_log_archives_bytes Total compressed size of stored log archives\n")
	fmt.Fprintf(w, "# TYPE dist_log_archives_bytes gauge\n")
	fmt.Fprintf(w, "dist_log_archives_bytes %d\n", totalArchiveBytes)

	fmt.Fprintf(w, "# HELP dist_log_search_requests_total Total number of log search queries handled\n")
	fmt.Fprintf(w, "# TYPE dist_log_search_requests_total counter\n")
	fmt.Fprintf(w, "dist_log_search_requests_total %d\n", reqTotal)

	fmt.Fprintf(w, "# HELP dist_log_search_duration_seconds_total Total duration of log search queries in seconds\n")
	fmt.Fprintf(w, "# TYPE dist_log_search_duration_seconds_total counter\n")
	fmt.Fprintf(w, "dist_log_search_duration_seconds_total %.4f\n", latencySec)

	// Runtime 指标
	fmt.Fprintf(w, "# HELP go_goroutines Number of goroutines that currently exist\n")
	fmt.Fprintf(w, "# TYPE go_goroutines gauge\n")
	fmt.Fprintf(w, "go_goroutines %d\n", runtime.NumGoroutine())

	fmt.Fprintf(w, "# HELP go_memstats_alloc_bytes Number of bytes allocated and still in use\n")
	fmt.Fprintf(w, "# TYPE go_memstats_alloc_bytes gauge\n")
	fmt.Fprintf(w, "go_memstats_alloc_bytes %d\n", m.Alloc)

	fmt.Fprintf(w, "# HELP go_memstats_sys_bytes Number of bytes obtained from system\n")
	fmt.Fprintf(w, "# TYPE go_memstats_sys_bytes gauge\n")
	fmt.Fprintf(w, "go_memstats_sys_bytes %d\n", m.Sys)
}
