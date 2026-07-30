package rpc

import (
	"errors"
	"sync"
)

var ErrIncompleteClient = errors.New("incomplete RPC client")

type ClientParts struct {
	Reader      Reader
	Logs        LogSubscriber
	Heads       HeadSubscriber
	Broadcaster Broadcaster
	Closer      Closer
}

type GenerationClient struct {
	generation  uint64
	reader      Reader
	logs        LogSubscriber
	heads       HeadSubscriber
	broadcaster Broadcaster
	closer      Closer
	closeOnce   sync.Once
}

func NewGenerationClient(generation uint64, parts ClientParts) (*GenerationClient, error) {
	if parts.Reader == nil || parts.Logs == nil || parts.Heads == nil || parts.Broadcaster == nil || parts.Closer == nil {
		return nil, ErrIncompleteClient
	}

	return &GenerationClient{
		generation:  generation,
		reader:      parts.Reader,
		logs:        parts.Logs,
		heads:       parts.Heads,
		broadcaster: parts.Broadcaster,
		closer:      parts.Closer,
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

func (client *GenerationClient) Broadcaster() Broadcaster {
	return client.broadcaster
}

func (client *GenerationClient) Close() {
	client.closeOnce.Do(client.closer.Close)
}
