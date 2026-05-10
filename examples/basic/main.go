// Example: spawn the Claude CLI under go-runner with no sandbox and no
// supervision, and print every event observed by the runner to stdout.
//
// Prerequisites:
//   - The `claude` CLI on $PATH (https://github.com/anthropics/claude-code).
//   - A Claude API session already authenticated for the local user.
//
// Run:
//
//	go run ./examples/basic -prompt "explain quicksort in one sentence"
//
// The runner emits four kinds of events for a normal turn:
//   - process.started   (once, after spawn)
//   - provider.event    (one per parsed StreamEvent from stdout)
//   - process.exited    (once, after cmd.Wait)
//
// See README §"Event alphabet" for the full set including supervisor
// events when Config.Supervisor is non-nil.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	llmtypes "github.com/hollis-labs/go-llm-types"
	"github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/go-runner/runner"
)

func main() {
	prompt := flag.String("prompt", "say hello", "prompt to send to claude")
	timeout := flag.Duration("timeout", 60*time.Second, "overall run timeout")
	flag.Parse()

	workspace, err := os.Getwd()
	if err != nil {
		log.Fatalf("getwd: %v", err)
	}

	adapter := provider.NewClaudeAdapter()
	if _, ok := adapter.Detect(); !ok {
		log.Fatalf("claude binary not found on PATH; install Claude Code or adjust PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	cfg := runner.Config{
		Provider:  adapter,
		Workspace: workspace,
		Args:      adapter.BuildArgs(*prompt, "", ""),
		Stderr:    os.Stderr, // surface adapter / CLI diagnostics directly
		OnEvent: func(ev runner.Event) {
			switch ev.Kind {
			case runner.EventProcessStarted:
				fmt.Printf("[started] pid=%v binary=%v\n",
					ev.Payload["pid"], ev.Payload["binary"])
			case runner.EventProviderEvent:
				se, _ := ev.Payload["event"].(llmtypes.StreamEvent)
				done, _ := ev.Payload["is_turn_complete"].(bool)
				fmt.Printf("[event] type=%s done=%v content=%q\n",
					se.Type, done, se.Content)
			case runner.EventProcessExited:
				fmt.Printf("[exited] code=%v signal=%v killed=%v cause=%q\n",
					ev.Payload["exit_code"], ev.Payload["signal"],
					ev.Payload["killed"], ev.Payload["cause"])
			case runner.EventProcessTimeout:
				fmt.Printf("[timeout] %v\n", ev.Payload["error"])
			}
		},
	}

	if err := runner.Run(ctx, cfg); err != nil {
		log.Fatalf("run: %v", err)
	}
}
