package krunlet

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"
)

// Stats describes an individual VM command. PeakMemoryMiB is the *host
// helper's* max resident set size from wait4/Getrusage, NOT a measure of
// guest allocated RAM. ReadyMillis is -1 when the one-shot helper has no
// explicit guest-ready frame. No Prometheus dependency is required.
type Stats struct {
	RunID          string
	ReadyMillis    int64
	DurationMillis int64
	ExitCode       int
	PeakMemoryMiB  float64
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
