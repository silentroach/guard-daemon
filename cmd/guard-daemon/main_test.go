package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"guard-daemon/internal/buildinfo"
	"guard-daemon/internal/domain"
)

func TestCLIHelpDoesNotStartDaemon(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	started := false
	code := runCLI([]string{"--help"}, &stdout, &stderr, func() int {
		started = true
		return 1
	})
	if code != 0 || started || stderr.Len() != 0 {
		t.Fatalf("help: code=%d started=%t stderr=%q", code, started, stderr.String())
	}
	if stdout.String() != commandHelp || !strings.Contains(stdout.String(), "without loading configuration") {
		t.Fatalf("unexpected help output: %q", stdout.String())
	}
}

func TestCLIRejectsUnknownArgumentsBeforeStartup(t *testing.T) {
	var stderr bytes.Buffer
	started := false
	code := runCLI([]string{"--unknown"}, &bytes.Buffer{}, &stderr, func() int {
		started = true
		return 0
	})
	if code != 2 || started || stderr.String() != "Error: guard-daemon accepts only --help or --version.\n" {
		t.Fatalf("unknown argument: code=%d started=%t stderr=%q", code, started, stderr.String())
	}
}

func TestCLIVersionDoesNotStartDaemonAndRedactsInvalidIdentity(t *testing.T) {
	previous := buildinfo.ReleaseCommit
	t.Cleanup(func() { buildinfo.ReleaseCommit = previous })

	for _, test := range []struct {
		name     string
		embedded string
		want     string
	}{
		{name: "development", want: "development\n"},
		{name: "release", embedded: strings.Repeat("a", 40), want: strings.Repeat("a", 40) + "\n"},
		{name: "invalid identity", embedded: "private-build-detail\n", want: "development\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			buildinfo.ReleaseCommit = test.embedded
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			started := false
			code := runCLI([]string{"--version"}, &stdout, &stderr, func() int {
				started = true
				return 1
			})
			if code != 0 || started || stderr.Len() != 0 || stdout.String() != test.want {
				t.Fatalf("version: code=%d started=%t stdout=%q stderr=%q", code, started, stdout.String(), stderr.String())
			}
		})
	}
}

func TestCLIWithoutArgumentsStartsDaemon(t *testing.T) {
	started := false
	code := runCLI(nil, &bytes.Buffer{}, &bytes.Buffer{}, func() int {
		started = true
		return 7
	})
	if code != 7 || !started {
		t.Fatalf("startup: code=%d started=%t", code, started)
	}
}

func TestStartupOperatorMessageIsSpecificAndRedacted(t *testing.T) {
	const privateDetail = "private-rpc-credential-canary"
	tests := []struct {
		operation string
		contains  string
	}{
		{operation: "daemon.manifest", contains: "deployment manifest"},
		{operation: "daemon.restore_marker", contains: "restored state"},
		{operation: "daemon.attestation", contains: "RPC quorum"},
		{operation: "daemon.signer_address", contains: "signer address"},
		{operation: "daemon.lease_acquire", contains: "exclusive lease"},
		{operation: "daemon.fence_acquire", contains: "exclusive lease"},
	}
	for _, test := range tests {
		t.Run(test.operation, func(t *testing.T) {
			err := domain.NewError(test.operation, domain.ErrorConfiguration, errorStartupInvalid, false, false, errors.New(privateDetail))
			message := startupOperatorMessage(err)
			if !strings.Contains(message, test.contains) {
				t.Fatalf("message = %q", message)
			}
			if strings.Contains(message, privateDetail) {
				t.Fatalf("message exposed its cause: %q", message)
			}
		})
	}
}
