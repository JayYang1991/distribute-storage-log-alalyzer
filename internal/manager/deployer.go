package manager

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
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
	DiskDevice  string   `json:"disk_device"`  // 单盘兼容字符串 (支持逗号分隔多盘)
	DiskDevices []string `json:"disk_devices"` // 多盘列表
	FSType      string   `json:"fs_type"`      // ext4 或 xfs
	MountPoint  string   `json:"mount_point"`  // 挂载点根目录，默认 /data/dist-log-storage
	FormatDisk  bool     `json:"format_disk"`  // 是否执行格式化
}

// GetDiskList 解析并标准化多盘设备列表 (自动去重并剔除空项)
func (opts SSHDeployOptions) GetDiskList() []string {
	var list []string
	seen := make(map[string]bool)

	addDev := func(d string) {
		cd := strings.TrimSpace(d)
		if cd != "" && !seen[cd] {
			seen[cd] = true
			list = append(list, cd)
		}
	}

	for _, d := range opts.DiskDevices {
		addDev(d)
	}
	if opts.DiskDevice != "" {
		for _, d := range strings.Split(opts.DiskDevice, ",") {
			addDev(d)
		}
	}
	return list
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
func checkBlockDeviceSafety(dev rawBlockDevice, sysMaps ...map[string]bool) (hasFS bool, isSystem bool, partsSummary []string) {
	var sysMap map[string]bool
	if len(sysMaps) > 0 {
		sysMap = sysMaps[0]
	}
	mountPoint := strings.TrimSpace(dev.MountPoint)
	fsType := strings.TrimSpace(dev.FSType)

	if isSystemMount(mountPoint) || isSystemDeviceName(dev.Name, sysMap) {
		isSystem = true
		hasFS = true
	}
	if fsType != "" || mountPoint != "" {
		hasFS = true
		partsSummary = append(partsSummary, fmt.Sprintf("%s(fs:%s, mount:%s)", dev.Name, fsType, mountPoint))
	}

	// 递归检查所有子分区
	for _, child := range dev.Children {
		cHasFS, cIsSystem, cSummary := checkBlockDeviceSafety(child, sysMap)
		if cIsSystem {
			isSystem = true
			hasFS = true
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

func isSystemDeviceName(name string, sysMap map[string]bool) bool {
	if len(sysMap) == 0 {
		return false
	}
	clean := strings.TrimPrefix(strings.TrimSpace(name), "/dev/")
	clean = strings.TrimPrefix(clean, "mapper/")
	if sysMap[clean] {
		return true
	}
	for sysName := range sysMap {
		if sysName == clean {
			return true
		}
		// 例如 clean 为 "vda"，sysName 为 "vda1"、"vda2"、"vda3"
		if strings.HasPrefix(sysName, clean) {
			return true
		}
		// 例如 clean 为 "vda1"，sysName 为 "vda"
		if strings.HasPrefix(clean, sysName) {
			return true
		}
	}
	return false
}

// DetectRemoteDisks 通过 SSH 远程检测目标主机的物理磁盘与分区列表（执行防呆过滤与状态分析）
func DetectRemoteDisks(opts SSHDeployOptions) ([]model.DiskInfo, error) {
	client, err := createSSHClient(opts)
	if err != nil {
		return nil, fmt.Errorf("SSH 连接失败: %w", err)
	}
	defer client.Close()

	log.Printf("[DetectRemoteDisks] 连接目标 %s:%d 成功，开始探测...", opts.Host, opts.Port)

	// 提前探测系统关键根分区及引导分区所驻留的底层物理磁盘名称列表 (内核级反查)
	systemDisksMap := make(map[string]bool)
	var sysDevBuf bytes.Buffer
	cmdFindSys := `for m in / /boot /boot/efi /usr /var /home; do src=$(findmnt -n -o SOURCE "$m" 2>/dev/null || df "$m" 2>/dev/null | tail -1 | awk '{print $1}'); if [ -n "$src" ]; then lsblk -slno NAME "$src" 2>/dev/null || echo "$src"; fi; done | sort -u`
	if errSys := runRemoteCmd(client, cmdFindSys, &sysDevBuf); errSys == nil {
		for _, devLine := range strings.Split(sysDevBuf.String(), "\n") {
			devName := strings.TrimSpace(devLine)
			devName = strings.TrimPrefix(devName, "/dev/")
			devName = strings.TrimPrefix(devName, "mapper/")
			if devName != "" {
				systemDisksMap[devName] = true
			}
		}
	}
	log.Printf("[DetectRemoteDisks] 探测到系统关键磁盘/分区集合: %+v", systemDisksMap)

	// 优先执行 lsblk JSON 格式化输出 (包括 children)
	cmdJSON := "lsblk -b -J -o NAME,SIZE,TYPE,MOUNTPOINT,FSTYPE,MODEL"
	var outBuf bytes.Buffer
	var errBuf bytes.Buffer
	err = runRemoteCmdSeparate(client, cmdJSON, &outBuf, &errBuf)
	log.Printf("[DetectRemoteDisks] cmdJSON run err: %v, outBuf len: %d, errBuf len: %d", err, outBuf.Len(), errBuf.Len())
	if err == nil && outBuf.Len() > 0 {
		var res struct {
			BlockDevices []rawBlockDevice `json:"blockdevices"`
		}
		jsonErr := json.Unmarshal(outBuf.Bytes(), &res)
		log.Printf("[DetectRemoteDisks] jsonErr: %v, blockdevices count: %d", jsonErr, len(res.BlockDevices))
		if jsonErr == nil && len(res.BlockDevices) > 0 {
			disks := make([]model.DiskInfo, 0)
			for _, b := range res.BlockDevices {
				// 过滤非磁盘设备 (如 loop, rom, zram 等) 以及大小为 0 的读卡器设备
				bType := strings.ToLower(strings.TrimSpace(b.Type))
				if bType == "loop" || bType == "rom" || b.Size <= 0 {
					continue
				}

				hasFS, isSystem, partsSummary := checkBlockDeviceSafety(b, systemDisksMap)
				if isSystemDeviceName(b.Name, systemDisksMap) {
					isSystem = true
					hasFS = true
				}
				canFormat := !hasFS && !isSystem

				var statusText string
				if isSystem {
					canFormat = false
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

			log.Printf("[DetectRemoteDisks] JSON 模式探测完成，返回磁盘数: %d", len(disks))
			return disks, nil
		}
	}

	// 降级使用普通文本表格解析
	outBuf.Reset()
	cmdFallback := "lsblk -d -n -o NAME,SIZE,TYPE,MOUNTPOINT,FSTYPE 2>/dev/null"
	_ = runRemoteCmd(client, cmdFallback, &outBuf)
	log.Printf("[DetectRemoteDisks] 降级走 fallback 表格解析, outBuf len=%d", outBuf.Len())
	lines := strings.Split(outBuf.String(), "\n")
	disks := make([]model.DiskInfo, 0)
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

			// 双重防呆：通过 lsblk 查询该设备及其所有子分区的挂载点
			var partBuf bytes.Buffer
			_ = runRemoteCmd(client, fmt.Sprintf("lsblk -n -o MOUNTPOINT,FSTYPE /dev/%s 2>/dev/null", d.Name), &partBuf)
			partOut := partBuf.String()
			for _, pLine := range strings.Split(partOut, "\n") {
				pF := strings.Fields(pLine)
				if len(pF) > 0 {
					for _, item := range pF {
						if isSystemMount(item) {
							d.IsSystem = true
						}
						if item != "" {
							d.HasFS = true
						}
					}
				}
			}

			if isSystemMount(d.MountPoint) || isSystemDeviceName(d.Name, systemDisksMap) {
				d.IsSystem = true
				d.HasFS = true
			}
			if d.FSType != "" || d.MountPoint != "" {
				d.HasFS = true
			}
			d.CanFormat = !d.HasFS && !d.IsSystem
			if d.IsSystem {
				d.CanFormat = false
				d.StatusText = "🚫 系统关键盘 (含系统启动/根分区，严禁格式化)"
			} else if d.HasFS {
				d.StatusText = "🚫 已有文件系统/分区，防呆锁定"
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

	log.Printf("[DetectRemoteDisks] Fallback 模式探测完成，返回磁盘数: %d", len(disks))
	return disks, nil
}

// DeployWorkerViaSSH 通过纯 Go SSH 远程登录并在目标机器上一键部署 Worker 业务组件（包含磁盘格式化挂载与多盘多进程）
func DeployWorkerViaSSH(opts SSHDeployOptions, logWriter io.Writer) error {
	disks := opts.GetDiskList()
	if len(disks) == 0 {
		errMsg := "【安全策略限制】严禁使用系统盘存放日志！增加 Worker 节点时必须至少指定一块独立的物理存储盘。"
		fmt.Fprintln(logWriter, errMsg)
		return fmt.Errorf("%s", errMsg)
	}

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

	fmt.Fprintf(logWriter, "[SSH Deploy] SSH 认证成功！目标选定 %d 块物理存储盘进行多进程隔离部署...\n", len(disks))

	// 探测目标主机的系统盘/分区集合 (如 /、/boot、/home 等)
	systemDisksMap := make(map[string]bool)
	var sysDevBuf bytes.Buffer
	cmdFindSys := `for m in / /boot /boot/efi /usr /var /home; do src=$(findmnt -n -o SOURCE "$m" 2>/dev/null || df "$m" 2>/dev/null | tail -1 | awk '{print $1}'); if [ -n "$src" ]; then lsblk -slno NAME "$src" 2>/dev/null || echo "$src"; fi; done | sort -u`
	if errSys := runRemoteCmd(client, cmdFindSys, &sysDevBuf); errSys == nil {
		for _, devLine := range strings.Split(sysDevBuf.String(), "\n") {
			devName := strings.TrimSpace(devLine)
			devName = strings.TrimPrefix(devName, "/dev/")
			devName = strings.TrimPrefix(devName, "mapper/")
			if devName != "" {
				systemDisksMap[devName] = true
			}
		}
	}

	// 1. 逐一执行物理磁盘安全防呆检测
	for _, diskDev := range disks {
		fmt.Fprintf(logWriter, "[SSH Deploy] 正在执行磁盘安全防呆检测 (目标: %s)...\n", diskDev)
		// 1.0 防呆校验 0：检查是否为系统盘/启动盘/根分区
		diskClean := strings.TrimPrefix(strings.TrimSpace(diskDev), "/dev/")
		if isSystemDeviceName(diskClean, systemDisksMap) {
			errMsg := fmt.Sprintf("【安全防呆拦截】目标设备 %s 关联系统关键分区或系统启动盘，严禁用于日志存储！已中止部署以防系统损毁", diskDev)
			fmt.Fprintln(logWriter, errMsg)
			return fmt.Errorf("%s", errMsg)
		}

		// 1.1 防呆校验 1：检查是否包含系统启动/根挂载点以及关键路径
		var lsblkBuf bytes.Buffer
		_ = runRemoteCmd(client, fmt.Sprintf("lsblk -n -o NAME,MOUNTPOINT,FSTYPE %s 2>/dev/null", diskDev), &lsblkBuf)
		for _, line := range strings.Split(lsblkBuf.String(), "\n") {
			fields := strings.Fields(line)
			for _, f := range fields {
				if isSystemMount(f) {
					errMsg := fmt.Sprintf("【安全防呆拦截】目标设备 %s 关联系统关键分区 (%s)，严禁用于日志存储！已中止部署以防系统损毁", diskDev, f)
					fmt.Fprintln(logWriter, errMsg)
					return fmt.Errorf("%s", errMsg)
				}
			}
		}

		// 1.2 防呆校验 2：如果勾选格式化，检查是否包含未明确清空的文件系统签名
		if opts.FormatDisk {
			var wipefsBuf bytes.Buffer
			_ = runRemoteCmd(client, fmt.Sprintf("wipefs -n %s 2>/dev/null", diskDev), &wipefsBuf)
			wipefsOutput := strings.TrimSpace(wipefsBuf.String())
			if len(wipefsOutput) > 0 {
				errMsg := fmt.Sprintf("【安全防呆拦截】目标磁盘 %s 上检测到已有文件系统签名:\n%s\n系统防呆机制已阻止格式化以防误删重要数据！请选择未格式化的纯净裸盘", diskDev, wipefsOutput)
				fmt.Fprintln(logWriter, errMsg)
				return fmt.Errorf("%s", errMsg)
			}
		}
	}

	sudoPrefix := ""
	if opts.Username != "root" {
		sudoPrefix = "sudo -n "
	}

	// 2. 创建安装主目录
	cmdCreateDir := fmt.Sprintf("%smkdir -p %s/bin %s/logs %s/run && %schown -R %s %s 2>/dev/null || mkdir -p %s/bin %s/logs %s/run",
		sudoPrefix, opts.InstallDir, opts.InstallDir, opts.InstallDir, sudoPrefix, opts.Username, opts.InstallDir,
		opts.InstallDir, opts.InstallDir, opts.InstallDir)
	if err := runRemoteCmd(client, cmdCreateDir, logWriter); err != nil {
		return fmt.Errorf("创建远程目录失败: %w", err)
	}

	// 3. 检查本地二进制路径并拷贝至远程
	currentExec, err := os.Executable()
	if err != nil {
		currentExec = "/usr/local/bin/dist-log-analyzer"
	}
	// 若当前运行在 go test 环境中，优先使用工程根目录的正式二进制
	if strings.Contains(currentExec, ".test") {
		candidates := []string{"bin/dist-log-analyzer", "../bin/dist-log-analyzer", "../../bin/dist-log-analyzer"}
		for _, c := range candidates {
			if _, sErr := os.Stat(c); sErr == nil {
				currentExec = c
				break
			}
		}
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

	// 4. 停止所有旧的 worker 进程与已注册的 systemd 服务 (防止自动拉起竞争)
	stopCmd := fmt.Sprintf("%ssystemctl stop 'dist-log-worker-*.service' 2>/dev/null || true; %spkill -9 -f '%s/bin/dist-log-analyzer worker' 2>/dev/null || true; %schown -R %s %s 2>/dev/null || true; chmod +x %s/bin/dist-log-analyzer 2>/dev/null || true",
		sudoPrefix, sudoPrefix, opts.InstallDir, sudoPrefix, opts.Username, opts.InstallDir, opts.InstallDir)
	_ = runRemoteCmd(client, stopCmd, logWriter)

	// 检测目标机器是否支持 Systemd 守护进程
	var hasSystemd bool
	var sysCheckBuf bytes.Buffer
	checkErr := runRemoteCmd(client, "command -v systemctl || which systemctl", &sysCheckBuf)
	if strings.Contains(sysCheckBuf.String(), "systemctl") || checkErr == nil {
		hasSystemd = true
		fmt.Fprintln(logWriter, "[SSH Deploy] 检测到目标节点支持 Systemd，将为每个磁盘 Worker 注册系统守护服务并开启故障 3 秒自动拉起 (Restart=always)")
	}

	// 5. 为每块选定的物理硬盘格式化挂载，并分别启动一个专属 Worker 进程 (多进程隔离架构)
	for idx, diskDev := range disks {
		diskName := filepath.Base(diskDev)
		instPort := opts.WorkerPort + idx
		instName := fmt.Sprintf("%s-%s", opts.NodeName, diskName)
		instMount := fmt.Sprintf("%s/disk-%s", opts.MountPoint, diskName)
		instDataDir := fmt.Sprintf("%s/data", instMount)

		fmt.Fprintf(logWriter, "[SSH Deploy] [%d/%d] 正在处理物理磁盘: %s (服务端口: %d, 挂载点: %s)...\n",
			idx+1, len(disks), diskDev, instPort, instMount)

		if opts.FormatDisk {
			_ = runRemoteCmd(client, fmt.Sprintf("%sumount %s 2>/dev/null || true", sudoPrefix, diskDev), logWriter)
			var formatCmd string
			if strings.ToLower(opts.FSType) == "xfs" {
				formatCmd = fmt.Sprintf("%smkfs.xfs -f %s", sudoPrefix, diskDev)
			} else {
				formatCmd = fmt.Sprintf("%smkfs.ext4 -F %s", sudoPrefix, diskDev)
			}
			if err := runRemoteCmd(client, formatCmd, logWriter); err != nil {
				return fmt.Errorf("格式化硬盘 %s 失败: %w", diskDev, err)
			}
		}

		mountCmd := fmt.Sprintf("%smkdir -p %s && (%smount | grep -q 'on %s ' || %smount %s %s) && %schown -R %s %s 2>/dev/null || true",
			sudoPrefix, instMount, sudoPrefix, instMount, sudoPrefix, diskDev, instMount, sudoPrefix, opts.Username, instMount)
		if err := runRemoteCmd(client, mountCmd, logWriter); err != nil {
			return fmt.Errorf("挂载硬盘 %s 失败: %w", diskDev, err)
		}

		fstabCmd := fmt.Sprintf("%sbash -c \"grep -v '%s' /etc/fstab > /tmp/fstab.tmp && mv -f /tmp/fstab.tmp /etc/fstab && echo '%s %s %s defaults 0 0' >> /etc/fstab\" 2>/dev/null || true",
			sudoPrefix, diskDev, diskDev, instMount, opts.FSType)
		_ = runRemoteCmd(client, fstabCmd, logWriter)

		// 创建该盘的数据目录并赋予当前用户权限
		_ = runRemoteCmd(client, fmt.Sprintf("%smkdir -p %s && %schown -R %s %s 2>/dev/null || true", sudoPrefix, instDataDir, sudoPrefix, opts.Username, instDataDir), logWriter)

		// 优先配置并启动 Systemd 守护服务（配置故障 3 秒自动拉起，保障高可用与健康管理）
		svcFile := fmt.Sprintf("dist-log-worker-%s.service", diskName)
		svcPath := fmt.Sprintf("/etc/systemd/system/%s", svcFile)
		workerExec := fmt.Sprintf("%s/bin/dist-log-analyzer worker --port=%d --manager-url=%s --cluster-token=%s --node-name=%s --advertise-ip=%s --data-dir=%s",
			opts.InstallDir, instPort, opts.ManagerURL, opts.ClusterToken, instName, opts.Host, instDataDir)

		svcContent := fmt.Sprintf(`[Unit]
Description=Distributed Storage Log Analyzer Worker (Disk: %s, Port: %d)
After=network.target
StartLimitIntervalSec=60s
StartLimitBurst=10

[Service]
Type=simple
User=root
WorkingDirectory=%s
ExecStart=%s
Restart=always
RestartSec=3s
LimitNOFILE=65536
TimeoutStopSec=15s
KillMode=mixed

[Install]
WantedBy=multi-user.target
`, diskDev, instPort, opts.InstallDir, workerExec)

		if hasSystemd {
			tmpSvcPath := fmt.Sprintf("/tmp/%s", svcFile)
			if err := copyBytesToRemote(client, []byte(svcContent), tmpSvcPath, 0644, logWriter); err != nil {
				return fmt.Errorf("传输 Systemd 服务配置失败: %w", err)
			}

			startSvcCmd := fmt.Sprintf("%smv -f %s %s && %schown root:root %s && %schmod 644 %s && %ssystemctl daemon-reload && %ssystemctl enable %s 2>/dev/null || true && %ssystemctl restart %s",
				sudoPrefix, tmpSvcPath, svcPath, sudoPrefix, svcPath, sudoPrefix, svcPath, sudoPrefix, sudoPrefix, svcFile, sudoPrefix, svcFile)
			if err := runRemoteCmd(client, startSvcCmd, logWriter); err != nil {
				return fmt.Errorf("启动 Systemd 服务 (%s) 失败: %w", svcFile, err)
			}

			// 循环验证服务健康状态 (最多等待 4 秒，确保 systemd 从 activating 过渡到 active)
			actState := ""
			for retries := 0; retries < 8; retries++ {
				time.Sleep(500 * time.Millisecond)
				var activeBuf bytes.Buffer
				_ = runRemoteCmd(client, fmt.Sprintf("%ssystemctl is-active %s 2>/dev/null || true", sudoPrefix, svcFile), &activeBuf)
				actState = strings.TrimSpace(activeBuf.String())
				if actState == "active" {
					break
				}
			}
			if actState == "active" {
				fmt.Fprintf(logWriter, "[SSH Deploy] ✔ Worker Systemd 服务 [%s] 健康检查通过 (active)！已配置故障 3 秒自动拉起 (Restart=always)\n", svcFile)
			} else {
				return fmt.Errorf("Worker Systemd 服务 [%s] 启动健康检查失败 (状态: %s)", svcFile, actState)
			}
		} else {
			// 降级：后台守护进程
			startCmd := fmt.Sprintf("nohup %s </dev/null > %s/logs/worker-%s.log 2>&1 &",
				workerExec, opts.InstallDir, diskName)
			if err := runRemoteCmd(client, startCmd, logWriter); err != nil {
				return fmt.Errorf("启动 Worker 进程 (磁盘: %s) 失败: %w", diskDev, err)
			}
			fmt.Fprintf(logWriter, "[SSH Deploy] ✔ Worker 实例 [%s] 启动成功 (无 Systemd 环境，已通过 nohup 托管)\n", instName)
		}
	}

	time.Sleep(1500 * time.Millisecond)
	fmt.Fprintf(logWriter, "[SSH Deploy] 全部 %d 个物理磁盘的 Worker 实例已成功拉起并接入集群，多进程 I/O 隔离与容量调度生效！\n", len(disks))
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
	fullCmd := fmt.Sprintf("export PATH=$PATH:/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin; %s", cmd)
	return session.Run(fullCmd)
}

func runRemoteCmdSeparate(client *ssh.Client, cmd string, stdout io.Writer, stderr io.Writer) error {
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	session.Stdout = stdout
	session.Stderr = stderr
	fullCmd := fmt.Sprintf("export PATH=$PATH:/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin; %s", cmd)
	return session.Run(fullCmd)
}

func copyBytesToRemote(client *ssh.Client, data []byte, remotePath string, mode os.FileMode, logWriter io.Writer) error {
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	session.Stdout = logWriter
	session.Stderr = logWriter

	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}

	cmd := fmt.Sprintf("export PATH=$PATH:/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin; rm -f %s && cat > %s && chmod %04o %s", remotePath, remotePath, mode, remotePath)
	if err := session.Start(cmd); err != nil {
		return err
	}

	if _, err := stdin.Write(data); err != nil {
		return err
	}
	if err := stdin.Close(); err != nil {
		return err
	}

	return session.Wait()
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
