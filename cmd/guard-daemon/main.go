package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"guard-daemon/internal/config"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runtimeConfig, err := config.LoadLegacy()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка конфигурации: guard-daemon не запущен; проверьте обязательные переменные окружения.")
		stop()
		os.Exit(1)
	}

	observer := newSafeConsoleObserver(os.Stdout)
	daemon, err := newDaemon(runtimeConfig, newProductionDependencies(observer))
	if err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка запуска: guard-daemon не смог безопасно подготовить зависимости.")
		stop()
		os.Exit(1)
	}

	if err := daemon.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка работы: guard-daemon остановлен из-за внутреннего сбоя.")
		stop()
		os.Exit(1)
	}
}
