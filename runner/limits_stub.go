package runner

import "os/exec"

// applyResourceLimitsImpl is a no-op stub used by the SupervisorOptions
// commit. Replaced with real per-platform shell-wrap / systemd-run
// logic in the ResourceLimits commit.
//
// Returning the limits with zero application is incorrect for the
// real implementation, but this stub is only ever compiled when the
// real impl has not yet landed; the Cap-3 commit replaces this file.
func applyResourceLimitsImpl(cmd *exec.Cmd, limits ResourceLimits) (func(), error) {
	_ = cmd
	_ = limits
	return func() {}, nil
}
