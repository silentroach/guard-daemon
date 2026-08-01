package main

import (
	"errors"
	"strings"
	"testing"

	"guard-daemon/internal/domain"
)

func TestStartupOperatorMessageIsSpecificAndRedacted(t *testing.T) {
	const privateDetail = "private-rpc-credential-canary"
	tests := []struct {
		operation string
		contains  string
	}{
		{operation: "daemon.manifest", contains: "deployment manifest"},
		{operation: "daemon.attestation", contains: "RPC quorum"},
		{operation: "daemon.signer_address", contains: "подписывающего компонента"},
	}
	for _, test := range tests {
		t.Run(test.operation, func(t *testing.T) {
			err := domain.NewError(test.operation, domain.ErrorConfiguration, errorStartupInvalid, false, false, errors.New(privateDetail))
			message := startupOperatorMessage(err)
			if !strings.Contains(message, test.contains) {
				t.Fatalf("message = %q", message)
			}
			if strings.Contains(message, privateDetail) {
				t.Fatalf("message раскрывает cause: %q", message)
			}
		})
	}
}
