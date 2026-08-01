package rpc

import (
	"testing"

	"github.com/ethereum/go-ethereum/ethclient"
)

func TestReadOnlyFacadesDoNotExposeBroadcastCapability(t *testing.T) {
	backend := (*ethclient.Client)(nil)
	if _, ok := any(backend).(Broadcaster); !ok {
		t.Fatal("test backend должен иметь broadcast capability")
	}

	parts := ClientParts{
		Reader: &readerFacade{Reader: backend},
		Logs:   &logSubscriberFacade{LogSubscriber: backend},
		Heads:  &headSubscriberFacade{HeadSubscriber: backend},
		Closer: closeFunc(func() {}),
	}
	client, err := NewGenerationClient(1, parts)
	if err != nil {
		t.Fatal(err)
	}
	for name, capability := range map[string]any{
		"reader": client.Reader(),
		"logs":   client.LogSubscriber(),
		"heads":  client.HeadSubscriber(),
	} {
		if _, ok := capability.(Broadcaster); ok {
			t.Fatalf("%s facade раскрывает broadcast capability", name)
		}
	}
}

type closeFunc func()

func (close closeFunc) Close() { close() }
