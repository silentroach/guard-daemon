//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDisableProcessDumps(t *testing.T) {
	if os.Getenv("GUARD_DAEMON_TEST_DUMP_PROTECTION") == "1" {
		if err := disableProcessDumps(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		var limit unix.Rlimit
		if err := unix.Getrlimit(unix.RLIMIT_CORE, &limit); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		dumpable, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
		if err != nil || limit.Cur != 0 || limit.Max != 0 || dumpable != 0 {
			fmt.Fprintf(os.Stderr, "dump protection: limit=%d:%d dumpable=%d error=%v\n", limit.Cur, limit.Max, dumpable, err)
			os.Exit(2)
		}
		os.Exit(0)
	}

	command := exec.Command(os.Args[0], "-test.run=^TestDisableProcessDumps$")
	command.Env = append(os.Environ(), "GUARD_DAEMON_TEST_DUMP_PROTECTION=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("dump protection subprocess: %v\n%s", err, output)
	}
}
