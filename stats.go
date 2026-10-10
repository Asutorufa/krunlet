package krunlet

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"runtime"
	"time"
)

// Stats describes a VM command. MemoryPeakBytes is the host cgroup's
// memory.peak (including VMM guest RAM and charged page cache), NOT guest
// internal memory. Nil means not measured. HelperRSSBytes is the host helper
// process's peak resident set size. DurationMillis covers the whole run.
type Stats struct {
	RunID             string  `json:"run_id"`
	ReadyMillis       int64   `json:"ready_millis"`
	DurationMillis    int64   `json:"duration_millis"`
	ExitCode          int     `json:"exit_code"`
	HelperRSSBytes    uint64  `json:"helper_rss_bytes"`
	MemoryPeakBytes   *uint64 `json:"memory_peak_bytes,omitempty"`
	Containment       string  `json:"containment"`
	TerminationReason string  `json:"termination_reason,omitempty"`
	NetworkPolicy     bool    `json:"network_policy"`
	TimedOut          bool    `json:"timed_out"`
}

func newRunID() string {
	var bytes [12]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return hex.EncodeToString(bytes[:])
	}
	return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
}

func defaultContainment() string {
	switch runtime.GOOS {
	case "linux", "darwin":
		return "process_group"
	default:
		return "none"
	}
}
