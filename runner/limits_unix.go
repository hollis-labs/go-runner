//go:build !windows

package runner

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

// applyResourceLimitsImpl wraps cmd's argv to enforce the configured
// resource limits. The wrap layers:
//
//  1. (Linux only, when available) systemd-run --user --scope --property=...
//     for cgroup v2 enforcement of memory limits.
//  2. sh -c "ulimit ...; exec \"$@\"" for kernel-enforced setrlimit
//     limits (CPU time, open files, processes, file size; memory when
//     systemd-run is unavailable).
//
// Layering: the systemd-run wrap is OUTERMOST (process-level cgroup),
// the sh -c is next (rlimit setup), the original argv is innermost.
// Caller-supplied sandbox wrapping (sandbox.Apply) sits between sh -c
// and the original argv when both are configured — applyResourceLimits
// runs AFTER sandbox.Apply in runOnce, so cmd.Args at this point may
// already be `sandbox-exec -p profile-id real-binary args...`. We wrap
// it without inspecting it.
//
// FDs (cmd.ExtraFiles, stdin/stdout/stderr) flow through unchanged:
// sh's `exec` builtin re-execs in-place, preserving FDs.
func applyResourceLimitsImpl(cmd *exec.Cmd, limits ResourceLimits) (func(), error) {
	shPath, err := exec.LookPath("sh")
	if err != nil {
		return nil, fmt.Errorf("sh not found in PATH for resource-limits wrap: %w", err)
	}

	useSystemdMemory := false
	if limits.MemoryMax > 0 && runtime.GOOS == "linux" && systemdRunUserAvailable() {
		useSystemdMemory = true
	}

	var ulimitParts []string
	if limits.CPUTime > 0 {
		sec := int(limits.CPUTime.Seconds())
		if sec < 1 {
			sec = 1
		}
		ulimitParts = append(ulimitParts, fmt.Sprintf("ulimit -t %d", sec))
	}
	if limits.MemoryMax > 0 && !useSystemdMemory && runtime.GOOS != "darwin" {
		// ulimit -v is in KiB on linux (RLIMIT_AS). macOS does not
		// expose RLIMIT_AS via bash's ulimit -v; without systemd-run
		// (always absent on darwin) MemoryMax is silently dropped.
		// Callers wanting hard memory limits on macOS must use
		// VM-based isolation (Lima / OrbStack) — see README.
		kb := limits.MemoryMax / 1024
		if kb < 1 {
			kb = 1
		}
		ulimitParts = append(ulimitParts, fmt.Sprintf("ulimit -v %d", kb))
	}
	if limits.MaxOpenFiles > 0 {
		ulimitParts = append(ulimitParts, fmt.Sprintf("ulimit -n %d", limits.MaxOpenFiles))
	}
	if limits.MaxProcesses > 0 {
		ulimitParts = append(ulimitParts, fmt.Sprintf("ulimit -u %d", limits.MaxProcesses))
	}
	if limits.MaxFileSize > 0 {
		// bash's ulimit -f default unit is 1024-byte blocks (kilobytes);
		// POSIX-strict mode uses 512-byte blocks but go-runner does not
		// invoke `set -o posix`, so 1024 is correct on both linux and
		// darwin's /bin/sh (which is bash on darwin and dash on linux —
		// dash also uses 1024-byte blocks for -f by default).
		blocks := limits.MaxFileSize / 1024
		if blocks < 1 {
			blocks = 1
		}
		ulimitParts = append(ulimitParts, fmt.Sprintf("ulimit -f %d", blocks))
	}

	origPath := cmd.Path
	origArgs := append([]string(nil), cmd.Args...)

	if len(ulimitParts) > 0 {
		// Build: sh -c "ulimit ...; exec "$@"" sh origPath origArgs[1:]...
		script := strings.Join(ulimitParts, "; ") + `; exec "$@"`
		newArgs := []string{shPath, "-c", script, "sh", origPath}
		if len(origArgs) > 1 {
			newArgs = append(newArgs, origArgs[1:]...)
		}
		cmd.Path = shPath
		cmd.Args = newArgs
	}

	if useSystemdMemory {
		srPath, _ := exec.LookPath("systemd-run")
		srArgs := []string{
			srPath,
			"--user",
			"--scope",
			"--quiet",
			fmt.Sprintf("--property=MemoryMax=%d", limits.MemoryMax),
		}
		srArgs = append(srArgs, "--")
		srArgs = append(srArgs, cmd.Args...)
		cmd.Path = srPath
		cmd.Args = srArgs
	}

	return func() {}, nil
}

var (
	systemdProbeOnce   sync.Once
	systemdProbeResult bool
)

// systemdRunUserAvailable returns true if `systemd-run --user` is
// usable (binary present + version probe succeeds). Probe runs once
// per process. On non-linux platforms always returns false.
func systemdRunUserAvailable() bool {
	systemdProbeOnce.Do(func() {
		if runtime.GOOS != "linux" {
			return
		}
		if _, err := exec.LookPath("systemd-run"); err != nil {
			return
		}
		cmd := exec.Command("systemd-run", "--user", "--version")
		if err := cmd.Run(); err != nil {
			return
		}
		systemdProbeResult = true
	})
	return systemdProbeResult
}
