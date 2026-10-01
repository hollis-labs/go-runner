//go:build !windows

package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// and the original argv when both are configured — ApplyResourceLimits
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
		// ulimit -f counts blocks whose size depends on the shell: see
		// shellFileSizeBlock.
		blocks := limits.MaxFileSize / shellFileSizeBlock(shPath)
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
			// Without this the scope may swap past MemoryMax: cgroup v2
			// reclaims to swap before it OOM-kills, and a transient
			// scope's memory.swap.max defaults to max. On a host with
			// free swap a 200 MiB allocation under MemoryMax=50M then
			// finishes cleanly (CW-20261001-0108).
			"--property=MemorySwapMax=0",
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

// systemdRunUserAvailable returns true if `systemd-run --user --scope`
// works here: it starts a transient scope running true. `--version` alone
// never contacts the user manager, so it succeeded where the manager is
// unreachable (no user bus, a sandbox hiding it) and every MemoryMax launch
// then failed instead of falling back to ulimit -v. Probe runs once per
// process. On non-linux platforms always returns false.
func systemdRunUserAvailable() bool {
	systemdProbeOnce.Do(func() {
		if runtime.GOOS != "linux" {
			return
		}
		srPath, err := exec.LookPath("systemd-run")
		if err != nil {
			return
		}
		truePath, err := exec.LookPath("true")
		if err != nil {
			return
		}
		cmd := exec.Command(srPath, "--user", "--scope", "--quiet", "--", truePath)
		if err := cmd.Run(); err != nil {
			return
		}
		systemdProbeResult = true
	})
	return systemdProbeResult
}

var (
	fsizeBlockOnce sync.Once
	fsizeBlock     uint64
)

// shellFileSizeBlock reports the size of the blocks sh's `ulimit -f`
// counts. POSIX says 512 bytes, and dash, busybox and bash in POSIX mode
// (bash 4+ invoked as sh) count 512; bash 3.2, macOS's /bin/sh, counts 1024.
// go-runner assumed 1024 everywhere, so on Linux MaxFileSize came out at
// half (CW-20261001-0108). It is measured once: under `ulimit -f 1`, a
// 600-byte write stops at 512 bytes if a block is 512 bytes and completes
// if it is 1024. If the measurement fails it answers 1024, which never
// grants more than was asked.
func shellFileSizeBlock(shPath string) uint64 {
	fsizeBlockOnce.Do(func() {
		fsizeBlock = measureFileSizeBlock(shPath)
	})
	return fsizeBlock
}

func measureFileSizeBlock(shPath string) uint64 {
	dir, err := os.MkdirTemp("", "go-runner-fsize-")
	if err != nil {
		return 1024
	}
	defer func() { _ = os.RemoveAll(dir) }()
	probe := filepath.Join(dir, "probe")
	// The write past the limit raises SIGXFSZ, which ends the shell; only
	// the file's size matters.
	_ = exec.Command(shPath, "-c", `ulimit -f 1 && printf '%0600d' 0 > "$1"`, "sh", probe).Run()
	if info, err := os.Stat(probe); err == nil && info.Size() == 512 {
		return 512
	}
	return 1024
}
