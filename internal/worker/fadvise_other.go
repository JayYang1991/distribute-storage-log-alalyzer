//go:build !linux
// +build !linux

package worker

import "os"

func adviseSequential(f *os.File) {}
