package rpc

import (
	"errors"
	"sync"
)

var ErrIncompleteClient = errors.New("RPC client is incomplete")

type ClientParts struct {
	Reader Reader
	Logs   LogSubscriber
	Heads  HeadSubscriber
	Closer Closer
}

type GenerationClient struct {
	generation uint64
	reader     Reader
	logs       LogSubscriber
	heads      HeadSubscriber
	closer     Closer
	closeOnce  sync.Once
}

func NewGenerationClient(generation uint64, parts ClientParts) (*GenerationClient, error) {
	if parts.Reader == nil || parts.Logs == nil || parts.Heads == nil || parts.Closer == nil {
		return nil, ErrIncompleteClient
	}

	return &GenerationClient{
		generation: generation,
		reader:     parts.Reader,
		logs:       parts.Logs,
		heads:      parts.Heads,
		closer:     parts.Closer,
	}, nil
}

func (client *GenerationClient) Generation() uint64 {
	return client.generation
}

func (client *GenerationClient) Reader() Reader {
	return client.reader
}

func (client *GenerationClient) LogSubscriber() LogSubscriber {
	return client.logs
}

func (client *GenerationClient) HeadSubscriber() HeadSubscriber {
	return client.heads
}

func (client *GenerationClient) Close() {
	client.closeOnce.Do(client.closer.Close)
}
