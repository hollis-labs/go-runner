package runner_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/go-runner/runner"
)

// TestResourceLimits_RlimitsApplied verifies the runner's argv wrap
// actually changes the rlimits seen by the child. stubcli's
// -show-rlimits flag prints RLIMIT_CPU, RLIMIT_NOFILE, RLIMIT_FSIZE
// values as delta lines; we capture them and compare to the configured
// values.
func TestResourceLimits_RlimitsApplied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ResourceLimits unsupported on windows")
	}
	bin := buildStubCLI(t)
	workspace := t.TempDir()

	wantCPU := uint64(60) // seconds
	wantNofile := uint64(64)
	wantFsize := uint64(1024 * 1024) // 1 MiB
	limits := runner.ResourceLimits{
		CPUTime:      time.Duration(wantCPU) * time.Second,
		MaxOpenFiles: wantNofile,
		MaxFileSize:  wantFsize,
	}

	var (
		mu     sync.Mutex
		deltas []string
	)
	cfg := runner.Config{
		Provider:       &stubAdapter{binPath: bin},
		Workspace:      workspace,
		Args:           []string{"-count", "0", "-show-rlimits"},
		ResourceLimits: limits,
		OnEvent: func(ev runner.Event) {
			if ev.Kind != runner.EventProviderEvent {
				return
			}
			se, ok := ev.Payload["event"].(provider.StreamEvent)
			if !ok || se.Type != provider.EventDelta {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			deltas = append(deltas, se.Content)
		},
	}

	if err := runner.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(deltas, "")

	wantSubstrs := []string{
		"cpu=60 ",
		"nofile=64 ",
		// FSIZE is reported in bytes via Getrlimit (Go's syscall.Rlimit
		// returns the raw kernel value); ulimit -f sets in 512-byte
		// blocks, so 1 MiB = 2048 blocks = 1048576 bytes.
		"fsize=1048576 ",
	}
	for _, want := range wantSubstrs {
		if !strings.Contains(joined, want) {
			t.Errorf("rlimit deltas %q missing %q", joined, want)
		}
	}
}

// TestResourceLimits_CPUTime functionally verifies CPUTime enforcement.
// Uses a sh busy-loop (not a Go binary) because Go's runtime swallows
// SIGXCPU on at least darwin — the runtime catches the signal and
// lets the program continue. Native C-based binaries (sh, yes, dd)
// honor SIGXCPU normally and terminate at the soft limit.
func TestResourceLimits_CPUTime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ResourceLimits unsupported on windows")
	}
	workspace := t.TempDir()

	// sh script: emit one delta line then busy-loop. RLIMIT_CPU=1s
	// fires SIGXCPU which sh's default handler honors (terminates).
	script := filepath.Join(workspace, "burn.sh")
	body := `#!/bin/sh
echo '{"type":"delta","content":"start"}'
while :; do : $((1+1)); done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	cfg := runner.Config{
		Provider:  &stubAdapter{binPath: script},
		Workspace: workspace,
		Args:      []string{},
		ResourceLimits: runner.ResourceLimits{
			CPUTime: 1 * time.Second,
		},
		OnEvent: func(runner.Event) {},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	err := runner.Run(ctx, cfg)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected non-nil error after CPU time limit")
	}
	var xe *runner.ExitError
	if !errors.As(err, &xe) {
		t.Fatalf("error is not *ExitError: %T %v", err, err)
	}
	if xe.Signal != int(syscall.SIGXCPU) && xe.Signal != int(syscall.SIGKILL) {
		t.Errorf("ExitError.Signal = %d, want SIGXCPU(%d) or SIGKILL(%d)",
			xe.Signal, syscall.SIGXCPU, syscall.SIGKILL)
	}
	if elapsed > 8*time.Second {
		t.Errorf("Run took %v, expected <8s with CPUTime=1s", elapsed)
	}
}


// TestResourceLimits_MemoryMax_Linux verifies cgroup-based MemoryMax
// enforcement when systemd-run --user is available. Skips on darwin
// (RLIMIT_AS unsupported there) and on linux without systemd.
func TestResourceLimits_MemoryMax_Linux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("MemoryMax cgroup enforcement is linux-specific")
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("systemd-run not installed")
	}
	probe := exec.Command("systemd-run", "--user", "--version")
	if err := probe.Run(); err != nil {
		t.Skip("systemd-run --user unavailable in this environment")
	}

	bin := buildStubCLI(t)
	workspace := t.TempDir()

	cfg := runner.Config{
		Provider:  &stubAdapter{binPath: bin},
		Workspace: workspace,
		Args:      []string{"-count", "1", "-malloc-mb", "200"},
		ResourceLimits: runner.ResourceLimits{
			MemoryMax: 50 * 1024 * 1024,
		},
		OnEvent: func(runner.Event) {},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := runner.Run(ctx, cfg)
	if err == nil {
		t.Fatal("expected non-nil error after memory limit OOM")
	}
	var xe *runner.ExitError
	if !errors.As(err, &xe) {
		t.Fatalf("error is not *ExitError: %T %v", err, err)
	}
	// cgroup OOM-kill: SIGKILL. Process may also crash via Go runtime
	// abort if RLIMIT_AS happens to be set additionally.
	if xe.Signal != int(syscall.SIGKILL) && xe.Code == 0 {
		t.Errorf("ExitError = %+v, expected SIGKILL or non-zero exit from OOM", xe)
	}
}

// TestResourceLimits_ZeroIsNoOp regression-guards the IsZero path: an
// empty ResourceLimits must not wrap argv (so cmd.Args / cmd.Path stay
// unchanged) and must not break the existing Run path.
func TestResourceLimits_ZeroIsNoOp(t *testing.T) {
	bin := buildStubCLI(t)
	workspace := t.TempDir()

	var startedArgs []string
	cfg := runner.Config{
		Provider:  &stubAdapter{binPath: bin},
		Workspace: workspace,
		Args:      []string{"-count", "1"},
		// ResourceLimits left zero
		OnEvent: func(ev runner.Event) {
			if ev.Kind == runner.EventProcessStarted {
				startedArgs = ev.Payload["args"].([]string)
			}
		},
	}
	if err := runner.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(startedArgs) == 0 {
		t.Fatal("EventProcessStarted args empty")
	}
	if !strings.HasSuffix(startedArgs[0], "stubcli") {
		t.Errorf("startedArgs[0] = %q, want path ending in stubcli (zero ResourceLimits should not wrap)",
			startedArgs[0])
	}
}
