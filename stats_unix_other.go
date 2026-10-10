//go:build !linux && !darwin

package krunlet

import "os"

func helperPeakMemory(_ *os.ProcessState) float64 { return 0 }
