package krunlet

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"
)

// Stats describes a VM command. HostHelperPeakRSSMiB is the host helper's
// high-water resident set size, NEVER guest allocated memory. ReadyMillis
// measures helper startup to VM-ready; -1 means no ready signal exists
// (currently one-shot runs). DurationMillis measures the entire command
// including admission, cloning, and cleanup. No Prometheus dependency.
type Stats struct {
	RunID                string
	ReadyMillis          int64
	DurationMillis       int64
	ExitCode             int
	HostHelperPeakRSSMiB float64
	NetworkPolicy        bool
	TimedOut             bool
}

func newRunID() string {
	var bytes [12]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return hex.EncodeToString(bytes[:])
	}
	return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
}
