package model

import "testing"

func TestIsInternalIndexFile(t *testing.T) {
	cases := []struct {
		path     string
		expected bool
	}{
		{"syslog.log", false},
		{"var/log/audit.log", false},
		{"var/log/audit.txt", false},
		{"var/log/kernel.trace", false},
		{"syslog.log.lidx", true},
		{"syslog.log.bidx", true},
		{"var/log/kernel.trace.lidx", true},
		{"var/log/kernel.trace.bidx", true},
		{"var/log/kernel.trace.lidx.tmp", true},
		{"var/log/kernel.trace.bidx.tmp", true},
		{"SYSLOG.LOG.LIDX", true},
		{"SYSLOG.LOG.BIDX", true},
		{"", false},
		{"lidx", false},
		{"bidx", false},
	}

	for _, c := range cases {
		got := IsInternalIndexFile(c.path)
		if got != c.expected {
			t.Errorf("IsInternalIndexFile(%q) = %v, expected %v", c.path, got, c.expected)
		}
	}
}

func TestIsSafeSubpath(t *testing.T) {
	cases := []struct {
		base     string
		target   string
		expected bool
	}{
		{"/data/logs", "/data/logs", true},
		{"/data/logs", "/data/logs/syslog.log", true},
		{"/data/logs", "/data/logs/sub/dir/app.log", true},
		// 恶意攻击场景 1: 前缀截断绕过 (/data/logs-evil)
		{"/data/logs", "/data/logs-evil", false},
		{"/data/logs", "/data/logs-evil/hack.sh", false},
		// 恶意攻击场景 2: 向上穿越
		{"/data/logs", "/data/logs/../../etc/passwd", false},
		{"/data/logs", "/etc/shadow", false},
		{"/data/logs", "/data", false},
		// 相对路径测试
		{"data/logs", "data/logs/sys.log", true},
		{"data/logs", "data/logs_fake/sys.log", false},
		{"data/logs", "data/logs/../logs/sys.log", true},
		{"data/logs", "data/logs/../../outside", false},
	}

	for _, c := range cases {
		got := IsSafeSubpath(c.base, c.target)
		if got != c.expected {
			t.Errorf("IsSafeSubpath(%q, %q) = %v, expected %v", c.base, c.target, got, c.expected)
		}
	}
}
