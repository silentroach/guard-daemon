package rpc_test

import (
	"context"
	"strings"
	"testing"

	"guard-daemon/internal/rpc"
)

func TestEthClientDialerRedactsEndpointFromError(t *testing.T) {
	t.Parallel()

	const endpoint = "unsupported-test://deterministic.invalid/non-secret-marker"
	_, err := (rpc.EthClientDialer{}).DialContext(context.Background(), endpoint, 1)
	if err == nil {
		t.Fatal("DialContext() error = nil")
	}
	if strings.Contains(err.Error(), endpoint) || strings.Contains(err.Error(), "deterministic.invalid") {
		t.Fatalf("DialContext() exposed endpoint: %q", err)
	}
}
