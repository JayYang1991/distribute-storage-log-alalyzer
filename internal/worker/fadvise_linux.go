//go:build linux
// +build linux

package worker

import (
	"os"

	"golang.org/x/sys/unix"
)

func adviseSequential(f *os.File) {
	if f != nil {
		_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_SEQUENTIAL)
	}
}
