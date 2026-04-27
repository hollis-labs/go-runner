// stubcli is a deterministic CLI used by go-runner's e2e tests. It writes
// a fixed number of NDJSON events to stdout (delta * N, then done) and
// exits cleanly. It accepts but ignores stdin; arguments are parsed as
// flags (-count for delta count, -fail to exit non-zero after writing,
// -stderr-msg to emit one line to stderr before the stdout stream).
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	count := flag.Int("count", 3, "number of delta events to emit")
	fail := flag.Bool("fail", false, "exit with status 2 after writing events")
	stderrMsg := flag.String("stderr-msg", "", "if non-empty, write this line to stderr before the stdout stream")
	flag.Parse()

	if *stderrMsg != "" {
		fmt.Fprintln(os.Stderr, *stderrMsg)
	}

	for i := 0; i < *count; i++ {
		fmt.Fprintf(os.Stdout, "{\"type\":\"delta\",\"content\":\"chunk-%d \"}\n", i)
	}
	fmt.Fprintln(os.Stdout, "{\"type\":\"done\"}")

	if *fail {
		os.Exit(2)
	}
}
