package config

import (
	"encoding/json"
	"os"
	"path/filepath"

	"dist-log-analyzer/internal/model"
)

// Config 系统配置
type Config struct {
	Role         string `json:"role"`           // manager | worker
	ListenHost   string `json:"listen_host"`    // 0.0.0.0
	Port         int    `json:"port"`           // manager 默认为 8080, worker 默认为 8081
	ManagerURL   string `json:"manager_url"`    // worker 连接 manager 的完整地址，支持逗号分隔的主备多地址
	ClusterToken string `json:"cluster_token"`  // 集群内部通信鉴权 Token
	NodeName     string `json:"node_name"`      // 当前节点名称
	AdvertiseIP  string `json:"advertise_ip"`   // 对外广播 IP
	DataDir      string `json:"data_dir"`       // 数据主目录，默认 ./data
	JWTSecret    string `json:"jwt_secret"`     // JWT 密钥

	// HA 高可用主备配置
	HAMode               string `json:"ha_mode"`                // standalone (单节点) | primary (主节点) | backup (备节点)
	PeerURL              string `json:"peer_url"`               // 对端 Manager 节点地址，如 http://192.168.1.11:8080
	HeartbeatIntervalSec int    `json:"heartbeat_interval_sec"` // 心跳间隔，默认 2 秒
	FailoverTimeoutSec   int    `json:"failover_timeout_sec"`   // 判定主节点宕机超时，默认 6 秒
	SyncIntervalSec      int    `json:"sync_interval_sec"`      // 数据快照自动同步周期，默认 5 秒
	VIP                  string `json:"vip"`                    // 可选虚拟 IP (如 192.168.1.200/24)
	VIPInterface         string `json:"vip_interface"`          // 虚拟 IP 绑定网卡 (默认自动探测)

	// 防脑裂保护配置
	GatewayIP          string `json:"gateway_ip"`            // 默认网关 IP (为空时自动从系统路由探测)
	EnableGatewayCheck bool   `json:"enable_gateway_check"`  // 开启网关连通性自检 (默认 true)
	EnableWorkerQuorum bool   `json:"enable_worker_quorum"`  // 开启 Worker 反向多数派仲裁 (默认 true)

	// AI 大模型智能分析配置
	AI model.AIConfig `json:"ai"`

	InitialAdmin struct {
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"initial_admin"`
}

// HAConfig 管理页面上可视化配置的高可用与网络参数
type HAConfig struct {
	HAMode               string `json:"ha_mode"`
	PeerURL              string `json:"peer_url"`
	GatewayIP            string `json:"gateway_ip"`
	EnableGatewayCheck   bool   `json:"enable_gateway_check"`
	EnableWorkerQuorum   bool   `json:"enable_worker_quorum"`
	VIP                  string `json:"vip"`
	VIPInterface         string `json:"vip_interface"`
	HeartbeatIntervalSec int    `json:"heartbeat_interval_sec"`
	FailoverTimeoutSec   int    `json:"failover_timeout_sec"`
	SyncIntervalSec      int    `json:"sync_interval_sec"`
}

func (c *Config) GetHAConfig() HAConfig {
	return HAConfig{
		HAMode:               c.HAMode,
		PeerURL:              c.PeerURL,
		GatewayIP:            c.GatewayIP,
		EnableGatewayCheck:   c.EnableGatewayCheck,
		EnableWorkerQuorum:   c.EnableWorkerQuorum,
		VIP:                  c.VIP,
		VIPInterface:         c.VIPInterface,
		HeartbeatIntervalSec: c.HeartbeatIntervalSec,
		FailoverTimeoutSec:   c.FailoverTimeoutSec,
		SyncIntervalSec:      c.SyncIntervalSec,
	}
}

func (c *Config) ApplyHAConfig(hac HAConfig) {
	if hac.HAMode != "" {
		c.HAMode = hac.HAMode
	}
	c.PeerURL = hac.PeerURL
	c.GatewayIP = hac.GatewayIP
	c.EnableGatewayCheck = hac.EnableGatewayCheck
	c.EnableWorkerQuorum = hac.EnableWorkerQuorum
	c.VIP = hac.VIP
	c.VIPInterface = hac.VIPInterface
	if hac.HeartbeatIntervalSec > 0 {
		c.HeartbeatIntervalSec = hac.HeartbeatIntervalSec
	}
	if hac.FailoverTimeoutSec > 0 {
		c.FailoverTimeoutSec = hac.FailoverTimeoutSec
	}
	if hac.SyncIntervalSec > 0 {
		c.SyncIntervalSec = hac.SyncIntervalSec
	}
}

// DefaultConfig 默认配置
func DefaultConfig() *Config {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "node-default"
	}
	cfg := &Config{
		Role:                 "manager",
		ListenHost:           "0.0.0.0",
		Port:                 8080,
		ManagerURL:           "http://127.0.0.1:8080",
		ClusterToken:         "dist-log-cluster-secret-token",
		NodeName:             hostname,
		AdvertiseIP:          "127.0.0.1",
		DataDir:              "./data",
		JWTSecret:            "dist-log-jwt-secret-key-2026",
		HAMode:               "standalone",
		HeartbeatIntervalSec: 2,
		FailoverTimeoutSec:   6,
		SyncIntervalSec:      5,
		EnableGatewayCheck:   true,
		EnableWorkerQuorum:   true,
		AI: model.AIConfig{
			Enabled:             true,
			Provider:            "mock", // 默认开箱即用内置自研推断引擎，亦支持切换为 ollama, vllm, openai
			BaseURL:             "http://127.0.0.1:11434/v1",
			Model:               "deepseek-r1:70b",
			TimeoutSec:          120,
			AutoAnalyzeCritical: false,
		},
	}
	cfg.InitialAdmin.Username = "admin"
	cfg.InitialAdmin.Password = "admin123"
	return cfg
}

// LoadConfig 从指定路径加载或生成配置文件
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			_ = os.MkdirAll(filepath.Dir(path), 0755)
			b, _ := json.MarshalIndent(cfg, "", "  ")
			_ = os.WriteFile(path, b, 0644)
			return cfg, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
