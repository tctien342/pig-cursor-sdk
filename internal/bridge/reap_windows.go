//go:build windows

package bridge

import "os"

// terminatePID has no polite form to send here, so the watchdog kill below is the
// only step.
func terminatePID(proc *os.Process) {}
