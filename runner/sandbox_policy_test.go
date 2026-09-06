package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	llmtypes "github.com/hollis-labs/go-llm-types"
	"github.com/hollis-labs/go-sandbox/sandbox"
)

type contractAdapter struct{ binPath string }

func (a contractAdapter) Name() string                      { return "contract" }
func (a contractAdapter) BuildArgs(_, _, _ string) []string { return nil }
func (a contractAdapter) Detect() (string, bool)            { return a.binPath, true }
func (a contractAdapter) ParseLine(line []byte) ([]llmtypes.StreamEvent, error) {
	var raw struct {
		Type    string `json:"type"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, err
	}
	if raw.Type == "done" {
		return []llmtypes.StreamEvent{{Type: llmtypes.EventDone}}, nil
	}
	if raw.Type == "delta" {
		return []llmtypes.StreamEvent{{Type: llmtypes.EventDelta, Content: raw.Content}}, nil
	}
	return nil, nil
}

func writeShell(t *testing.T, dir, name, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test")
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body), 0o755); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func resolvedTestPolicy(id, workspace string) sandbox.ResolvedAccessPolicy {
	return sandbox.ResolvedAccessPolicy{
		ID:      id,
		Mode:    sandbox.ConfinementRequired,
		Backend: sandbox.BackendAuto,
		Roots: sandbox.ResolvedRoots{
			Project: workspace,
			Boot:    filepath.Join(workspace, "boot"),
			State:   filepath.Join(workspace, "state"),
			Scratch: filepath.Join(workspace, "scratch"),
			CWD:     workspace,
		},
		FS: sandbox.ResolvedFilesystemAccess{
			Read: []sandbox.ResolvedPath{{Kind: sandbox.AccessRead, Root: sandbox.ProjectRoot, Path: workspace, Source: "."}},
		},
		Network:    sandbox.NetworkAccess{Mode: sandbox.NetworkDeny},
		Subprocess: sandbox.SubprocessAllow,
	}
}

func TestSandboxPolicyAppliedBeforeEveryStartAndRestart(t *testing.T) {
	workspace := t.TempDir()
	bin := writeShell(t, workspace, "crash.sh", `printf '{"type":"done"}\n'
exit 2
`)

	oldApply := applyResolvedSandbox
	t.Cleanup(func() { applyResolvedSandbox = oldApply })

	policy := resolvedTestPolicy("policy-restart", workspace)
	var (
		mu       sync.Mutex
		calls    []string
		cleanups int
	)
	applyResolvedSandbox = func(cmd *exec.Cmd, p sandbox.ResolvedAccessPolicy) (sandbox.EnforcementOutcome, func(), error) {
		if cmd.Process != nil {
			t.Fatal("sandbox setup ran after process start")
		}
		mu.Lock()
		calls = append(calls, fmt.Sprintf("%s|dir=%s|argv=%v|env=%v", p.ID, cmd.Dir, cmd.Args, cmd.Env))
		mu.Unlock()
		out := sandbox.EnforcementOutcome{
			PolicyID:     p.ID,
			Mode:         p.Mode,
			Backend:      sandbox.BackendDarwinSeatbelt,
			State:        sandbox.EnforcementApplied,
			Enforced:     true,
			BackendGOOS:  runtime.GOOS,
			BackendReady: true,
		}
		return out, func() {
			mu.Lock()
			defer mu.Unlock()
			cleanups++
		}, nil
	}

	var events []Event
	err := Run(context.Background(), Config{
		Provider:      contractAdapter{binPath: bin},
		SandboxPolicy: &policy,
		Workspace:     workspace,
		Args:          []string{"--flag", "value"},
		Env:           []string{"RUNNER_CONTRACT=1"},
		Supervisor: &SupervisorOptions{
			RestartOnCrash:    2,
			MaxRestartBackoff: time.Millisecond,
		},
		OnEvent: func(ev Event) { events = append(events, ev) },
	})
	if err == nil {
		t.Fatal("expected crash after restart exhaustion")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 {
		t.Fatalf("sandbox apply calls = %d, want 3 (initial + 2 restarts): %#v", len(calls), calls)
	}
	if cleanups != 3 {
		t.Fatalf("sandbox cleanups = %d, want 3", cleanups)
	}
	for _, call := range calls {
		if !containsAll(call, "policy-restart", "dir="+workspace, "--flag", "RUNNER_CONTRACT=1") {
			t.Fatalf("sandbox setup lost policy/cwd/argv/env: %s", call)
		}
	}

	started := 0
	for _, ev := range events {
		if ev.Kind != EventProcessStarted {
			continue
		}
		started++
		out, ok := ev.Payload["sandbox"].(SandboxOutcome)
		if !ok {
			t.Fatalf("process.started missing sandbox outcome: %#v", ev.Payload)
		}
		if out.State != SandboxStateLaunched || !out.Enforced || out.PolicyID != policy.ID {
			t.Fatalf("process.started sandbox outcome = %#v, want launched/enforced policy", out)
		}
	}
	if started != 3 {
		t.Fatalf("process.started events = %d, want 3", started)
	}
}

func TestSandboxPolicySetupFailurePreventsStart(t *testing.T) {
	workspace := t.TempDir()
	marker := filepath.Join(workspace, "started")
	bin := writeShell(t, workspace, "should-not-run.sh", fmt.Sprintf(`touch %q
printf '{"type":"done"}\n'
`, marker))

	oldApply := applyResolvedSandbox
	t.Cleanup(func() { applyResolvedSandbox = oldApply })
	applyResolvedSandbox = func(cmd *exec.Cmd, p sandbox.ResolvedAccessPolicy) (sandbox.EnforcementOutcome, func(), error) {
		if cmd.Process != nil {
			t.Fatal("sandbox setup ran after process start")
		}
		return sandbox.EnforcementOutcome{
			PolicyID:    p.ID,
			Mode:        p.Mode,
			Backend:     sandbox.BackendLinuxBwrap,
			State:       sandbox.EnforcementUnsupported,
			Unsupported: []sandbox.Capability{sandbox.CapSubprocessDeny},
		}, nil, fmt.Errorf("%w: synthetic unsupported", sandbox.ErrUnsupportedPolicy)
	}

	policy := resolvedTestPolicy("policy-unsupported", workspace)
	policy.Subprocess = sandbox.SubprocessDeny
	var events []Event
	err := Run(context.Background(), Config{
		Provider:      contractAdapter{binPath: bin},
		SandboxPolicy: &policy,
		Workspace:     workspace,
		OnEvent:       func(ev Event) { events = append(events, ev) },
	})
	if err == nil {
		t.Fatal("expected sandbox setup error")
	}
	var sandboxErr *SandboxError
	if !errors.As(err, &sandboxErr) {
		t.Fatalf("error is not *SandboxError: %T %v", err, err)
	}
	if sandboxErr.Outcome.State != SandboxStateUnsupported {
		t.Fatalf("sandbox error outcome = %#v, want unsupported", sandboxErr.Outcome)
	}
	if len(events) != 0 {
		t.Fatalf("events emitted after setup failure = %#v, want none", events)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("child appears to have started; marker stat err = %v", statErr)
	}
}

func TestSandboxPolicyDisabledStartsUnwrappedAndReportsDisabled(t *testing.T) {
	workspace := t.TempDir()
	bin := writeShell(t, workspace, "ok-disabled.sh", `printf '{"type":"done"}\n'
`)
	policy := resolvedTestPolicy("policy-disabled", workspace)
	policy.Mode = sandbox.ConfinementDisabled

	var started SandboxOutcome
	if err := Run(context.Background(), Config{
		Provider:      contractAdapter{binPath: bin},
		SandboxPolicy: &policy,
		Workspace:     workspace,
		Args:          []string{"safe-arg"},
		Env:           []string{"RUNNER_SECRET=do-not-copy"},
		OnEvent: func(ev Event) {
			if ev.Kind == EventProcessStarted {
				started, _ = ev.Payload["sandbox"].(SandboxOutcome)
			}
		},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if started.State != SandboxStateDisabled || !started.Disabled || started.Enforced {
		t.Fatalf("sandbox outcome = %#v, want disabled and unenforced", started)
	}
	if fmt.Sprint(started.Diagnostics, started.PolicyID) == "RUNNER_SECRET=do-not-copy" {
		t.Fatal("sandbox outcome copied environment secret")
	}
}

func TestSandboxPolicyStartFailureReportsConfiguredNotLaunched(t *testing.T) {
	workspace := t.TempDir()
	oldApply := applyResolvedSandbox
	t.Cleanup(func() { applyResolvedSandbox = oldApply })
	applyResolvedSandbox = func(cmd *exec.Cmd, p sandbox.ResolvedAccessPolicy) (sandbox.EnforcementOutcome, func(), error) {
		return sandbox.EnforcementOutcome{
			PolicyID:     p.ID,
			Mode:         p.Mode,
			Backend:      sandbox.BackendDarwinSeatbelt,
			State:        sandbox.EnforcementApplied,
			Enforced:     true,
			BackendGOOS:  runtime.GOOS,
			BackendReady: true,
		}, func() {}, nil
	}

	policy := resolvedTestPolicy("policy-start-failure", workspace)
	var events []Event
	err := Run(context.Background(), Config{
		Provider:      contractAdapter{binPath: filepath.Join(workspace, "missing-binary")},
		SandboxPolicy: &policy,
		Workspace:     workspace,
		Args:          []string{"--token=secret-value"},
		Env:           []string{"SECRET_VALUE=secret-value"},
		OnEvent:       func(ev Event) { events = append(events, ev) },
	})
	if err == nil {
		t.Fatal("expected start error")
	}
	var startErr *StartError
	if !errors.As(err, &startErr) {
		t.Fatalf("error is not *StartError: %T %v", err, err)
	}
	if startErr.Outcome.State != SandboxStateConfigured || startErr.Outcome.PolicyID != policy.ID {
		t.Fatalf("start error outcome = %#v, want configured policy", startErr.Outcome)
	}
	if strings.Contains(fmt.Sprint(startErr.Outcome), "secret-value") {
		t.Fatalf("start error outcome leaked argv/env secret: %#v", startErr.Outcome)
	}
	if len(events) != 0 {
		t.Fatalf("events emitted before failed Start = %#v, want none", events)
	}
}

func TestRunResolvedSandboxRealBackendDeniesAccess(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("resolved sandbox backend unsupported on %s", runtime.GOOS)
	}
	if runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("sandbox-exec"); err != nil {
			t.Skip("sandbox-exec not installed")
		}
	}
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("bwrap"); err != nil {
			t.Skip("bwrap not installed")
		}
	}

	workspace := t.TempDir()
	for _, rel := range []string{"boot", "state", "scratch", "denied"} {
		if err := os.MkdirAll(filepath.Join(workspace, rel), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, "allowed.txt"), []byte("allowed\n"), 0o644); err != nil {
		t.Fatalf("write allowed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "denied", "secret.txt"), []byte("secret\n"), 0o644); err != nil {
		t.Fatalf("write denied: %v", err)
	}
	script := writeShell(t, workspace, "probe.sh", `cat "$PROJECT/allowed.txt"
if cat "$PROJECT/denied/secret.txt"; then
	echo "denied path was visible" >&2
	exit 42
fi
printf '{"type":"done"}\n'
`)

	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("resolve sh: %v", err)
	}
	policy, err := sandbox.ResolveAccessPolicy(sandbox.AccessPolicy{
		ID:      "runner-real-backend-smoke",
		Mode:    sandbox.ConfinementRequired,
		Backend: sandbox.BackendAuto,
		Roots: sandbox.Roots{
			Project: workspace,
			Boot:    filepath.Join(workspace, "boot"),
			State:   filepath.Join(workspace, "state"),
			Scratch: filepath.Join(workspace, "scratch"),
			CWD:     workspace,
		},
		FS: sandbox.FilesystemAccess{
			Read: []sandbox.PathRef{{Root: sandbox.ProjectRoot, Relative: "."}},
			Deny: []sandbox.PathRef{{Root: sandbox.ProjectRoot, Relative: "denied"}},
		},
		Runtime:    sandbox.RuntimeAccess{Executable: sandbox.PathRef{Path: shPath}},
		Scratch:    sandbox.ScratchAccess{Writable: true},
		Network:    sandbox.NetworkAccess{Mode: sandbox.NetworkDeny},
		Subprocess: sandbox.SubprocessAllow,
	})
	if err != nil {
		t.Fatalf("resolve access policy: %v", err)
	}

	var started SandboxOutcome
	err = Run(context.Background(), Config{
		Provider:      contractAdapter{binPath: shPath},
		SandboxPolicy: &policy,
		Workspace:     workspace,
		Args:          []string{script},
		Env:           []string{"PROJECT=" + workspace, "PATH=/bin:/usr/bin"},
		OnEvent: func(ev Event) {
			if ev.Kind == EventProcessStarted {
				started, _ = ev.Payload["sandbox"].(SandboxOutcome)
			}
		},
	})
	if err != nil {
		var sandboxErr *SandboxError
		if errors.As(err, &sandboxErr) && sandboxErr.Outcome.State == SandboxStateUnsupported {
			t.Skipf("resolved sandbox backend unavailable: %v", err)
		}
		t.Fatalf("Run: %v", err)
	}
	if started.State != SandboxStateLaunched || !started.Enforced || started.PolicyID != policy.ID {
		t.Fatalf("sandbox outcome = %#v, want launched/enforced %q", started, policy.ID)
	}
}

func TestRunResolvedSandboxRealBackendDeniesAccessOnRestart(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("resolved sandbox backend unsupported on %s", runtime.GOOS)
	}
	if runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("sandbox-exec"); err != nil {
			t.Skip("sandbox-exec not installed")
		}
	}
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("bwrap"); err != nil {
			t.Skip("bwrap not installed")
		}
	}

	workspace := t.TempDir()
	for _, rel := range []string{"boot", "state", "scratch", "denied"} {
		if err := os.MkdirAll(filepath.Join(workspace, rel), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, "allowed.txt"), []byte("allowed\n"), 0o644); err != nil {
		t.Fatalf("write allowed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "denied", "secret.txt"), []byte("secret\n"), 0o644); err != nil {
		t.Fatalf("write denied: %v", err)
	}
	script := writeShell(t, workspace, "restart-probe.sh", `cat "$PROJECT/allowed.txt" >/dev/null
if cat "$PROJECT/denied/secret.txt" >/dev/null 2>&1; then
	printf '{"type":"delta","content":"denied-visible"}\n'
	exit 42
fi
printf '{"type":"delta","content":"denied-blocked"}\n'
printf '{"type":"done"}\n'
exit 2
`)

	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("resolve sh: %v", err)
	}
	policy, err := sandbox.ResolveAccessPolicy(sandbox.AccessPolicy{
		ID:      "runner-real-backend-restart",
		Mode:    sandbox.ConfinementRequired,
		Backend: sandbox.BackendAuto,
		Roots: sandbox.Roots{
			Project: workspace,
			Boot:    filepath.Join(workspace, "boot"),
			State:   filepath.Join(workspace, "state"),
			Scratch: filepath.Join(workspace, "scratch"),
			CWD:     workspace,
		},
		FS: sandbox.FilesystemAccess{
			Read: []sandbox.PathRef{{Root: sandbox.ProjectRoot, Relative: "."}},
			Deny: []sandbox.PathRef{{Root: sandbox.ProjectRoot, Relative: "denied"}},
		},
		Runtime:    sandbox.RuntimeAccess{Executable: sandbox.PathRef{Path: shPath}},
		Scratch:    sandbox.ScratchAccess{Writable: true},
		Network:    sandbox.NetworkAccess{Mode: sandbox.NetworkDeny},
		Subprocess: sandbox.SubprocessAllow,
	})
	if err != nil {
		t.Fatalf("resolve access policy: %v", err)
	}

	var events []Event
	err = Run(context.Background(), Config{
		Provider:      contractAdapter{binPath: shPath},
		SandboxPolicy: &policy,
		Workspace:     workspace,
		Args:          []string{script},
		Env:           []string{"PROJECT=" + workspace, "PATH=/bin:/usr/bin"},
		Supervisor: &SupervisorOptions{
			RestartOnCrash:    2,
			MaxRestartBackoff: time.Millisecond,
		},
		OnEvent: func(ev Event) { events = append(events, ev) },
	})
	if err == nil {
		t.Fatal("expected crash after restart exhaustion")
	}
	var sandboxErr *SandboxError
	if errors.As(err, &sandboxErr) && sandboxErr.Outcome.State == SandboxStateUnsupported {
		t.Skipf("resolved sandbox backend unavailable: %v", err)
	}

	started := 0
	blocked := 0
	for _, ev := range events {
		switch ev.Kind {
		case EventProcessStarted:
			started++
			out, ok := ev.Payload["sandbox"].(SandboxOutcome)
			if !ok || out.State != SandboxStateLaunched || !out.Enforced || out.PolicyID != policy.ID {
				t.Fatalf("process.started sandbox outcome = %#v, want launched/enforced %q", ev.Payload["sandbox"], policy.ID)
			}
		case EventProviderEvent:
			stream, ok := ev.Payload["event"].(llmtypes.StreamEvent)
			if ok && stream.Type == llmtypes.EventDelta {
				if stream.Content == "denied-visible" {
					t.Fatalf("sandbox exposed denied path; events=%#v", events)
				}
				if stream.Content == "denied-blocked" {
					blocked++
				}
			}
		}
	}
	if started != 3 || blocked != 3 {
		t.Fatalf("restart denial evidence started=%d blocked=%d, want 3 each; events=%#v", started, blocked, events)
	}
}

func TestRunRejectsAmbiguousSandboxConfiguration(t *testing.T) {
	workspace := t.TempDir()
	policy := resolvedTestPolicy("policy", workspace)
	err := Run(context.Background(), Config{
		Provider:      contractAdapter{binPath: "/bin/true"},
		SandboxPolicy: &policy,
		Profile:       sandbox.Profile{ID: "legacy"},
		Workspace:     workspace,
		OnEvent:       func(Event) {},
	})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("Run error = %v, want mutually exclusive", err)
	}

	policy.ID = ""
	err = Run(context.Background(), Config{
		Provider:      contractAdapter{binPath: "/bin/true"},
		SandboxPolicy: &policy,
		Workspace:     workspace,
		OnEvent:       func(Event) {},
	})
	if err == nil || !strings.Contains(err.Error(), "SandboxPolicy.ID") {
		t.Fatalf("Run error = %v, want missing SandboxPolicy.ID", err)
	}
}

func TestLegacyProfileAdapterReportsCompatibilitySemantics(t *testing.T) {
	workspace := t.TempDir()
	bin := writeShell(t, workspace, "ok.sh", `printf '{"type":"done"}\n'
`)

	oldApply := applyLegacySandbox
	t.Cleanup(func() { applyLegacySandbox = oldApply })
	var gotWorkspace string
	applyLegacySandbox = func(cmd *exec.Cmd, p sandbox.Profile, workspace string) (func(), error) {
		if cmd.Process != nil {
			t.Fatal("legacy sandbox setup ran after process start")
		}
		gotWorkspace = workspace
		return func() {}, nil
	}

	var started SandboxOutcome
	err := Run(context.Background(), Config{
		Provider:  contractAdapter{binPath: bin},
		Profile:   sandbox.Profile{ID: "legacy-profile", FS: sandbox.FSSpec{Read: []string{"workspace"}}, Subprocess: true},
		Workspace: workspace,
		OnEvent: func(ev Event) {
			if ev.Kind == EventProcessStarted {
				started, _ = ev.Payload["sandbox"].(SandboxOutcome)
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotWorkspace != workspace {
		t.Fatalf("legacy apply workspace = %q, want %q", gotWorkspace, workspace)
	}
	if !started.Legacy || !started.LegacyDefaultAllow || started.State != SandboxStateLaunched || !started.Enforced {
		t.Fatalf("legacy sandbox outcome = %#v, want launched legacy default-allow outcome", started)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
