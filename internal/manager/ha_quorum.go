package manager

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/store"
)

// DetectDefaultGateway 从 Linux 内核 /proc/net/route 解析默认网关 IP
func DetectDefaultGateway() string {
	f, err := os.Open("/proc/net/route")
	if err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			// 格式: Iface Destination Gateway Flags ...
			// 默认路由的 Destination 为 00000000
			if len(fields) >= 3 && fields[1] == "00000000" {
				gwHex := fields[2]
				if gwHex != "00000000" && len(gwHex) == 8 {
					// 解码十六进制 IPv4 字节序 (例如 017AA8C0 -> b[0]=01, b[1]=7a, b[2]=a8, b[3]=c0)
					// 实际对应真实网关 192.168.122.1 (b[3].b[2].b[1].b[0])
					b, err := hex.DecodeString(gwHex)
					if err == nil && len(b) == 4 {
						ip := net.IPv4(b[3], b[2], b[1], b[0])
						return ip.String()
					}
				}
			}
		}
	}

	// 降级使用 ip route 命令提取
	out, err := exec.Command("ip", "route", "show", "default").Output()
	if err == nil {
		fields := strings.Fields(string(out))
		for i, field := range fields {
			if field == "via" && i+1 < len(fields) {
				return fields[i+1]
			}
		}
	}

	return ""
}

// CheckGatewayPing 探测默认网关连通性 (使用标准 ICMP ping)
func CheckGatewayPing(gwIP string, timeout time.Duration) bool {
	if gwIP == "" {
		return true
	}

	// 优先使用标准系统 ping 命令 (发送 2 个包，等待 1 秒)
	cmd := exec.Command("ping", "-c", "2", "-W", "1", gwIP)
	if err := cmd.Run(); err == nil {
		return true
	}

	return false
}

// CheckWorkerQuorum 并发探测所有注册 Worker 节点，判定是否达到过半数多数派 (M > N/2)
func CheckWorkerQuorum(st *store.Store, timeout time.Duration) (total int, online int, passed bool) {
	if st == nil {
		return 0, 0, true
	}

	nodes, err := st.ListNodes()
	if err != nil || len(nodes) == 0 {
		return 0, 0, true
	}

	var workers []*model.Node
	for _, n := range nodes {
		if n.Role == "worker" {
			workers = append(workers, n)
		}
	}

	total = len(workers)
	if total == 0 {
		// 集群尚无 worker 业务节点，直接放行
		return 0, 0, true
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	onlineCount := 0
	client := &http.Client{Timeout: timeout}

	for _, w := range workers {
		wg.Add(1)
		go func(node *model.Node) {
			defer wg.Done()
			url := fmt.Sprintf("http://%s:%d/api/worker/health", node.IP, node.Port)
			resp, err := client.Get(url)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					mu.Lock()
					onlineCount++
					mu.Unlock()
				}
			}
		}(w)
	}

	wg.Wait()
	online = onlineCount

	// 必须严格超过半数: online > total / 2
	passed = online > (total / 2)
	return total, online, passed
}

// CheckSplitBrainSafety 综合防脑裂安全性检查
func CheckSplitBrainSafety(gatewayIP string, enableGateway bool, enableWorkerQuorum bool, st *store.Store) (safe bool, reason string, gwOnline bool, wTotal int, wOnline int) {
	gwOnline = true
	// 1. 网关连通性检查
	if enableGateway {
		targetGW := gatewayIP
		if targetGW == "" {
			targetGW = DetectDefaultGateway()
		}

		if targetGW != "" {
			gwOnline = CheckGatewayPing(targetGW, 1500*time.Millisecond)
			if !gwOnline {
				return false, fmt.Sprintf("默认网关 (%s) 无法连通，判定本机处于网络孤岛状态，阻断晋升", targetGW), false, 0, 0
			}
		}
	}

	// 2. Worker 业务节点多数派仲裁
	if enableWorkerQuorum {
		total, online, passed := CheckWorkerQuorum(st, 1500*time.Millisecond)
		wTotal = total
		wOnline = online
		if total > 0 && !passed {
			return false, fmt.Sprintf("连通的 Worker 节点数 (%d/%d) 未达到多数派法定人数 (> 50%%)，阻断晋升", online, total), gwOnline, wTotal, wOnline
		}
	}

	return true, "防脑裂校验通过", gwOnline, wTotal, wOnline
}
