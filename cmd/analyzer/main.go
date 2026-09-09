package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"dist-log-analyzer/internal/config"
	"dist-log-analyzer/internal/manager"
	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/store"
	"dist-log-analyzer/internal/web"
	"dist-log-analyzer/internal/worker"
)

var Version = "1.0.0"

func main() {
	if len(os.Args) < 2 {
		printHelp()
		os.Exit(1)
	}

	subCmd := os.Args[1]
	switch subCmd {
	case "manager":
		runManager(os.Args[2:])
	case "worker":
		runWorker(os.Args[2:])
	case "version":
		fmt.Printf("分布式存储日志分析系统 (Distributed Storage Log Analyzer)\n")
		fmt.Printf("版本: %s\n", Version)
		fmt.Printf("架构特性: 纯静态单一二进制 (CGO_ENABLED=0), 零外部依赖, 原生适配所有通用 RedHat/CentOS 衍生 Linux\n")
	default:
		printHelp()
		os.Exit(1)
	}
}

func printHelp() {
	fmt.Printf(`分布式存储日志分析系统 (Distributed Storage Log Analyzer)

使用方法:
  dist-log-analyzer <子命令> [参数]

可用子命令:
  manager    启动管理组件服务（提供 Web 管理界面、多用户空间隔离、集群调度与一键远程安装）
  worker     启动业务分析计算节点（负责解压、分布式检索与故障诊断）
  version    查看系统版本与构建信息

示例:
  # 启动管理节点
  ./dist-log-analyzer manager --port=8080 --data-dir=/opt/dist-log/data

  # 启动业务节点
  ./dist-log-analyzer worker --port=8081 --manager-url=http://192.168.1.100:8080 --cluster-token=...
`)
}

func runManager(args []string) {
	fs := flag.NewFlagSet("manager", flag.ExitOnError)
	port := fs.Int("port", 8080, "管理节点 HTTP 监听端口")
	host := fs.String("host", "0.0.0.0", "监听地址")
	dataDir := fs.String("data-dir", "./data", "数据存储与用户隔离根目录")
	advertiseIP := fs.String("advertise-ip", "", "管理节点对外广播 IP (默认自动探测)")
	token := fs.String("cluster-token", "dist-log-cluster-secret-token", "集群内部通信握手 Token")
	adminUser := fs.String("admin-user", "admin", "初始管理员账号")
	adminPass := fs.String("admin-pass", "admin123", "初始管理员密码")

	// HA 参数
	haMode := fs.String("ha-mode", "standalone", "部署模式: standalone (单节点) | primary (主节点) | backup (备节点)")
	peerURL := fs.String("peer-url", "", "对端 Manager 节点地址 (例如 http://192.168.1.11:8080)")
	vip := fs.String("vip", "", "高可用虚拟 IP (例如 192.168.1.200/24)")
	vipInterface := fs.String("vip-interface", "", "虚拟 IP 绑定网卡 (默认自动探测)")
	gatewayIP := fs.String("gateway-ip", "", "默认网关 IP (用于防脑裂自检，默认自动探测)")
	_ = fs.Parse(args)

	cfg := config.DefaultConfig()
	cfg.Role = "manager"
	cfg.Port = *port
	cfg.ListenHost = *host
	cfg.DataDir = *dataDir
	cfg.ClusterToken = *token
	cfg.InitialAdmin.Username = *adminUser
	cfg.InitialAdmin.Password = *adminPass
	cfg.HAMode = *haMode
	cfg.PeerURL = *peerURL
	cfg.VIP = *vip
	cfg.VIPInterface = *vipInterface
	cfg.GatewayIP = *gatewayIP

	if *advertiseIP != "" {
		cfg.AdvertiseIP = *advertiseIP
	} else {
		cfg.AdvertiseIP = manager.GetOutboundIP()
	}

	st, err := store.NewStore(cfg)
	if err != nil {
		log.Fatalf("初始化数据存储引擎失败: %v", err)
	}
	defer st.Close()

	// 若数据库中已有通过 Web 管理页面配置的高可用与网络配置，自动加载并应用 (支持网页配置持久化)
	if savedHACfg, err := st.GetHAConfig(); err == nil && savedHACfg != nil {
		cfg.ApplyHAConfig(*savedHACfg)
	}

	// 保存自身作为管理节点展示
	mgrNode := &model.Node{
		ID:       fmt.Sprintf("manager_%s", cfg.HAMode),
		Name:     fmt.Sprintf("manager-%s", cfg.HAMode),
		IP:       cfg.AdvertiseIP,
		Port:     cfg.Port,
		Role:     "manager",
		Status:   "online",
		Resource: model.SystemResource{OS: "linux", Arch: "amd64"},
	}
	_ = st.SaveNode(mgrNode)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	staticFS := web.GetFileSystem()
	server := manager.NewServer(cfg, st, staticFS)

	fmt.Println("==================================================================")
	fmt.Println("   分布式存储日志分析系统 (Distributed Storage Log Analyzer)      ")
	fmt.Println("==================================================================")
	fmt.Printf(" 管理节点 Web 控制台已就绪: http://%s:%d\n", cfg.AdvertiseIP, cfg.Port)
	fmt.Printf(" 部署运行模式: %s (HA Mode)\n", cfg.HAMode)
	if cfg.PeerURL != "" {
		fmt.Printf(" 对端节点地址: %s\n", cfg.PeerURL)
	}
	if cfg.VIP != "" {
		fmt.Printf(" 虚拟高可用 IP: %s\n", cfg.VIP)
	}
	fmt.Printf(" 本机数据存储隔离目录: %s\n", cfg.DataDir)
	fmt.Printf(" 默认管理员初始账号: %s / %s\n", cfg.InitialAdmin.Username, cfg.InitialAdmin.Password)
	fmt.Println("==================================================================")

	if err := server.Start(ctx); err != nil {
		log.Fatalf("管理组件启动失败: %v", err)
	}
}

func runWorker(args []string) {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	port := fs.Int("port", 8081, "业务节点 HTTP 监听端口")
	host := fs.String("host", "0.0.0.0", "监听地址")
	managerURL := fs.String("manager-url", "http://127.0.0.1:8080", "管理节点访问 URL")
	token := fs.String("cluster-token", "dist-log-cluster-secret-token", "集群内部通信握手 Token")
	nodeName := fs.String("node-name", "", "节点名称 (默认主机名)")
	advertiseIP := fs.String("advertise-ip", "", "业务节点对外 IP (默认自动探测)")
	dataDir := fs.String("data-dir", "./worker_data", "工作数据目录")
	_ = fs.Parse(args)

	cfg := config.DefaultConfig()
	cfg.Role = "worker"
	cfg.Port = *port
	cfg.ListenHost = *host
	cfg.ManagerURL = *managerURL
	cfg.ClusterToken = *token
	cfg.DataDir = *dataDir

	if *advertiseIP != "" {
		cfg.AdvertiseIP = *advertiseIP
	} else {
		cfg.AdvertiseIP = manager.GetOutboundIP()
	}

	if *nodeName != "" {
		cfg.NodeName = *nodeName
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	agent := worker.NewAgent(cfg)

	fmt.Println("==================================================================")
	fmt.Println("   分布式存储日志分析系统 - 业务组件 Worker                      ")
	fmt.Println("==================================================================")
	fmt.Printf(" 业务组件运行端口: %d\n", cfg.Port)
	fmt.Printf(" 目标管理节点 URL: %s\n", cfg.ManagerURL)
	fmt.Println("==================================================================")

	if err := agent.Start(ctx); err != nil {
		log.Fatalf("业务组件启动失败: %v", err)
	}
}
