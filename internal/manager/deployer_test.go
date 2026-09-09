package manager

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func getTestSSHPrivateKey() string {
	if key := os.Getenv("TEST_SSH_KEY"); key != "" {
		return key
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		if data, err := os.ReadFile(filepath.Join(home, ".ssh", "id_ed25519")); err == nil {
			return string(data)
		}
		if data, err := os.ReadFile(filepath.Join(home, ".ssh", "id_rsa")); err == nil {
			return string(data)
		}
	}
	return ""
}

func TestCheckBlockDeviceSafety(t *testing.T) {
	// 1. 测试真实生产常见的系统盘 (包含 boot 和 / 分区)
	sysDev := rawBlockDevice{
		Name:  "nvme0n1",
		Size:  1024209543168,
		Type:  "disk",
		Model: "WD PC SN740",
		Children: []rawBlockDevice{
			{
				Name:       "nvme0n1p1",
				Size:       1127219200,
				Type:       "part",
				MountPoint: "/boot/efi",
				FSType:     "vfat",
			},
			{
				Name:       "nvme0n1p2",
				Size:       1023079874560,
				Type:       "part",
				MountPoint: "/",
				FSType:     "ext4",
			},
		},
	}

	hasFS, isSystem, parts := checkBlockDeviceSafety(sysDev)
	if !hasFS {
		t.Fatalf("系统盘应该被检测为含有文件系统/分区 (hasFS=true)")
	}
	if !isSystem {
		t.Fatalf("系统盘应该被判定为系统关键盘 (isSystem=true)")
	}
	if len(parts) < 2 {
		t.Fatalf("应列出所有包含文件系统/挂载点的子分区，实际: %v", parts)
	}

	// 2. 测试已有 ext4 文件系统的数据盘 (无子分区，但盘本身已格式化)
	dataDevWithFS := rawBlockDevice{
		Name:       "sdb",
		Size:       536870912000,
		Type:       "disk",
		MountPoint: "/data/old",
		FSType:     "ext4",
		Model:      "SAMSUNG SSD",
	}
	hasFS, isSystem, _ = checkBlockDeviceSafety(dataDevWithFS)
	if !hasFS {
		t.Fatalf("已有 ext4 文件系统的盘应该判定 hasFS=true")
	}
	if isSystem {
		t.Fatalf("普通数据盘不应误判为系统盘")
	}

	// 3. 测试完全未格式化的纯净物理裸盘
	cleanDev := rawBlockDevice{
		Name:       "sdc",
		Size:       2147483648000,
		Type:       "disk",
		MountPoint: "",
		FSType:     "",
		Model:      "SEAGATE HDD",
		Children:   nil,
	}
	hasFS, isSystem, parts = checkBlockDeviceSafety(cleanDev)
	if hasFS || isSystem || len(parts) > 0 {
		t.Fatalf("纯净裸盘应该判定 hasFS=false, isSystem=false, 实际: hasFS=%v, isSystem=%v, parts=%v", hasFS, isSystem, parts)
	}

	// 4. 测试被分了区但尚未挂载的盘
	partitionedDev := rawBlockDevice{
		Name:  "sdd",
		Size:  500000000000,
		Type:  "disk",
		Model: "TOSHIBA",
		Children: []rawBlockDevice{
			{
				Name:   "sdd1",
				Size:   500000000000,
				Type:   "part",
				FSType: "xfs",
			},
		},
	}
	hasFS, _, _ = checkBlockDeviceSafety(partitionedDev)
	if !hasFS {
		t.Fatalf("已有分区/xfs 的盘必须被判定 hasFS=true 防呆锁定")
	}
}

func TestIsSystemMount(t *testing.T) {
	testCases := []struct {
		mount    string
		expected bool
	}{
		{"/", true},
		{"/boot", true},
		{"/boot/efi", true},
		{"/home", true},
		{"/usr", true},
		{"/var", true},
		{"[SWAP]", true},
		{"/data", false},
		{"/mnt/storage", false},
		{"", false},
	}

	for _, tc := range testCases {
		res := isSystemMount(tc.mount)
		if res != tc.expected {
			t.Errorf("isSystemMount(%q) = %v, expected %v", tc.mount, res, tc.expected)
		}
	}
}

