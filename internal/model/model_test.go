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
