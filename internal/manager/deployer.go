package manager

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"dist-log-analyzer/internal/model"

	"golang.org/x/crypto/ssh"
)

// SSHDeployOptions 远程一键部署选项
type SSHDeployOptions struct {
	Host         string `json:"host"`
	Port         int    `json:"port"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	PrivateKey   string `json:"private_key"`
	NodeName     string `json:"node_name"`
	WorkerPort   int    `json:"worker_port"`
	ManagerURL   string `json:"manager_url"`
	ClusterToken string `json:"cluster_token"`
	InstallDir   string `json:"install_dir"` // 默认 /opt/dist-log-worker

	// 磁盘格式化与挂载选项
	DiskDevice string `json:"disk_device"` // 例如 /dev/sdb
	FSType     string `json:"fs_type"`     // ext4 或 xfs
	MountPoint string `json:"mount_point"` // 例如 /data/dist-log-storage
	FormatDisk bool   `json:"format_disk"` // 是否执行格式化
}

func createSSHClient(opts SSHDeployOptions) (*ssh.Client, error) {
	if opts.Port <= 0 {
		opts.Port = 22
	}
	var authMethods []ssh.AuthMethod
	if opts.Password != "" {
		authMethods = append(authMethods, ssh.Password(opts.Password))
	}
	if opts.PrivateKey != "" {
		signer, err := ssh.ParsePrivateKey([]byte(opts.PrivateKey))
		if err == nil {
			authMethods = append(authMethods, ssh.PublicKeys(signer))
		}
	}

	sshConfig := &ssh.Config{}
	sshConfig.SetDefaults()

	clientConfig := &ssh.ClientConfig{
		User:            opts.Username,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
		Config:          *sshConfig,
	}

	return ssh.Dial("tcp", fmt.Sprintf("%s:%d", opts.Host, opts.Port), clientConfig)
}

type rawBlockDevice struct {
	Name       string           `json:"name"`
	Size       int64            `json:"size"`
	Type       string           `json:"type"`
	MountPoint string           `json:"mountpoint"`
	FSType     string           `json:"fstype"`
	Model      string           `json:"model"`
	Children   []rawBlockDevice `json:"children"`
}

// checkBlockDeviceSafety 递归检测块设备及其子分区是否已有文件系统或为系统分区
func checkBlockDeviceSafety(dev rawBlockDevice) (hasFS bool, isSystem bool, partsSummary []string) {
	mountPoint := strings.TrimSpace(dev.MountPoint)
	fsType := strings.TrimSpace(dev.FSType)

	// 检查自身挂载点与系统关键路径
	if isSystemMount(mountPoint) {
		isSystem = true
	}
	if fsType != "" || mountPoint != "" {
		hasFS = true
		partsSummary = append(partsSummary, fmt.Sprintf("%s(fs:%s, mount:%s)", dev.Name, fsType, mountPoint))
	}

	// 递归检查所有子分区
	for _, child := range dev.Children {
		cHasFS, cIsSystem, cSummary := checkBlockDeviceSafety(child)
		if cIsSystem {
			isSystem = true
		}
		if cHasFS {
			hasFS = true
		}
		partsSummary = append(partsSummary, cSummary...)
	}

	// 只要有子分区（即使尚未格式化），通常也是已被分区表划分的磁盘
	if len(dev.Children) > 0 {
		hasFS = true
	}

	return hasFS, isSystem, partsSummary
}

func isSystemMount(mp string) bool {
	if mp == "" {
		return false
	}
	mp = strings.ToLower(mp)
	return mp == "/" || mp == "/boot" || strings.HasPrefix(mp, "/boot/") ||
		mp == "/home" || mp == "/usr" || mp == "/var" || strings.Contains(mp, "swap")
}

// DetectRemoteDisks 通过 SSH 远程检测目标主机的物理磁盘与分区列表（执行防呆过滤与状态分析）
func DetectRemoteDisks(opts SSHDeployOptions) ([]model.DiskInfo, error) {
	client, err := createSSHClient(opts)
	if err != nil {
		return nil, fmt.Errorf("SSH 连接失败: %w", err)
	}
	defer client.Close()

	// 优先执行 lsblk JSON 格式化输出 (包括 children)
	cmdJSON := "lsblk -b -J -o NAME,SIZE,TYPE,MOUNTPOINT,FSTYPE,MODEL 2>/dev/null"
	var outBuf bytes.Buffer
	err = runRemoteCmd(client, cmdJSON, &outBuf)
	if err == nil && outBuf.Len() > 0 {
		var res struct {
			BlockDevices []rawBlockDevice `json:"blockdevices"`
		}
		if jsonErr := json.Unmarshal(outBuf.Bytes(), &res); jsonErr == nil && len(res.BlockDevices) > 0 {
			var disks []model.DiskInfo
			for _, b := range res.BlockDevices {
				// 过滤非磁盘设备 (如 loop, rom, zram 等) 以及大小为 0 的读卡器设备
				bType := strings.ToLower(strings.TrimSpace(b.Type))
				if bType == "loop" || bType == "rom" || b.Size <= 0 {
					continue
				}

				hasFS, isSystem, partsSummary := checkBlockDeviceSafety(b)
				canFormat := !hasFS && !isSystem

				var statusText string
				if isSystem {
					statusText = "🚫 系统关键盘 (含系统启动/根分区，严禁格式化)"
				} else if hasFS {
					summary := strings.Join(partsSummary, "; ")
					if summary == "" {
						summary = "含已分配分区"
					}
					statusText = fmt.Sprintf("🚫 已有文件系统/分区 [%s]，防呆锁定", summary)
				} else {
					statusText = "✅ 纯净物理裸盘 (无文件系统/无分区，安全推荐)"
				}

				diskPath := fmt.Sprintf("/dev/%s", b.Name)
				sizeStr := formatBytes(b.Size)
				disks = append(disks, model.DiskInfo{
					Name:       b.Name,
					Path:       diskPath,
					Size:       sizeStr,
					SizeBytes:  b.Size,
					Type:       b.Type,
					MountPoint: b.MountPoint,
					FSType:     b.FSType,
					Model:      strings.TrimSpace(b.Model),
					HasFS:      hasFS,
					IsSystem:   isSystem,
					Partitions: partsSummary,
					StatusText: statusText,
					CanFormat:  canFormat,
				})
			}

			// 排序：安全可用的裸盘优先排在前面，已占用的盘排在后面
			sort.SliceStable(disks, func(i, j int) bool {
				if disks[i].CanFormat && !disks[j].CanFormat {
					return true
				}
				if !disks[i].CanFormat && disks[j].CanFormat {
					return false
				}
				return disks[i].Name < disks[j].Name
			})

			return disks, nil
		}
	}

	// 降级使用普通文本表格解析
	outBuf.Reset()
	cmdFallback := "lsblk -d -n -o NAME,SIZE,TYPE,MOUNTPOINT,FSTYPE 2>/dev/null"
	_ = runRemoteCmd(client, cmdFallback, &outBuf)
	lines := strings.Split(outBuf.String(), "\n")
	var disks []model.DiskInfo
	for _, line := range lines {
		f := strings.Fields(line)
		if len(f) >= 3 {
			bType := strings.ToLower(f[2])
			if bType == "loop" || bType == "rom" {
				continue
			}
			d := model.DiskInfo{
				Name: f[0],
				Path: fmt.Sprintf("/dev/%s", f[0]),
				Size: f[1],
				Type: f[2],
			}
			if len(f) >= 4 && f[3] != "" {
				d.MountPoint = f[3]
			}
			if len(f) >= 5 && f[4] != "" {
				d.FSType = f[4]
			}
			if isSystemMount(d.MountPoint) {
				d.IsSystem = true
			}
			if d.FSType != "" || d.MountPoint != "" {
				d.HasFS = true
			}
			d.CanFormat = !d.HasFS && !d.IsSystem
			if d.IsSystem {
				d.StatusText = "🚫 系统关键盘，严禁格式化"
			} else if d.HasFS {
				d.StatusText = "🚫 已有文件系统，防呆锁定"
			} else {
				d.StatusText = "✅ 纯净物理裸盘，安全推荐"
			}
			disks = append(disks, d)
		}
	}

	sort.SliceStable(disks, func(i, j int) bool {
		if disks[i].CanFormat && !disks[j].CanFormat {
			return true
		}
		if !disks[i].CanFormat && disks[j].CanFormat {
			return false
		}
		return disks[i].Name < disks[j].Name
	})

	return disks, nil
}

// DeployWorkerViaSSH 通过纯 Go SSH 远程登录并在目标机器上一键部署 Worker 业务组件（包含磁盘格式化挂载）
func DeployWorkerViaSSH(opts SSHDeployOptions, logWriter io.Writer) error {
	if opts.WorkerPort <= 0 {
		opts.WorkerPort = 8081
	}
	if opts.InstallDir == "" {
		opts.InstallDir = "/opt/dist-log-worker"
	}
	if opts.NodeName == "" {
		opts.NodeName = fmt.Sprintf("worker-%s", strings.ReplaceAll(opts.Host, ".", "-"))
	}
	if opts.FSType == "" {
		opts.FSType = "ext4"
	}
	if opts.MountPoint == "" {
		opts.MountPoint = "/data/dist-log-storage"
	}

	fmt.Fprintf(logWriter, "[SSH Deploy] 正在连接目标节点 %s:%d (用户: %s)...\n", opts.Host, opts.Port, opts.Username)

	client, err := createSSHClient(opts)
	if err != nil {
		fmt.Fprintf(logWriter, "[SSH Deploy] 连接失败: %v\n", err)
		return fmt.Errorf("SSH 连接失败: %w", err)
	}
	defer client.Close()

	fmt.Fprintf(logWriter, "[SSH Deploy] SSH 认证成功！检测目标系统架构与环境...\n")

	// 1. 检查并格式化挂载存储硬盘
	targetDataDir := fmt.Sprintf("%s/data", opts.InstallDir)

	if opts.DiskDevice != "" {
		fmt.Fprintf(logWriter, "[SSH Deploy] 检测到已选定存储硬盘: %s, 挂载点: %s, 文件系统: %s\n",
			opts.DiskDevice, opts.MountPoint, opts.FSType)

		if opts.FormatDisk {
			fmt.Fprintf(logWriter, "[SSH Deploy] 正在执行磁盘安全防呆检测 (目标: %s)...\n", opts.DiskDevice)
			// 1.1 防呆校验 1：检查是否包含系统启动/根挂载点以及关键路径
			var lsblkBuf bytes.Buffer
			_ = runRemoteCmd(client, fmt.Sprintf("lsblk -n -o NAME,MOUNTPOINT,FSTYPE %s 2>/dev/null", opts.DiskDevice), &lsblkBuf)
			lsblkOutput := lsblkBuf.String()
			for _, line := range strings.Split(lsblkOutput, "\n") {
				fields := strings.Fields(line)
				for _, f := range fields {
					if isSystemMount(f) {
						errMsg := fmt.Sprintf("【安全防呆拦截】目标设备 %s 关联系统关键分区 (%s)，严禁格式化！已中止部署以防系统损毁", opts.DiskDevice, f)
						fmt.Fprintln(logWriter, errMsg)
						return fmt.Errorf("%s", errMsg)
					}
				}
			}

			// 1.2 防呆校验 2：检查是否已经存在文件系统 (如 ext4, xfs, vfat, ntfs, btrfs, swap 等签名)
			var wipefsBuf bytes.Buffer
			_ = runRemoteCmd(client, fmt.Sprintf("wipefs -n %s 2>/dev/null", opts.DiskDevice), &wipefsBuf)
			wipefsOutput := strings.TrimSpace(wipefsBuf.String())
			if len(wipefsOutput) > 0 {
				errMsg := fmt.Sprintf("【安全防呆拦截】目标磁盘 %s 上检测到已有文件系统或分区签名:\n%s\n系统防呆机制已阻止格式化，以防重要数据被清除！请选择未格式化的纯净裸盘", opts.DiskDevice, wipefsOutput)
				fmt.Fprintln(logWriter, errMsg)
				return fmt.Errorf("%s", errMsg)
			}

			fmt.Fprintf(logWriter, "[SSH Deploy] 安全防呆检测通过 (确认目标盘 %s 为无文件系统的纯净裸盘)\n", opts.DiskDevice)
			fmt.Fprintf(logWriter, "[SSH Deploy] 正在执行磁盘格式化操作 (格式: %s, 目标: %s)...\n", opts.FSType, opts.DiskDevice)
			// 1.3 先卸载可能已挂载的旧路径
			_ = runRemoteCmd(client, fmt.Sprintf("umount %s 2>/dev/null || true", opts.DiskDevice), logWriter)

			// 1.4 执行格式化命令
			var formatCmd string
			if strings.ToLower(opts.FSType) == "xfs" {
				formatCmd = fmt.Sprintf("mkfs.xfs -f %s", opts.DiskDevice)
			} else {
				formatCmd = fmt.Sprintf("mkfs.ext4 -F %s", opts.DiskDevice)
			}
			if err := runRemoteCmd(client, formatCmd, logWriter); err != nil {
				return fmt.Errorf("格式化硬盘 %s 失败: %w", opts.DiskDevice, err)
			}
			fmt.Fprintf(logWriter, "[SSH Deploy] 磁盘 %s 格式化成功！\n", opts.DiskDevice)
		}

		// 1.3 创建挂载目录并挂载
		fmt.Fprintf(logWriter, "[SSH Deploy] 正在挂载硬盘到: %s...\n", opts.MountPoint)
		mountCmd := fmt.Sprintf("mkdir -p %s && (mount | grep -q 'on %s ' || mount %s %s)",
			opts.MountPoint, opts.MountPoint, opts.DiskDevice, opts.MountPoint)
		if err := runRemoteCmd(client, mountCmd, logWriter); err != nil {
			return fmt.Errorf("挂载硬盘失败: %w", err)
		}

		// 1.4 持久化配置到 /etc/fstab (防止重启后挂载丢失)
		fstabCmd := fmt.Sprintf("grep -v '%s' /etc/fstab > /tmp/fstab.tmp && mv -f /tmp/fstab.tmp /etc/fstab; echo '%s %s %s defaults 0 0' >> /etc/fstab",
			opts.DiskDevice, opts.DiskDevice, opts.MountPoint, opts.FSType)
		_ = runRemoteCmd(client, fstabCmd, logWriter)
		fmt.Fprintf(logWriter, "[SSH Deploy] 磁盘挂载成功并已持久化写入 /etc/fstab！\n")

		// 将 Worker 的主要数据目录指向该格式化后的硬盘挂载目录
		targetDataDir = opts.MountPoint
	}

	// 2. 创建程序与日志目录
	cmdCreateDir := fmt.Sprintf("mkdir -p %s/bin %s/logs %s", opts.InstallDir, opts.InstallDir, targetDataDir)
	if err := runRemoteCmd(client, cmdCreateDir, logWriter); err != nil {
		return fmt.Errorf("创建远程目录失败: %w", err)
	}

	// 3. 检查本地二进制路径并拷贝至远程
	currentExec, err := os.Executable()
	if err != nil {
		currentExec = "/usr/local/bin/dist-log-analyzer"
	}
	fmt.Fprintf(logWriter, "[SSH Deploy] 正在传输业务二进制组件 (%s -> %s/bin/dist-log-analyzer)...\n", currentExec, opts.InstallDir)

	execData, err := os.ReadFile(currentExec)
	if err != nil {
		fmt.Fprintf(logWriter, "[SSH Deploy] 读取本地程序失败，将尝试从 Manager HTTP 下载\n")
		downloadCmd := fmt.Sprintf("curl -sSL -o %s/bin/dist-log-analyzer %s/api/cluster/binary || wget -qO %s/bin/dist-log-analyzer %s/api/cluster/binary",
			opts.InstallDir, opts.ManagerURL, opts.InstallDir, opts.ManagerURL)
		if err := runRemoteCmd(client, downloadCmd, logWriter); err != nil {
			return fmt.Errorf("远程下载二进制失败: %w", err)
		}
	} else {
		remoteDest := fmt.Sprintf("%s/bin/dist-log-analyzer", opts.InstallDir)
		if err := copyBytesToRemote(client, execData, remoteDest, 0755, logWriter); err != nil {
			return fmt.Errorf("文件推送失败: %w", err)
		}
	}

	// 4. 授权并停止旧进程
	stopCmd := fmt.Sprintf("chmod +x %s/bin/dist-log-analyzer && pkill -f '%s/bin/dist-log-analyzer worker' || true", opts.InstallDir, opts.InstallDir)
	_ = runRemoteCmd(client, stopCmd, logWriter)

	// 5. 远程后台启动 Worker 组件 (数据目录指向 targetDataDir)
	startCmd := fmt.Sprintf("nohup %s/bin/dist-log-analyzer worker --port=%d --manager-url=%s --cluster-token=%s --node-name=%s --advertise-ip=%s --data-dir=%s </dev/null > %s/logs/worker.log 2>&1 &",
		opts.InstallDir, opts.WorkerPort, opts.ManagerURL, opts.ClusterToken, opts.NodeName, opts.Host, targetDataDir, opts.InstallDir)

	fmt.Fprintf(logWriter, "[SSH Deploy] 正在远程启动业务组件 Worker 进程 (存储数据目录: %s)...\n", targetDataDir)
	if err := runRemoteCmd(client, startCmd, logWriter); err != nil {
		return fmt.Errorf("远程启动 Worker 进程失败: %w", err)
	}

	// 6. 验证是否监听
	time.Sleep(1500 * time.Millisecond)
	checkCmd := fmt.Sprintf("pgrep -f '%s/bin/dist-log-analyzer worker' && echo 'WORKER_RUNNING_OK'", opts.InstallDir)
	var outBuf bytes.Buffer
	mw := io.MultiWriter(logWriter, &outBuf)
	_ = runRemoteCmd(client, checkCmd, mw)

	if strings.Contains(outBuf.String(), "WORKER_RUNNING_OK") {
		fmt.Fprintf(logWriter, "[SSH Deploy] 业务节点安装并启动成功！已成功接入集群并在指定硬盘上提供存储服务。\n")
		return nil
	}

	fmt.Fprintf(logWriter, "[SSH Deploy] 提示: 业务进程已发送启动指令，请在节点列表查看在线状态\n")
	return nil
}

func runRemoteCmd(client *ssh.Client, cmd string, logWriter io.Writer) error {
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	session.Stdout = logWriter
	session.Stderr = logWriter
	return session.Run(cmd)
}

func copyBytesToRemote(client *ssh.Client, data []byte, remotePath string, mode os.FileMode, logWriter io.Writer) error {
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	go func() {
		w, _ := session.StdinPipe()
		defer w.Close()
		fmt.Fprintf(w, "C%04o %d %s\n", mode, len(data), "dist-log-analyzer")
		_, _ = w.Write(data)
		fmt.Fprint(w, "\x00")
	}()

	cmd := fmt.Sprintf("scp -t %s", remotePath)
	return session.Run(cmd)
}

// GetOutboundIP 获取本机对外 IP
func GetOutboundIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String()
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