func TestGetDiskList(t *testing.T) {
	// 1. 测试空输入
	optsEmpty := SSHDeployOptions{}
	if disks := optsEmpty.GetDiskList(); len(disks) != 0 {
		t.Fatalf("空选项应当返回空列表，实际: %v", disks)
	}

	// 2. 测试仅设置 DiskDevice (逗号分隔)
	optsSingleString := SSHDeployOptions{
		DiskDevice: "/dev/sdb, /dev/sdc, /dev/sdb",
	}
	disks := optsSingleString.GetDiskList()
	if len(disks) != 2 || disks[0] != "/dev/sdb" || disks[1] != "/dev/sdc" {
		t.Fatalf("逗号分隔解析与去重异常，实际: %v", disks)
	}

	// 3. 测试设置 DiskDevices 切片
	optsSlice := SSHDeployOptions{
		DiskDevices: []string{"/dev/nvme1n1", " /dev/nvme2n1 ", "/dev/nvme1n1", ""},
	}
	disks = optsSlice.GetDiskList()
	if len(disks) != 2 || disks[0] != "/dev/nvme1n1" || disks[1] != "/dev/nvme2n1" {
		t.Fatalf("切片去重修剪异常，实际: %v", disks)
	}

	// 4. 测试 DiskDevices 与 DiskDevice 混用
	optsMixed := SSHDeployOptions{
		DiskDevice:  "/dev/sdd",
		DiskDevices: []string{"/dev/sdb", "/dev/sdc"},
	}
	disks = optsMixed.GetDiskList()
	if len(disks) != 3 {
		t.Fatalf("混用时应当合并去重，实际: %v", disks)
	}
}

func TestLsblkJsonUnmarshal(t *testing.T) {
	rawJSON := `{
   "blockdevices": [
      {
         "name": "loop0",
         "size": 1581056,
         "type": "loop",
         "mountpoint": "/snap/hwctl/123",
         "fstype": "squashfs",
         "model": null
      },{
         "name": "vda",
         "size": 26843545600,
         "type": "disk",
         "mountpoint": null,
         "fstype": null,
         "model": null,
         "children": [
            {
               "name": "vda1",
               "size": 1048576,
               "type": "part",
               "mountpoint": null,
               "fstype": null,
               "model": null
            },{
               "name": "vda2",
               "size": 2147483648,
               "type": "part",
               "mountpoint": "/boot",
               "fstype": "ext4",
               "model": null
            },{
               "name": "vda3",
               "size": 24692916224,
               "type": "part",
               "mountpoint": null,
               "fstype": "LVM2_member",
               "model": null,
               "children": [
                  {
                     "name": "ubuntu--vg-ubuntu--lv",
                     "size": 12343836672,
                     "type": "lvm",
                     "mountpoint": "/",
                     "fstype": "ext4",
                     "model": null
                  }
               ]
            }
         ]
      }
   ]
}`

	var res struct {
		BlockDevices []rawBlockDevice `json:"blockdevices"`
	}
	err := json.Unmarshal([]byte(rawJSON), &res)
	if err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	t.Logf("Successfully unmarshaled %d devices", len(res.BlockDevices))
	for _, b := range res.BlockDevices {
		hasFS, isSys, summary := checkBlockDeviceSafety(b)
		t.Logf("dev: %s, hasFS: %v, isSys: %v, summary: %v", b.Name, hasFS, isSys, summary)
	}
}

func TestDetectRemoteDisksLive(t *testing.T) {
	key := getTestSSHPrivateKey()
	if key == "" {
		t.Skip("跳过虚拟机真实 SSH 测试 (未提供 SSH 私钥)")
	}

	opts := SSHDeployOptions{
		Host:       "192.168.122.100",
		Port:       22,
		Username:   "jason",
		PrivateKey: key,
	}
	disks, err := DetectRemoteDisks(opts)
	if err != nil {
		t.Fatalf("DetectRemoteDisks failed: %v", err)
	}
	for _, d := range disks {
		t.Logf("Disk: %+v", d)
	}
}

func TestDeployWorkerBlockSystemDiskLive(t *testing.T) {
	key := getTestSSHPrivateKey()
	if key == "" {
		t.Skip("跳过虚拟机真实 SSH 测试 (未提供 SSH 私钥)")
	}

	opts := SSHDeployOptions{
		Host:        "192.168.122.100",
		Port:        22,
		Username:    "jason",
		PrivateKey:  key,
		DiskDevices: []string{"/dev/vda"},
	}
	var buf bytes.Buffer
	err := DeployWorkerViaSSH(opts, &buf)
	t.Logf("Deploy output:\n%s", buf.String())
	if err == nil {
		t.Fatalf("选择系统盘 /dev/vda 必须被严格拦截报错！")
	}
	t.Logf("拦截成功，错误信息: %v", err)
}

