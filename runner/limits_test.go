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

	llmtypes "github.com/hollis-labs/go-llm-types"
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
			se, ok := ev.Payload["event"].(llmtypes.StreamEvent)
			if !ok || se.Type != llmtypes.EventDelta {
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

// TestApplyResourceLimits_ArgvShape verifies the exported wrap rewrites a
// synthetic *exec.Cmd into `sh -c "ulimit ...; exec \"$@\"" sh <orig argv>`
// without spawning anything. MemoryMax is deliberately absent: its argv
// depends on the host (systemd-run probe on Linux, dropped on darwin).
func TestApplyResourceLimits_ArgvShape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ResourceLimits unsupported on windows")
	}
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not found: %v", err)
	}
	cases := []struct {
		name   string
		limits runner.ResourceLimits
		script string
	}{
		{"CPUTime", runner.ResourceLimits{CPUTime: 30 * time.Second}, `ulimit -t 30; exec "$@"`},
		{"CPUTimeSubSecondRoundsUp", runner.ResourceLimits{CPUTime: 10 * time.Millisecond}, `ulimit -t 1; exec "$@"`},
		{"MaxOpenFiles", runner.ResourceLimits{MaxOpenFiles: 64}, `ulimit -n 64; exec "$@"`},
		{"MaxProcesses", runner.ResourceLimits{MaxProcesses: 128}, `ulimit -u 128; exec "$@"`},
		{"MaxFileSize", runner.ResourceLimits{MaxFileSize: 2048}, `ulimit -f 2; exec "$@"`},
		{"MaxFileSizeSubBlockRoundsUp", runner.ResourceLimits{MaxFileSize: 10}, `ulimit -f 1; exec "$@"`},
		{
			"AllUlimitFields",
			runner.ResourceLimits{CPUTime: 30 * time.Second, MaxOpenFiles: 64, MaxProcesses: 128, MaxFileSize: 2048},
			`ulimit -t 30; ulimit -n 64; ulimit -u 128; ulimit -f 2; exec "$@"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("/bin/echo", "hello", "world")
			origPath := cmd.Path
			cleanup, err := runner.ApplyResourceLimits(cmd, tc.limits)
			if err != nil {
				t.Fatalf("ApplyResourceLimits: %v", err)
			}
			if cleanup == nil {
				t.Fatal("cleanup is nil")
			}
			cleanup()
			if cmd.Path != shPath {
				t.Errorf("cmd.Path = %q, want %q", cmd.Path, shPath)
			}
			want := []string{shPath, "-c", tc.script, "sh", origPath, "hello", "world"}
			if strings.Join(cmd.Args, "\x00") != strings.Join(want, "\x00") {
				t.Errorf("cmd.Args = %q, want %q", cmd.Args, want)
			}
		})
	}
}

// TestApplyResourceLimits_ZeroIsNoOp verifies the IsZero short-circuit
// leaves the cmd untouched and returns a callable no-op cleanup.
func TestApplyResourceLimits_ZeroIsNoOp(t *testing.T) {
	cmd := exec.Command("/bin/echo", "hi")
	origPath := cmd.Path
	origArgs := append([]string(nil), cmd.Args...)
	cleanup, err := runner.ApplyResourceLimits(cmd, runner.ResourceLimits{})
	if err != nil {
		t.Fatalf("ApplyResourceLimits: %v", err)
	}
	if cleanup == nil {
		t.Fatal("cleanup is nil")
	}
	cleanup()
	if cmd.Path != origPath || strings.Join(cmd.Args, "\x00") != strings.Join(origArgs, "\x00") {
		t.Errorf("cmd mutated: path=%q args=%q", cmd.Path, cmd.Args)
	}
}

// TestApplyResourceLimits_WindowsUnsupported pins the documented Windows
// behavior: non-zero limits are an error and the cmd is not rewritten.
func TestApplyResourceLimits_WindowsUnsupported(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only behavior")
	}
	cmd := exec.Command("cmd", "/c", "echo")
	if _, err := runner.ApplyResourceLimits(cmd, runner.ResourceLimits{MaxOpenFiles: 64}); err == nil {
		t.Fatal("expected error on windows for non-zero limits")
	}
}

// TestApplyResourceLimits_EnforcedOnRealChild runs a small `sh` child through
// the wrap and reads back the limits it sees. The limits are tiny and only
// lower the child's own rlimits; nothing here consumes real resources.
func TestApplyResourceLimits_EnforcedOnRealChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ResourceLimits unsupported on windows")
	}
	cmd := exec.Command("sh", "-c", `echo "nofile=$(ulimit -n) fsize=$(ulimit -f)"`)
	cleanup, err := runner.ApplyResourceLimits(cmd, runner.ResourceLimits{MaxOpenFiles: 48, MaxFileSize: 4096})
	if err != nil {
		t.Fatalf("ApplyResourceLimits: %v", err)
	}
	defer cleanup()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run wrapped cmd: %v", err)
	}
	if got, want := strings.TrimSpace(string(out)), "nofile=48 fsize=4"; got != want {
		t.Errorf("child saw %q, want %q", got, want)
	}
}
