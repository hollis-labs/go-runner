package runner_test

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/hollis-labs/go-sandbox/sandbox"
)

// stubAdapter is a minimal CLIAdapter for the e2e test. It parses the
// NDJSON shape emitted by internal/stubcli — one object per line with a
// "type" field of "delta" or "done" — and maps each to a StreamEvent.
type stubAdapter struct{ binPath string }

func (s *stubAdapter) Name() string                      { return "stub" }
func (s *stubAdapter) BuildArgs(_, _, _ string) []string { return nil }
func (s *stubAdapter) Detect() (string, bool)            { return s.binPath, true }

func (s *stubAdapter) ParseLine(line []byte) ([]provider.StreamEvent, error) {
	var raw struct {
		Type    string `json:"type"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, err
	}
	switch raw.Type {
	case "delta":
		return []provider.StreamEvent{{Type: provider.EventDelta, Content: raw.Content}}, nil
	case "done":
		return []provider.StreamEvent{{Type: provider.EventDone}}, nil
	}
	return nil, nil
}

func buildStubCLI(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "stubcli")
	cmd := exec.Command("go", "build", "-o", out, "github.com/hollis-labs/go-runner/internal/stubcli")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build stubcli: %v", err)
	}
	return out
}

func TestRun_E2E_StubCLIUnderSandbox(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("sandbox.Apply unsupported on %s", runtime.GOOS)
	}
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("bwrap"); err != nil {
			t.Skip("bwrap not installed")
		}
	}

	bin := buildStubCLI(t)
	workspace := t.TempDir()

	profile := sandbox.Profile{
		ID:          "go-runner-e2e",
		Description: "e2e: stub CLI under sandbox-exec / bwrap",
		FS:          sandbox.FSSpec{Read: []string{"workspace"}, Write: []string{"workspace"}},
		Net:         false,
		Subprocess:  true, // stubcli does not fork; permissive for test simplicity
	}

	var (
		mu     sync.Mutex
		events []runner.Event
	)

	cfg := runner.Config{
		Provider:  &stubAdapter{binPath: bin},
		Profile:   profile,
		Workspace: workspace,
		Args:      []string{"-count", "2"},
		WaitDelay: 1 * time.Second,
		OnEvent: func(ev runner.Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := runner.Run(ctx, cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(events) < 4 {
		t.Fatalf("expected >=4 events (started + 2 delta + done + exited), got %d: %+v", len(events), events)
	}

	if events[0].Kind != runner.EventProcessStarted {
		t.Errorf("events[0] kind = %q, want %q", events[0].Kind, runner.EventProcessStarted)
	}
	if pid, ok := events[0].Payload["pid"].(int); !ok || pid <= 0 {
		t.Errorf("events[0] pid not a positive int: %v", events[0].Payload["pid"])
	}

	last := events[len(events)-1]
	if last.Kind != runner.EventProcessExited {
		t.Errorf("last event kind = %q, want %q", last.Kind, runner.EventProcessExited)
	}
	if code, _ := last.Payload["exit_code"].(int); code != 0 {
		t.Errorf("last exit_code = %d, want 0", code)
	}

	var (
		deltas        int
		sawTerminal   bool
		terminalIndex = -1
	)
	for i, ev := range events {
		if ev.Kind != runner.EventProviderEvent {
			continue
		}
		se, ok := ev.Payload["event"].(provider.StreamEvent)
		if !ok {
			t.Fatalf("provider.event payload missing event: %+v", ev.Payload)
		}
		switch se.Type {
		case provider.EventDelta:
			deltas++
		case provider.EventDone:
			sawTerminal = true
			terminalIndex = i
			if !ev.Payload["is_turn_complete"].(bool) {
				t.Errorf("EventDone is_turn_complete = false, want true")
			}
		}
	}
	if deltas != 2 {
		t.Errorf("delta count = %d, want 2", deltas)
	}
	if !sawTerminal {
		t.Error("no terminal provider.event observed")
	}
	if terminalIndex >= 0 && terminalIndex >= len(events)-1 {
		t.Error("terminal provider event came after process.exited; expected before")
	}
}

func TestRun_RequiresProvider(t *testing.T) {
	err := runner.Run(context.Background(), runner.Config{
		Workspace: t.TempDir(),
		OnEvent:   func(runner.Event) {},
	})
	if err == nil {
		t.Fatal("expected error for missing Provider")
	}
}

func TestRun_RequiresWorkspace(t *testing.T) {
	err := runner.Run(context.Background(), runner.Config{
		Provider: &stubAdapter{binPath: "/bin/true"},
		OnEvent:  func(runner.Event) {},
	})
	if err == nil {
		t.Fatal("expected error for missing Workspace")
	}
}

func TestRun_RequiresOnEvent(t *testing.T) {
	err := runner.Run(context.Background(), runner.Config{
		Provider:  &stubAdapter{binPath: "/bin/true"},
		Workspace: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error for missing OnEvent")
	}
}

func TestRun_NoProfile_SkipsSandbox(t *testing.T) {
	bin := buildStubCLI(t)
	workspace := t.TempDir()
	var count int
	cfg := runner.Config{
		Provider:  &stubAdapter{binPath: bin},
		Workspace: workspace,
		Args:      []string{"-count", "1"},
		OnEvent:   func(runner.Event) { count++ },
	}
	if err := runner.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run without profile: %v", err)
	}
	if count < 3 {
		t.Errorf("expected >=3 events without sandbox, got %d", count)
	}
}

func TestRun_Stderr_CapturesToWriter(t *testing.T) {
	bin := buildStubCLI(t)
	workspace := t.TempDir()

	var stderrBuf bytes.Buffer
	cfg := runner.Config{
		Provider:  &stubAdapter{binPath: bin},
		Workspace: workspace,
		Args:      []string{"-count", "1", "-stderr-msg", "diagnostic-line"},
		Stderr:    &stderrBuf,
		OnEvent:   func(runner.Event) {},
	}
	if err := runner.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := stderrBuf.String()
	if !strings.Contains(got, "diagnostic-line") {
		t.Errorf("Stderr buffer = %q, want to contain %q", got, "diagnostic-line")
	}
}

func TestRun_Stderr_NilLeavesCmdStderrUnset(t *testing.T) {
	// Sanity: passing a nil Stderr (the zero-value default) does not
	// regress the "no caller knob" behavior — Run completes cleanly and
	// any process stderr goes to /dev/null per os/exec's default.
	bin := buildStubCLI(t)
	workspace := t.TempDir()
	cfg := runner.Config{
		Provider:  &stubAdapter{binPath: bin},
		Workspace: workspace,
		Args:      []string{"-count", "1", "-stderr-msg", "should-be-discarded"},
		OnEvent:   func(runner.Event) {},
	}
	if err := runner.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run with nil Stderr: %v", err)
	}
}

func TestRun_ExitError_NonZeroExit(t *testing.T) {
	bin := buildStubCLI(t)
	workspace := t.TempDir()

	var (
		mu       sync.Mutex
		exitedEv runner.Event
	)
	cfg := runner.Config{
		Provider:  &stubAdapter{binPath: bin},
		Workspace: workspace,
		Args:      []string{"-count", "1", "-fail"},
		OnEvent: func(ev runner.Event) {
			if ev.Kind != runner.EventProcessExited {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			exitedEv = ev
		},
	}

	err := runner.Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for -fail (exit 2)")
	}

	var xe *runner.ExitError
	if !errors.As(err, &xe) {
		t.Fatalf("error is not *ExitError: %T %v", err, err)
	}
	if xe.Code != 2 {
		t.Errorf("ExitError.Code = %d, want 2", xe.Code)
	}
	if xe.Signal != 0 {
		t.Errorf("ExitError.Signal = %d, want 0 (clean non-zero exit)", xe.Signal)
	}
	if xe.Killed {
		t.Error("ExitError.Killed = true, want false (non-signal exit)")
	}
	if xe.Cause != "" {
		t.Errorf("ExitError.Cause = %q, want \"\" (no supervisor cause)", xe.Cause)
	}
	if xe.ProcessState == nil {
		t.Error("ExitError.ProcessState is nil")
	}

	mu.Lock()
	defer mu.Unlock()
	if got, _ := exitedEv.Payload["exit_code"].(int); got != 2 {
		t.Errorf("EventProcessExited payload exit_code = %v, want 2", exitedEv.Payload["exit_code"])
	}
	if got, _ := exitedEv.Payload["signal"].(int); got != 0 {
		t.Errorf("EventProcessExited payload signal = %v, want 0", exitedEv.Payload["signal"])
	}
	if got, _ := exitedEv.Payload["killed"].(bool); got {
		t.Error("EventProcessExited payload killed = true, want false")
	}
	if got, _ := exitedEv.Payload["cause"].(string); got != "" {
		t.Errorf("EventProcessExited payload cause = %q, want \"\"", got)
	}
}

func TestRun_ExitError_CleanExit_ReturnsNil(t *testing.T) {
	bin := buildStubCLI(t)
	workspace := t.TempDir()
	cfg := runner.Config{
		Provider:  &stubAdapter{binPath: bin},
		Workspace: workspace,
		Args:      []string{"-count", "1"},
		OnEvent:   func(runner.Event) {},
	}
	err := runner.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("clean exit returned error: %v", err)
	}
}

func TestRun_ExitError_SIGTERM_FromContextCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix signals only")
	}
	bin := buildStubCLI(t)
	workspace := t.TempDir()

	// stubcli with -sleep 30s; we cancel ctx ~immediately; cmd.Cancel
	// delivers SIGTERM which Go's runtime translates into orderly exit
	// (signal terminates the program). ExitError should reflect Signal=15,
	// Killed=false (SIGTERM != SIGKILL).
	startedCh := make(chan struct{}, 1)
	cfg := runner.Config{
		Provider:  &stubAdapter{binPath: bin},
		Workspace: workspace,
		Args:      []string{"-count", "1", "-sleep", "30s"},
		WaitDelay: 5 * time.Second,
		OnEvent: func(ev runner.Event) {
			if ev.Kind == runner.EventProcessStarted {
				select {
				case startedCh <- struct{}{}:
				default:
				}
			}
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct{ err error }
	done := make(chan result, 1)
	go func() { done <- result{err: runner.Run(ctx, cfg)} }()

	select {
	case <-startedCh:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("process did not start within 5s")
	}

	cancel()
	res := <-done

	if res.err == nil {
		t.Fatal("expected non-nil error after SIGTERM")
	}
	var xe *runner.ExitError
	if !errors.As(res.err, &xe) {
		t.Fatalf("error is not *ExitError: %T %v", res.err, res.err)
	}
	if xe.Signal != int(syscall.SIGTERM) {
		t.Errorf("ExitError.Signal = %d, want %d (SIGTERM)", xe.Signal, syscall.SIGTERM)
	}
	if xe.Killed {
		t.Errorf("ExitError.Killed = true, want false (SIGTERM is not SIGKILL)")
	}
}

func TestRun_ExitError_SIGKILL_AfterWaitDelay(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix signals only")
	}
	bin := buildStubCLI(t)
	workspace := t.TempDir()

	// stubcli with -trap-sigterm swallows SIGTERM; cmd.WaitDelay then
	// elapses and os/exec sends SIGKILL. ExitError should reflect
	// Signal=9, Killed=true.
	startedCh := make(chan struct{}, 1)
	cfg := runner.Config{
		Provider:  &stubAdapter{binPath: bin},
		Workspace: workspace,
		Args:      []string{"-count", "1", "-sleep", "30s", "-trap-sigterm"},
		WaitDelay: 500 * time.Millisecond,
		OnEvent: func(ev runner.Event) {
			if ev.Kind == runner.EventProcessStarted {
				select {
				case startedCh <- struct{}{}:
				default:
				}
			}
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct{ err error }
	done := make(chan result, 1)
	go func() { done <- result{err: runner.Run(ctx, cfg)} }()

	select {
	case <-startedCh:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("process did not start within 5s")
	}
	// Give the trap a moment to install.
	time.Sleep(200 * time.Millisecond)

	cancel()
	res := <-done

	if res.err == nil {
		t.Fatal("expected non-nil error after SIGKILL")
	}
	var xe *runner.ExitError
	if !errors.As(res.err, &xe) {
		t.Fatalf("error is not *ExitError: %T %v", res.err, res.err)
	}
	if xe.Signal != int(syscall.SIGKILL) {
		t.Errorf("ExitError.Signal = %d, want %d (SIGKILL)", xe.Signal, syscall.SIGKILL)
	}
	if !xe.Killed {
		t.Error("ExitError.Killed = false, want true (terminated by SIGKILL)")
	}
}

func TestConfig_ExtraFiles_PassesToChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test script needs sh; not running on Windows")
	}

	workspace := t.TempDir()
	script := filepath.Join(workspace, "fake-cli.sh")
	body := `#!/bin/sh
IFS= read -r payload <&3
printf '{"type":"delta","content":"%s"}\n' "$payload"
printf '{"type":"done"}\n'
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer readEnd.Close()

	if _, err := writeEnd.WriteString("fd3-payload\n"); err != nil {
		t.Fatalf("write pipe: %v", err)
	}
	if err := writeEnd.Close(); err != nil {
		t.Fatalf("close write pipe: %v", err)
	}

	var (
		mu     sync.Mutex
		events []runner.Event
	)
	cfg := runner.Config{
		Provider:   &stubAdapter{binPath: script},
		Workspace:  workspace,
		ExtraFiles: []*os.File{readEnd},
		OnEvent: func(ev runner.Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		},
	}

	if err := runner.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	var gotDelta string
	for _, ev := range events {
		if ev.Kind != runner.EventProviderEvent {
			continue
		}
		se, ok := ev.Payload["event"].(provider.StreamEvent)
		if !ok {
			continue
		}
		if se.Type == provider.EventDelta {
			gotDelta = se.Content
			break
		}
	}
	if gotDelta != "fd3-payload" {
		t.Fatalf("delta content = %q, want %q", gotDelta, "fd3-payload")
	}
}
