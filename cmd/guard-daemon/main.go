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

const commandHelp = `Использование: guard-daemon [--help | --version]

Без аргументов guard-daemon загружает конфигурацию и запускает процесс.
  --help     показать эту справку без чтения конфигурации и сетевых вызовов
  --version  показать commit бинарного файла или development без чтения конфигурации
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
	_, _ = io.WriteString(stderr, "Ошибка: guard-daemon принимает только аргументы --help или --version.\n")
	return 2
}

func runProcess() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runtimeConfig, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка конфигурации: %v.\n", err)
		stop()
		return 1
	}

	observer := newSafeConsoleObserver(os.Stdout)
	daemon, err := newDaemon(ctx, runtimeConfig, newProductionDependencies(observer, runtimeConfig.Mode))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка запуска: %s.\n", startupOperatorMessage(err))
		stop()
		return 1
	}

	if err := daemon.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка работы: guard-daemon остановлен из-за внутреннего сбоя.")
		stop()
		return 1
	}
	return 0
}

func startupOperatorMessage(err error) string {
	var classified *domain.ClassifiedError
	if !errors.As(err, &classified) {
		return "guard-daemon не смог безопасно подготовить зависимости"
	}
	switch classified.Operation {
	case "daemon.manifest":
		return "не удалось доверенно загрузить deployment manifest и canonical artifact"
	case "daemon.restore_marker":
		return "восстановленное состояние запрещено использовать для подписания"
	case "daemon.attestation":
		return "RPC quorum не подтвердил deployment на общем finalized block"
	case "daemon.signers", "daemon.signer_address":
		return "адрес подписывающего компонента не совпал с настроенной ролью"
	case "daemon.lease_owner", "daemon.lease_acquire", "daemon.fence_acquire":
		return "не удалось получить исключительный lease для сети и sponsor"
	case "daemon.networks", "daemon.config":
		return "проверенная конфигурация не содержит допустимой включённой сети"
	default:
		return "guard-daemon не смог безопасно подготовить зависимости"
	}
}
