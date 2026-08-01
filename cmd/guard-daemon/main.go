package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"guard-daemon/internal/config"
	"guard-daemon/internal/domain"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runtimeConfig, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка конфигурации: %v.\n", err)
		stop()
		os.Exit(1)
	}

	observer := newSafeConsoleObserver(os.Stdout)
	daemon, err := newDaemon(ctx, runtimeConfig, newProductionDependencies(observer, runtimeConfig.Mode))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка запуска: %s.\n", startupOperatorMessage(err))
		stop()
		os.Exit(1)
	}

	if err := daemon.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка работы: guard-daemon остановлен из-за внутреннего сбоя.")
		stop()
		os.Exit(1)
	}
}

func startupOperatorMessage(err error) string {
	var classified *domain.ClassifiedError
	if !errors.As(err, &classified) {
		return "guard-daemon не смог безопасно подготовить зависимости"
	}
	switch classified.Operation {
	case "daemon.manifest":
		return "не удалось доверенно загрузить deployment manifest и canonical artifact"
	case "daemon.attestation":
		return "RPC quorum не подтвердил deployment на общем finalized block"
	case "daemon.signers", "daemon.signer_address":
		return "адрес подписывающего компонента не совпал с настроенной ролью"
	case "daemon.networks", "daemon.config":
		return "проверенная конфигурация не содержит допустимой включённой сети"
	default:
		return "guard-daemon не смог безопасно подготовить зависимости"
	}
}
