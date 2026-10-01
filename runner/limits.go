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

	// MemoryMax is the maximum memory in bytes. Linux: when
	// systemd-run --user can start a scope, enforced as cgroup v2
	// memory.max with memory.swap.max=0, so swap cannot extend it (real
	// OOM-kill on overshoot). Otherwise: RLIMIT_AS (advisory; see godoc
	// above). macOS: dropped.
	MemoryMax uint64

	// MaxOpenFiles is the maximum number of open file descriptors
	// (RLIMIT_NOFILE).
	MaxOpenFiles uint64

	// MaxProcesses is the maximum number of child processes
	// (RLIMIT_NPROC).
	MaxProcesses uint64

	// MaxFileSize is the maximum size of any single file the process
	// can create or extend, in bytes (RLIMIT_FSIZE), rounded down to the
	// shell's ulimit -f block (512 bytes on Linux's sh, 1024 on macOS's).
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

// ApplyResourceLimits wraps cmd.Path / cmd.Args with the argv prefix
// needed to enforce limits: `sh -c "ulimit ...; exec \"$@\""` for kernel
// rlimits, layered under `systemd-run --user --scope --property=MemoryMax=...`
// on Linux when available for MemoryMax. It returns a cleanup closure (safe
// to call after cmd.Wait; currently a no-op) and a nil error. When
// limits.IsZero() the cmd is left untouched and a no-op cleanup is returned.
// On Windows a non-zero limits value returns an error.
//
// This is exactly what Run applies internally before every spawn and every
// supervised restart. It is exported so a caller that builds its own
// *exec.Cmd outside Run (for example a PTY-based runtime) can apply identical
// enforcement without copying the wrap logic. Call it after any sandbox
// wrapping and before cmd.Start; the wrap does not inspect the existing argv.
// Each call wraps again, so call it once per *exec.Cmd.
func ApplyResourceLimits(cmd *exec.Cmd, limits ResourceLimits) (cleanup func(), err error) {
	if limits.IsZero() {
		return func() {}, nil
	}
	return applyResourceLimitsImpl(cmd, limits)
}
