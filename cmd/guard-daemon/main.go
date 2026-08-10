package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"guard-daemon/internal/buildinfo"
	"guard-daemon/internal/config"
	"guard-daemon/internal/domain"
)

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr, runProcess))
}

const commandHelp = `Usage: guard-daemon [--help | --version]

With no arguments, guard-daemon loads its configuration and starts the process.
  --help     show this help without loading configuration or making network requests
  --version  show the binary commit, or development, without loading configuration
`

func runCLI(args []string, stdout, stderr io.Writer, start func() int) int {
	if len(args) == 0 {
		return start()
	}
	if len(args) == 1 && args[0] == "--help" {
		_, _ = io.WriteString(stdout, commandHelp)
		return 0
	}
	if len(args) == 1 && args[0] == "--version" {
		_, _ = fmt.Fprintln(stdout, buildinfo.Version())
		return 0
	}
	_, _ = io.WriteString(stderr, "Error: guard-daemon accepts only --help or --version.\n")
	return 2
}

func runProcess() int {
	if err := disableProcessDumps(); err != nil {
		fmt.Fprintln(os.Stderr, "Startup error: failed to disable process core dumps.")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	secrets, err := loadSystemdCredentials()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Configuration error: failed to load credentials securely.")
		stop()
		return 1
	}
	runtimeConfig, err := config.LoadWithSecrets(secrets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Configuration error: %v.\n", err)
		stop()
		return 1
	}

	observer := newSafeConsoleObserver(os.Stdout)
	daemon, err := newDaemon(ctx, runtimeConfig, newProductionDependencies(observer, runtimeConfig.Mode))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Startup error: %s.\n", startupOperatorMessage(err))
		stop()
		return 1
	}

	if err := daemon.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "Runtime error: guard-daemon stopped due to an internal failure.")
		stop()
		return 1
	}
	return 0
}

func startupOperatorMessage(err error) string {
	var classified *domain.ClassifiedError
	if !errors.As(err, &classified) {
		return "guard-daemon could not prepare its dependencies safely"
	}
	switch classified.Operation {
	case "daemon.manifest":
		return "failed to load a trusted deployment manifest and canonical artifact"
	case "daemon.restore_marker":
		return "restored state cannot be used for signing"
	case "daemon.attestation":
		return "the RPC quorum did not confirm the deployment at a shared finalized block"
	case "daemon.signers", "daemon.signer_address":
		return "the signer address does not match the configured role"
	case "daemon.lease_owner", "daemon.lease_acquire", "daemon.fence_acquire":
		return "failed to acquire an exclusive lease for the network and sponsor"
	case "daemon.networks", "daemon.config":
		return "validated configuration does not contain an eligible enabled network"
	default:
		return "guard-daemon could not prepare its dependencies safely"
	}
}
