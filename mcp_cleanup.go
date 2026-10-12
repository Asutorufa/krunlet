package krunlet

import (
	"os"
)

// ReclaimMCPOrphans cleans up marked, unlocked temporary roots left by
// forcibly terminated MCP servers before accepting new agent requests.
// It deliberately does not touch reusable templates or active sessions.
func ReclaimMCPOrphans(cgroupParent string) error {
	if err := reclaimMCPVMGroups(cgroupParent); err != nil {
		return err
	}
	return cleanupStaleRootsMatching(os.TempDir(), 0,
		[]string{"krunlet-root-", "krunlet-state-", "krunlet-net-"})
}
