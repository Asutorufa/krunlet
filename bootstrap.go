package krunlet

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/Asutorufa/krunlet/internal/krunffi"
)

// The library is usable without a separately installed krunlet executable.
// libkrun exits the process in which it runs, so any Go application importing
// krunlet can re-exec itself as a dedicated helper. The special argument is
// internal, not a public CLI command. This runs before the application's main.
func init() {
	if len(os.Args) != 4 || os.Args[1] != "__krunlet_self_helper" || os.Args[2] != "--config" {
		return
	}
	if err := runSelfHelper(os.Args[3]); err != nil {
		fmt.Fprintln(os.Stderr, "krunlet helper:", err)
	}
	os.Exit(125) // successful krun_start_enter never returns
}

func runSelfHelper(configPath string) error {
	if err:=waitCgroupStartupGate();err!=nil{return err}
	if err := watchHelperParent(); err != nil {
		return err
	}
	file, err := os.Open(configPath)
	if err != nil {
		return err
	}
	defer file.Close()
	var cfg krunffi.Config
	if err := json.NewDecoder(file).Decode(&cfg); err != nil {
		return err
	}
	slog.Debug("krunlet helper enter", "run_id", cfg.RunID)
	err = krunffi.Enter(cfg)
	if err != nil && cfg.ErrorPath != "" {
		_ = os.WriteFile(cfg.ErrorPath, []byte(err.Error()), 0600)
	}
	return err
}
