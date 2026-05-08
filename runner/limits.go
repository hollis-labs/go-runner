package runner

import (
	"os/exec"
	"time"
)

// ResourceLimits applies OS-level resource caps to the spawned process.
// All fields are zero-default-disabled. Per-platform support:
//
//   - Linux: when systemd-run --user is available, uses transient cgroup
//     v2 enforcement for MemoryMax + CPUQuota. Otherwise (and for the
//     remaining limits) wraps the spawn argv with `sh -c "ulimit ...; exec "$@"`
//     and the kernel enforces RLIMIT_*. RLIMIT_AS is advisory; the kernel
//     enforces it but apps that don't malloc-fail-gracefully may still
//     overshoot via mmap or stack growth.
//
//   - macOS: setrlimit only via the same `sh -c "ulimit ..."` wrap.
//     MemoryMax (RLIMIT_AS / RLIMIT_DATA) is advisory on macOS — the
//     kernel does not enforce it as strictly as Linux. Document this
//     in your wrapper if you depend on hard memory limits.
//
// Limits compose with sandbox.Apply: the rlimit-setting shell exec's
// into the sandbox helper which exec's into the real binary. Limits
// inherit through the chain.
type ResourceLimits struct {
	// CPUTime is the hard CPU-seconds limit (RLIMIT_CPU). The kernel
	// sends SIGXCPU at the soft limit and SIGKILL at the hard limit.
	// Linux: enforced via systemd-run CPUQuota when available, else
	// RLIMIT_CPU. macOS: RLIMIT_CPU.
	CPUTime time.Duration

	// MemoryMax is the maximum virtual memory in bytes. Linux: when
	// systemd-run is available, enforced as cgroup v2 memory.max
	// (real OOM-kill on overshoot). Otherwise / on macOS: RLIMIT_AS
	// (advisory; see godoc above).
	MemoryMax uint64

	// MaxOpenFiles is the maximum number of open file descriptors
	// (RLIMIT_NOFILE).
	MaxOpenFiles uint64

	// MaxProcesses is the maximum number of child processes
	// (RLIMIT_NPROC).
	MaxProcesses uint64

	// MaxFileSize is the maximum size of any single file the process
	// can create or extend, in bytes (RLIMIT_FSIZE).
	MaxFileSize uint64
}

// IsZero reports whether all fields are zero (no limits configured).
func (r ResourceLimits) IsZero() bool {
	return r.CPUTime == 0 &&
		r.MemoryMax == 0 &&
		r.MaxOpenFiles == 0 &&
		r.MaxProcesses == 0 &&
		r.MaxFileSize == 0
}

// applyResourceLimits wraps cmd.Path / cmd.Args with the appropriate
// argv prefix to enforce the configured limits. Returns a cleanup
// closure run after cmd.Wait. No-op (and returns a no-op cleanup) when
// the limits are zero.
//
// This is the cross-platform stub. The real per-platform implementation
// lives in limits_unix.go (build-tagged for darwin / linux) and is
// filled out in the ResourceLimits commit. The stub here keeps the
// Cap-2 commit (SupervisorOptions) self-contained.
func applyResourceLimits(cmd *exec.Cmd, limits ResourceLimits) (func(), error) {
	if limits.IsZero() {
		return func() {}, nil
	}
	return applyResourceLimitsImpl(cmd, limits)
}
