//go:build !linux && !darwin

package krunlet

import "os"

func helperPeakMemory(_ *os.ProcessState) float64 { return 0 }

func helperRSSBytes(state *os.ProcessState) uint64 {
	return uint64(helperPeakMemory(state) * 1024 * 1024)
}