func TestDeployWorkerMultiDiskSuccessLive(t *testing.T) {
	key := getTestSSHPrivateKey()
	if key == "" {
		t.Skip("跳过虚拟机真实 SSH 测试 (未提供 SSH 私钥)")
	}

	opts := SSHDeployOptions{
		Host:         "192.168.122.100",
		Port:         22,
		Username:     "jason",
		PrivateKey:   key,
		NodeName:     fmt.Sprintf("auto-worker-%d", time.Now().Unix()%10000),
		WorkerPort:   8081,
		ManagerURL:   "http://192.168.122.100:8080",
		ClusterToken: "dist-log-cluster-secret-token",
		InstallDir:   "/opt/dist-log-worker-test",
		MountPoint:   "/data/dist-log-storage",
		DiskDevices:  []string{"/dev/vdb", "/dev/vdc"},
		FormatDisk:   false,
	}
	var buf bytes.Buffer
	err := DeployWorkerViaSSH(opts, &buf)
	t.Logf("Deploy output:\n%s", buf.String())
	if err != nil {
		t.Fatalf("多盘远程部署失败: %v", err)
	}

	client, err := createSSHClient(opts)
	if err != nil {
		t.Fatalf("SSH 连接失败: %v", err)
	}
	defer client.Close()

	// 1. 验证 Systemd 健康状态
	var activeBuf bytes.Buffer
	var activeState string
	for r := 0; r < 6; r++ {
		time.Sleep(500 * time.Millisecond)
		activeBuf.Reset()
		_ = runRemoteCmd(client, "sudo -n systemctl is-active dist-log-worker-vdb.service 2>/dev/null || true", &activeBuf)
		activeState = strings.TrimSpace(activeBuf.String())
		if activeState == "active" {
			break
		}
	}
	if activeState != "active" {
		t.Fatalf("预期 dist-log-worker-vdb.service 处于 active，实际: %s", activeState)
	}
	t.Logf("✔ Systemd 组件状态健康: dist-log-worker-vdb.service is active")

	// 2. 模拟突发故障/进程崩溃：直接强制杀掉该 worker 进程
	var pidBuf bytes.Buffer
	_ = runRemoteCmd(client, "sudo -n systemctl show --property MainPID --value dist-log-worker-vdb.service 2>/dev/null || true", &pidBuf)
	oldPID := strings.TrimSpace(pidBuf.String())
	if oldPID == "" || oldPID == "0" {
		pidBuf.Reset()
		_ = runRemoteCmd(client, "pgrep -f 'dist-log-analyzer worker --port=8081' | head -n 1", &pidBuf)
		oldPID = strings.TrimSpace(pidBuf.String())
	}
	t.Logf("模拟故障前 Worker MainPID: %s, 执行 kill -9 模拟意外崩溃...", oldPID)
	if oldPID != "" && oldPID != "0" {
		_ = runRemoteCmd(client, fmt.Sprintf("sudo -n kill -9 %s", oldPID), nil)
	}

	// 3. 等待 Systemd 触发 RestartSec (3s) 自动拉起
	time.Sleep(4500 * time.Millisecond)

	// 4. 再次验证 Systemd 自动拉起后的健康状态与新 PID
	activeBuf.Reset()
	cmdErr := runRemoteCmd(client, "sudo -n systemctl is-active dist-log-worker-vdb.service 2>/dev/null || true", &activeBuf)
	t.Logf("二次检查 activeBuf: %q, cmdErr: %v", activeBuf.String(), cmdErr)
	if strings.TrimSpace(activeBuf.String()) != "active" {
		t.Fatalf("故障后未成功自动拉起，当前状态: %q (err: %v)", activeBuf.String(), cmdErr)
	}

	pidBuf.Reset()
	pidErr := runRemoteCmd(client, "sudo -n systemctl show --property MainPID --value dist-log-worker-vdb.service 2>/dev/null || true", &pidBuf)
	newPID := strings.TrimSpace(pidBuf.String())
	t.Logf("newPID show 输出: %q, pidErr: %v", newPID, pidErr)
	if newPID == "" || newPID == "0" {
		pidBuf.Reset()
		_ = runRemoteCmd(client, "pgrep -f 'dist-log-analyzer worker --port=8081' | head -n 1", &pidBuf)
		newPID = strings.TrimSpace(pidBuf.String())
		t.Logf("newPID pgrep 输出: %q", newPID)
	}
	t.Logf("✔ 故障后自动拉起成功！新进程 MainPID: %s (原 MainPID: %s)", newPID, oldPID)
	if newPID == "" || newPID == "0" || newPID == oldPID {
		t.Fatalf("故障自愈异常，未能分配有效新 PID (新: %s, 旧: %s)", newPID, oldPID)
	}

	// 5. 清理测试服务
	_ = runRemoteCmd(client, "sudo -n systemctl stop dist-log-worker-vdb dist-log-worker-vdc 2>/dev/null || true", nil)
	_ = runRemoteCmd(client, "sudo -n rm -f /etc/systemd/system/dist-log-worker-*.service && sudo -n systemctl daemon-reload", nil)
	_ = runRemoteCmd(client, "sudo -n rm -rf /opt/dist-log-worker-test", nil)
}






