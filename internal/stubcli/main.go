// stubcli is a deterministic CLI used by go-runner's e2e tests. It writes
// a fixed number of NDJSON events to stdout (delta * N, then done) and
// exits cleanly. It accepts but ignores stdin; arguments are parsed as
// flags.
//
// Flags:
//
//	-count        N   number of delta events to emit (default 3)
//	-fail             exit with status 2 after writing events
//	-stderr-msg   S   write S to stderr before the stdout stream
//	-sleep        D   after emitting events, sleep for D before exiting
//	                  (used by go-runner supervision tests)
//	-trap-sigterm     install a SIGTERM handler that ignores the signal
//	                  (used to test the SIGTERM->WaitDelay->SIGKILL path)
//	-burn-cpu     D   after emitting events, busy-loop for D before exit
//	                  (used to test resource limits)
//	-malloc-mb    N   after emitting events, allocate N MiB and hold it
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"
)

func main() {
	count := flag.Int("count", 3, "number of delta events to emit")
	fail := flag.Bool("fail", false, "exit with status 2 after writing events")
	stderrMsg := flag.String("stderr-msg", "", "if non-empty, write this line to stderr before the stdout stream")
	sleepDur := flag.Duration("sleep", 0, "after emitting events, sleep for this duration before exit")
	trapSigterm := flag.Bool("trap-sigterm", false, "install a SIGTERM handler that ignores the signal")
	burnCPU := flag.Duration("burn-cpu", 0, "after emitting events, busy-loop for this duration before exit")
	mallocMB := flag.Int("malloc-mb", 0, "after emitting events, allocate this many MiB and hold it")
	flag.Parse()

	if *trapSigterm {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM)
		go func() {
			for range ch {
				// swallow
			}
		}()
	}

	if *stderrMsg != "" {
		fmt.Fprintln(os.Stderr, *stderrMsg)
	}

	for i := 0; i < *count; i++ {
		fmt.Fprintf(os.Stdout, "{\"type\":\"delta\",\"content\":\"chunk-%d \"}\n", i)
	}
	fmt.Fprintln(os.Stdout, "{\"type\":\"done\"}")

	if *mallocMB > 0 {
		// Allocate a single contiguous slab; touch each page so the kernel
		// actually backs it (RLIMIT_AS / cgroup MemoryMax fires here, not
		// at allocation time).
		buf := make([]byte, *mallocMB*1024*1024)
		const pageSize = 4096
		for i := 0; i < len(buf); i += pageSize {
			buf[i] = 1
		}
		runtime.KeepAlive(buf)
	}

	if *burnCPU > 0 {
		deadline := time.Now().Add(*burnCPU)
		var x uint64
		for time.Now().Before(deadline) {
			for i := 0; i < 1_000_000; i++ {
				x ^= uint64(i)
			}
		}
		runtime.KeepAlive(x)
	}

	if *sleepDur > 0 {
		time.Sleep(*sleepDur)
	}

	if *fail {
		os.Exit(2)
	}
}
