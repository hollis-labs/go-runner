package runner_test

import (
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"github.com/hollis-labs/go-runner/runner"
)

// Apply the same resource-limit wrap Run uses to a *exec.Cmd you built
// yourself (for example for a PTY-based runtime that never calls Run).
func ExampleApplyResourceLimits() {
	if runtime.GOOS == "windows" {
		// ResourceLimits are unsupported on Windows; ApplyResourceLimits
		// returns an error for non-zero limits there.
		fmt.Println("nofile=32")
		return
	}
	cmd := exec.Command("sh", "-c", `echo "nofile=$(ulimit -n)"`)
	cleanup, err := runner.ApplyResourceLimits(cmd, runner.ResourceLimits{MaxOpenFiles: 32})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer cleanup()

	out, err := cmd.Output()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Print(string(out))
	// Output: nofile=32
}

// Compute the backoff Run's Supervisor uses between restart attempts, for use
// in your own restart loop.
func ExampleComputeRestartBackoff() {
	for attempt := 1; attempt <= 6; attempt++ {
		fmt.Println(attempt, runner.ComputeRestartBackoff(attempt, 10*time.Second))
	}
	// Output:
	// 1 1s
	// 2 2s
	// 3 4s
	// 4 8s
	// 5 10s
	// 6 10s
}
