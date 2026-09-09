package manager

import (
	"testing"
)

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
