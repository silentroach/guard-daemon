package main

import (
	"io"

	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"
)

type safeConsoleObserver struct {
	console observability.Observer
}

func newSafeConsoleObserver(writer io.Writer) observability.Observer {
	if writer == nil {
		writer = io.Discard
	}
	return &safeConsoleObserver{console: observability.NewConsole(writer)}
}

func (observer *safeConsoleObserver) Record(event observability.Event) {
	observer.console.Record(observability.Event{
		Level:       event.Level,
		Code:        observability.EventCode(safeLabel(string(event.Code), "event_redacted")),
		NetworkName: safeLabel(event.NetworkName, "network_redacted"),
		ErrorCode:   domain.ErrorCode(safeOptionalLabel(string(event.ErrorCode))),
	})
}

func safeOptionalLabel(value string) string {
	if value == "" {
		return ""
	}
	return safeLabel(value, "error_redacted")
}

func safeLabel(value, fallback string) string {
	if value == "" || len(value) > 64 {
		return fallback
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '_' || character == '-' || character == '.' {
			continue
		}
		return fallback
	}
	return value
}

var _ observability.Observer = (*safeConsoleObserver)(nil)
