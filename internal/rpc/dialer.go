package rpc

import (
	"context"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/ethclient"
)

const dialFailed domain.ErrorCode = "rpc_dial_failed"

type Dialer interface {
	DialContext(context.Context, string, uint64) (*GenerationClient, error)
}

type EthClientDialer struct{}

type readerFacade struct{ Reader }
type logSubscriberFacade struct{ LogSubscriber }
type headSubscriberFacade struct{ HeadSubscriber }

func (EthClientDialer) DialContext(ctx context.Context, endpoint string, generation uint64) (*GenerationClient, error) {
	backend, err := ethclient.DialContext(ctx, endpoint)
	if err != nil {
		return nil, domain.NewError("rpc.dial", domain.ErrorRPCTransient, dialFailed, true, false, err)
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

var _ Dialer = EthClientDialer{}
