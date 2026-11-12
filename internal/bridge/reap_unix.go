//go:build !windows

package bridge

import (
	"os"
	"syscall"
)

// terminatePID asks a bridge to stop on its own terms first.
func terminatePID(proc *os.Process) { _ = proc.Signal(syscall.SIGTERM) }
