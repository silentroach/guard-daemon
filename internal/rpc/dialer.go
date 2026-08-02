package rpc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/ethclient"
	gethrpc "github.com/ethereum/go-ethereum/rpc"
)

const (
	dialFailed          domain.ErrorCode = "rpc_dial_failed"
	maxRPCResponseBytes                  = int64(8 << 20)
)

var errRPCResponseTooLarge = errors.New("RPC response превышает допустимый размер")

type Dialer interface {
	DialContext(context.Context, string, uint64) (*GenerationClient, error)
}

type EthClientDialer struct{}

type readerFacade struct{ Reader }
type logSubscriberFacade struct{ LogSubscriber }
type headSubscriberFacade struct{ HeadSubscriber }
type historicalReaderFacade struct{ HistoricalReader }

func (EthClientDialer) DialContext(ctx context.Context, endpoint string, generation uint64) (*GenerationClient, error) {
	backend, err := dialEthClient(ctx, endpoint)
	if err != nil {
		return nil, domain.NewError("rpc.dial", domain.ErrorRPCTransient, dialFailed, true, false, nil)
	}

	client, err := NewGenerationClient(generation, ClientParts{
		Reader: &readerFacade{Reader: backend},
		Logs:   &logSubscriberFacade{LogSubscriber: backend},
		Heads:  &headSubscriberFacade{HeadSubscriber: backend},
		Closer: backend,
	})
	if err != nil {
		backend.Close()
		return nil, domain.NewError("rpc.client", domain.ErrorInternal, "rpc_client_invalid", false, false, err)
	}
	return client, nil
}

func DialQuorum(ctx context.Context, endpoints []ProviderEndpoint, timeout time.Duration) (*QuorumReader, error) {
	return dialQuorum(ctx, endpoints, timeout, func(dialCtx context.Context, endpoint string) (HistoricalReader, func(), error) {
		backend, err := dialEthClient(dialCtx, endpoint)
		if err != nil {
			return nil, nil, err
		}
		return &historicalReaderFacade{HistoricalReader: backend}, backend.Close, nil
	})
}

func dialEthClient(ctx context.Context, endpoint string) (*ethclient.Client, error) {
	httpClient := &http.Client{Transport: responseLimitTransport{base: http.DefaultTransport}}
	backend, err := gethrpc.DialOptions(
		ctx,
		endpoint,
		gethrpc.WithHTTPClient(httpClient),
		gethrpc.WithWebsocketMessageSizeLimit(maxRPCResponseBytes),
	)
	if err != nil {
		return nil, err
	}
	return ethclient.NewClient(backend), nil
}

type responseLimitTransport struct {
	base http.RoundTripper
}

func (transport responseLimitTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.ContentLength > maxRPCResponseBytes {
		_ = response.Body.Close()
		return nil, errRPCResponseTooLarge
	}
	response.Body = http.MaxBytesReader(nil, response.Body, maxRPCResponseBytes)
	return response, nil
}

type quorumDialFunc func(context.Context, string) (HistoricalReader, func(), error)

func dialQuorum(ctx context.Context, endpoints []ProviderEndpoint, timeout time.Duration, dial quorumDialFunc) (*QuorumReader, error) {
	if timeout <= 0 {
		return nil, ErrInvalidTimeout
	}
	identities := make([]ProviderIdentity, len(endpoints))
	seenEndpoints := make(map[string]struct{}, len(endpoints))
	for index, endpoint := range endpoints {
		identities[index] = endpoint.Identity
		canonicalEndpoint := strings.TrimSpace(endpoint.Endpoint)
		if canonicalEndpoint == "" {
			return nil, ErrInvalidProviders
		}
		if _, exists := seenEndpoints[canonicalEndpoint]; exists {
			return nil, ErrInvalidProviders
		}
		seenEndpoints[canonicalEndpoint] = struct{}{}
	}
	if err := validateProviderIdentities(identities); err != nil {
		return nil, err
	}

	providers := make([]Provider, 0, len(endpoints))
	cleanup := func() {
		for _, provider := range providers {
			provider.Close()
		}
	}
	for _, endpoint := range endpoints {
		dialCtx, cancel := context.WithTimeout(ctx, timeout)
		backend, closeBackend, err := dial(dialCtx, endpoint.Endpoint)
		dialContextErr := dialCtx.Err()
		cancel()
		if err != nil || dialContextErr != nil || backend == nil || closeBackend == nil {
			if closeBackend != nil {
				closeBackend()
			}
			cleanup()
			return nil, redactedDialError(ctx, err, dialContextErr)
		}
		providers = append(providers, Provider{
			Identity: endpoint.Identity,
			Reader:   backend,
			Close:    closeBackend,
		})
	}

	reader, err := NewQuorumReader(providers, timeout)
	if err != nil {
		cleanup()
		return nil, err
	}
	return reader, nil
}

func redactedDialError(parent context.Context, backendErr, dialContextErr error) error {
	if parent.Err() != nil {
		return fmt.Errorf("%w: %w", ErrQuorumDial, parent.Err())
	}
	if errors.Is(dialContextErr, context.DeadlineExceeded) || errors.Is(backendErr, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrQuorumDial, context.DeadlineExceeded)
	}
	return ErrQuorumDial
}

var _ Dialer = EthClientDialer{}
