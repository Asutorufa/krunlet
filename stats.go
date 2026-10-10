package krunlet

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"
)

// Stats records run timing and host resource use. StartupReadyMillis is
// helper launch to guest-ready for persistent VMs; -1 means unavailable
// (one-shot execution currently has no independent ready event).
// RunElapsedMillis includes quota admission, staging, command execution,
// and cleanup. HelperPeakRSSMiB is the HOST helper ru_maxrss, not guest RAM.
type Stats struct {
	RunID          string
	StartupReadyMillis int64
	RunElapsedMillis int64
	ExitCode int
	HelperPeakRSSMiB float64
	// CgroupPeakMemoryMiB is memory.peak for helper and descendants on Linux.
	CgroupPeakMemoryMiB float64
	NetworkPolicy  bool
	TimedOut       bool
}

func newRunID() string {
	var bytes [12]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return hex.EncodeToString(bytes[:])
	}
	return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
}
