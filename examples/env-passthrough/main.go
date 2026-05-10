// Example: demonstrate Config.Env behavior. The runner forwards Config.Env
// to *exec.Cmd verbatim:
//
//	nil            → child inherits the parent process environment
//	[]string{}     → child runs with an empty environment
//	[]string{...}  → child runs with exactly those entries
//
// We spawn /usr/bin/env and parse each output line as a synthetic delta
// event so the runner's event callback can show what the child saw.
//
// Run:
//
//	RUNNER_DEMO_INHERITED=from-parent go run ./examples/env-passthrough -mode inherit
//	RUNNER_DEMO_INHERITED=from-parent go run ./examples/env-passthrough -mode empty
//	RUNNER_DEMO_INHERITED=from-parent go run ./examples/env-passthrough -mode custom
//
// Use this pattern when adapting non-LLM CLIs: any line-delimited stdout
// can be wrapped in a minimal CLIAdapter that emits each line as a delta.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	llmtypes "github.com/hollis-labs/go-llm-types"
	"github.com/hollis-labs/go-runner/runner"
)

// echoAdapter is a minimal CLIAdapter for binaries that write
// human-readable line-delimited output (env, ls, cat ...). It treats each
// non-empty line as a single delta StreamEvent. Real LLM adapters live in
// go-providers; this is here purely to demonstrate the runner's Env
// passthrough without needing a model on PATH.
type echoAdapter struct {
	name   string
	binary string
}

func (a echoAdapter) Name() string { return a.name }

func (a echoAdapter) BuildArgs(prompt, system, sessionID string) []string { return nil }

func (a echoAdapter) Detect() (string, bool) {
	if _, err := os.Stat(a.binary); err != nil {
		return "", false
	}
	return a.binary, true
}

func (a echoAdapter) ParseLine(line []byte) ([]llmtypes.StreamEvent, error) {
	return []llmtypes.StreamEvent{{
		Type:    llmtypes.EventDelta,
		Content: string(line),
	}}, nil
}

func main() {
	mode := flag.String("mode", "inherit", "inherit | empty | custom")
	flag.Parse()

	// Set a marker var so the inherit case has something visible to find.
	if os.Getenv("RUNNER_DEMO_INHERITED") == "" {
		_ = os.Setenv("RUNNER_DEMO_INHERITED", "from-parent")
	}

	var env []string
	switch *mode {
	case "inherit":
		env = nil
	case "empty":
		env = []string{}
	case "custom":
		// Keep PATH so /usr/bin/env can be located by exec; add a marker.
		env = []string{
			"RUNNER_DEMO_CUSTOM=hello-from-config-env",
			"PATH=" + os.Getenv("PATH"),
		}
	default:
		log.Fatalf("unknown mode %q (want inherit|empty|custom)", *mode)
	}

	workspace, err := os.Getwd()
	if err != nil {
		log.Fatalf("getwd: %v", err)
	}

	adapter := echoAdapter{name: "env", binary: "/usr/bin/env"}
	if _, ok := adapter.Detect(); !ok {
		log.Fatalf("/usr/bin/env not found on this system")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fmt.Printf("=== mode=%s ===\n", *mode)
	cfg := runner.Config{
		Provider:  adapter,
		Workspace: workspace,
		Env:       env,
		OnEvent: func(ev runner.Event) {
			switch ev.Kind {
			case runner.EventProviderEvent:
				se, _ := ev.Payload["event"].(llmtypes.StreamEvent)
				// Only show RUNNER_DEMO_* + a couple common vars to keep
				// inherit-mode output short.
				if strings.HasPrefix(se.Content, "RUNNER_DEMO_") ||
					strings.HasPrefix(se.Content, "PATH=") ||
					strings.HasPrefix(se.Content, "HOME=") {
					fmt.Println(se.Content)
				}
			case runner.EventProcessExited:
				fmt.Printf("(exit code %v)\n", ev.Payload["exit_code"])
			}
		},
	}

	if err := runner.Run(ctx, cfg); err != nil {
		log.Printf("run: %v", err)
	}
}
