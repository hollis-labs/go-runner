// stubcli is a deterministic CLI used by go-runner's e2e tests. It writes
// a fixed number of NDJSON events to stdout (delta * N, then done) and
// exits cleanly. It accepts but ignores stdin; arguments are parsed as
// flags (-count for delta count, -fail to exit non-zero after writing).
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	count := flag.Int("count", 3, "number of delta events to emit")
	fail := flag.Bool("fail", false, "exit with status 2 after writing events")
	flag.Parse()

	for i := 0; i < *count; i++ {
		fmt.Fprintf(os.Stdout, "{\"type\":\"delta\",\"content\":\"chunk-%d \"}\n", i)
	}
	fmt.Fprintln(os.Stdout, "{\"type\":\"done\"}")

	if *fail {
		os.Exit(2)
	}
}
