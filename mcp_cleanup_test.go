package krunlet

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReclaimMCPOrphansKeepsActiveAndUnrelatedDirectories(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	base := os.TempDir()
	active := filepath.Join(base, "krunlet-root-active")
	stale := filepath.Join(base, "krunlet-root-stale")
	unrelated := filepath.Join(base, "krunlet-session-keep")
	for _, dir := range []string{active, stale, unrelated} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	activeMarker, err := markTempRoot(active)
	if err != nil {
		t.Fatal(err)
	}
	defer activeMarker.Close()
	staleMarker, err := markTempRoot(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := staleMarker.Close(); err != nil {
		t.Fatal(err)
	}
	sessionMarker, err := markTempRoot(unrelated)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessionMarker.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ReclaimMCPOrphans(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(active); err != nil {
		t.Fatalf("reclaimed a still-active root: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("reclaimed non-MCP session directory: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("failed to reclaim abandoned root immediately: %v", err)
	}
	if err := activeMarker.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ReclaimMCPOrphans(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(active); !os.IsNotExist(err) {
		t.Fatalf("failed to reclaim previously active directory: %v", err)
	}
}
