package krunlet

// HelperParentWatch is intended only for the standalone CLI's private helper
// entry point. Parent liveness is monitored by pidfd (Linux) or kqueue
// EVFILT_PROC (macOS); it terminates the helper process group when orphaned.
func HelperParentWatch() error {
	if err := waitCgroupStartupGate(); err != nil {
		return err
	}
	return watchHelperParent()
}
