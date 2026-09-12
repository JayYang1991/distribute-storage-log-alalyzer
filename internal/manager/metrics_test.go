package manager

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"dist-log-analyzer/internal/config"
	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/store"
)

func TestMetricsAndHealthProbes(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "metrics_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := config.DefaultConfig()
	cfg.DataDir = tmpDir
	s, err := store.NewStore(cfg)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer s.Close()

	srv := &Server{
		cfg:   cfg,
		store: s,
		ha:    NewHAManager(cfg, s),
	}

	// 1. 测试 /healthz
	wHealth := httptest.NewRecorder()
	reqHealth := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	srv.handleHealthz(wHealth, reqHealth)

	if wHealth.Code != http.StatusOK {
		t.Errorf("expected /healthz status 200, got: %d", wHealth.Code)
	}
	if !strings.Contains(wHealth.Body.String(), `"status":"ok"`) {
		t.Errorf("expected /healthz body to contain status ok, got: %s", wHealth.Body.String())
	}

	// 2. 测试 /readyz
	wReady := httptest.NewRecorder()
	reqReady := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	srv.handleReadyz(wReady, reqReady)

	if wReady.Code != http.StatusOK {
		t.Errorf("expected /readyz status 200, got: %d", wReady.Code)
	}
	if !strings.Contains(wReady.Body.String(), `"status":"ready"`) {
		t.Errorf("expected /readyz body to contain status ready, got: %s", wReady.Body.String())
	}

	// 3. 测试 /metrics
	_ = s.SaveNode(&model.Node{
		ID:     "node_metric_1",
		Name:   "worker-metrics",
		Role:   "worker",
		Status: "online",
	})

	wMetrics := httptest.NewRecorder()
	reqMetrics := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	srv.handleMetrics(wMetrics, reqMetrics)

	if wMetrics.Code != http.StatusOK {
		t.Errorf("expected /metrics status 200, got: %d", wMetrics.Code)
	}
	metricsBody := wMetrics.Body.String()
	if !strings.Contains(metricsBody, "dist_log_nodes_total 1") {
		t.Errorf("expected metrics to contain dist_log_nodes_total 1, got:\n%s", metricsBody)
	}
	if !strings.Contains(metricsBody, "dist_log_nodes_online 1") {
		t.Errorf("expected metrics to contain dist_log_nodes_online 1, got:\n%s", metricsBody)
	}
	if !strings.Contains(metricsBody, "go_goroutines") {
		t.Errorf("expected metrics to contain go_goroutines, got:\n%s", metricsBody)
	}
}
