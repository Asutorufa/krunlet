//go:build darwin

package krunlet

import (
	"os"
	"syscall"
)

// macOS ru_maxrss is bytes.
func formatPeakRSS(usage *syscall.Rusage) float64 { return float64(usage.Maxrss) / (1024 * 1024) }

func helperPeakMemory(state *os.ProcessState) float64 {
	if state == nil {
		return 0
	}
	if usage, ok := state.SysUsage().(*syscall.Rusage); ok {
		return formatPeakRSS(usage)
	}
	return 0
}
