package rpc_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"guard-daemon/internal/rpc"

	"github.com/ethereum/go-ethereum/common"
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
	if errors.Unwrap(err) != nil {
		t.Fatalf("DialContext() exposed raw error through Unwrap: %v", errors.Unwrap(err))
	}
}

func TestDialQuorumRedactsEndpointFromError(t *testing.T) {
	t.Parallel()

	endpoints := []rpc.ProviderEndpoint{
		{
			Identity: rpc.ProviderIdentity{ID: "first", Fingerprint: "first-fingerprint", TrustDomain: "first.invalid"},
			Endpoint: "unsupported-test://first.invalid/private-credential",
		},
		{
			Identity: rpc.ProviderIdentity{ID: "second", Fingerprint: "second-fingerprint", TrustDomain: "second.invalid"},
			Endpoint: "unsupported-test://second.invalid/private-credential",
		},
	}
	_, err := rpc.DialQuorum(context.Background(), endpoints, time.Second)
	if !errors.Is(err, rpc.ErrQuorumDial) {
		t.Fatalf("DialQuorum() error = %v", err)
	}
	for _, endpoint := range endpoints {
		if strings.Contains(err.Error(), endpoint.Endpoint) || strings.Contains(err.Error(), "private-credential") {
			t.Fatalf("DialQuorum() exposed endpoint: %q", err)
		}
	}
}

func TestEthClientRejectsOversizedHTTPResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"jsonrpc":"2.0","id":1,"result":"0x`)
		chunk := strings.Repeat("a", 1<<20)
		for range 9 {
			if _, err := io.WriteString(response, chunk); err != nil {
				return
			}
		}
		_, _ = io.WriteString(response, `"}`)
	}))
	defer server.Close()

	client, err := (rpc.EthClientDialer{}).DialContext(context.Background(), server.URL, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Reader().CodeAt(context.Background(), common.Address{}, nil); err == nil {
		t.Fatal("oversized HTTP response принят")
	}
}

func TestDialEthClientRedactsEndpointFromError(t *testing.T) {
	t.Parallel()

	const endpoint = "http://deterministic.invalid/private-credential%zz"
	_, err := rpc.DialEthClient(context.Background(), endpoint)
	if err == nil {
		t.Fatalf("DialEthClient() error = %v", err)
	}
	if strings.Contains(err.Error(), endpoint) || strings.Contains(err.Error(), "private-credential") {
		t.Fatalf("DialEthClient() exposed endpoint: %q", err)
	}
	if errors.Unwrap(err) != nil {
		t.Fatalf("DialEthClient() exposed raw error through Unwrap: %v", errors.Unwrap(err))
	}
}
