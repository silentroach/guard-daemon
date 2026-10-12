package watcher

import (
	"context"
	"math/big"
	"testing"

	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"
	"guard-daemon/internal/rpc"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func FuzzMalformedLog(f *testing.F) {
	f.Add([]byte{1, 2, 3}, uint8(0), false, false, false, false)
	f.Add(make([]byte, common.HashLength), uint8(3), true, true, true, true)
	f.Fuzz(func(t *testing.T, data []byte, topicCount uint8, validEvent, validSource, validIdentity, removed bool) {
		if len(data) > 4096 {
			t.Skip()
		}
		source := testAddress(1)
		network := domain.Network{Name: "fuzz", ChainID: 31337, AllowUnknownTokens: true}
		service := &Service{
			codec: newTestCodec(t), source: source, networkID: network.ChainID,
			allowedTokens: map[common.Address]struct{}{}, allowUnknown: true,
			knownTokens: map[common.Address]domain.Token{}, metadata: map[common.Address]domain.Token{},
		}
		count := int(topicCount % 8)
		topics := make([]common.Hash, count)
		for index := range topics {
			start := index * common.HashLength
			if start < len(data) {
				copy(topics[index][:], data[start:])
			}
		}
		if count >= 3 && validEvent {
			topics[0] = service.codec.TransferTopic()
		}
		if count >= 3 && validSource {
			topics[2] = common.BytesToHash(source.Bytes())
		}
		address := common.BytesToAddress(data)
		blockHash := common.BytesToHash(data)
		txHash := common.BytesToHash(data)
		if validIdentity {
			address = testAddress(2)
			blockHash = testHash(3)
			txHash = testHash(4)
		}
		entry := types.Log{
			Address: address, Topics: topics, Data: append([]byte(nil), data...),
			BlockNumber: uint64(len(data)), BlockHash: blockHash, TxHash: txHash,
			Removed: removed,
		}
		candidate, accepted := service.logCandidate(context.Background(), entry, false)
		expected := count == 3 && len(data) == common.HashLength && validEvent && validSource && address != (common.Address{}) &&
			blockHash != (common.Hash{}) && txHash != (common.Hash{})
		if accepted != expected {
			t.Fatalf("log acceptance result = %v, want %v", accepted, expected)
		}
		if accepted && domain.ValidateCandidate(candidate) != nil {
			t.Fatal("corrupted candidate accepted")
		}
	})
}

func FuzzMetadataReturnData(f *testing.F) {
	f.Add([]byte{})
	f.Add(make([]byte, 32))
	f.Fuzz(func(t *testing.T, response []byte) {
		if len(response) > metadataReturnLimit+64 {
			t.Skip()
		}
		caller := &fuzzCaller{response: append([]byte(nil), response...)}
		service := &Service{
			contracts: caller, codec: newTestCodec(t), readTimeout: 1,
			knownTokens: map[common.Address]domain.Token{}, metadata: map[common.Address]domain.Token{},
			observer: observability.Discard{},
		}
		_ = service.resolveToken(context.Background(), testAddress(2))
		if len(service.metadata) > metadataCacheLimit {
			t.Fatal("metadata cache exceeded its limit")
		}
	})
}

type fuzzCaller struct{ response []byte }

func (caller *fuzzCaller) CallContract(context.Context, ethereum.CallMsg, *big.Int) ([]byte, error) {
	return append([]byte(nil), caller.response...), nil
}

var _ rpc.ContractCaller = (*fuzzCaller)(nil)
