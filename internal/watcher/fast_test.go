package watcher

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"guard-daemon/internal/domain"
	"guard-daemon/internal/rpc"
	"guard-daemon/internal/store"

	"github.com/ethereum/go-ethereum/core/types"
)

type separatedHeads struct {
	*fakeFinalized
	finalized uint64
}

type slowFinalized struct{ *fakeFinalized }

func (reader *slowFinalized) Latest(ctx context.Context) (rpc.BlockRef, error) {
	return reader.fakeFinalized.Finalized(ctx)
}
func (reader *slowFinalized) Finalized(ctx context.Context) (rpc.BlockRef, error) {
	<-ctx.Done()
	return rpc.BlockRef{}, ctx.Err()
}

func TestFastReadinessDoesNotWaitForFinalizedBackfill(t *testing.T) {
	source := testAddress(1)
	network := domain.Network{Name: "local", ChainID: 31337, AllowUnknownTokens: true}
	reader := &slowFinalized{fakeFinalized: newFakeFinalized()}
	reader.addBlock(10, testHash(10), testHash(9))
	handoff := openWatchStore(t, filepath.Join(t.TempDir(), "watcher.db"), source, network, 1)
	defer handoff.Close()
	service := newTestService(t, reader, handoff, source, network, 1, &fakeContractCaller{}, newFakeLogSubscriber(), newFakeHeadSubscriber(), true)
	ready := make(chan struct{})
	service.ready = func() error { close(ready); return nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- service.Run(ctx) }()
	receive(t, ready)
	cancel()
	if err := receive(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown: %v", err)
	}
}

func (reader *separatedHeads) Latest(ctx context.Context) (rpc.BlockRef, error) {
	return reader.fakeFinalized.Finalized(ctx)
}
func (reader *separatedHeads) Finalized(ctx context.Context) (rpc.BlockRef, error) {
	return reader.Header(ctx, reader.finalized)
}

func TestFastUnknownDepositDoesNotWaitForFinalityOrCorruptCheckpointOnReorg(t *testing.T) {
	ctx := context.Background()
	source, token := testAddress(1), testAddress(2)
	network := domain.Network{Name: "local", ChainID: 31337, AllowUnknownTokens: true}
	reader := &separatedHeads{fakeFinalized: newFakeFinalized(), finalized: 100}
	reader.addBlock(100, testHash(10), testHash(9))
	entry := matchingLog(newTestCodec(t), source, token, 101, testHash(11), 0)
	reader.addBlockWithLogs(101, testHash(11), testHash(10), []types.Log{entry})
	path := filepath.Join(t.TempDir(), "watcher.db")
	handoff := openWatchStore(t, path, source, network, 2)
	caller := &fakeContractCaller{}
	service := newTestService(t, reader, handoff, source, network, 2, caller, newFakeLogSubscriber(), newFakeHeadSubscriber(), true)
	service.dryRun = true
	if err := service.scanFast(ctx); err != nil {
		t.Fatal(err)
	}
	dry := domain.NewFastTokenCandidate(network.ChainID, source, domain.Token{Address: token}, entry.BlockHash, entry.TxHash, 101, 0, true)
	incident := store.Incident{ID: domain.NewIncidentID(dry.ID), Candidate: dry.ID, Network: network.ChainID, CreatedAt: time.Now()}
	if err := handoff.PutIncident(ctx, incident); err != nil {
		t.Fatal(err)
	}
	if err := handoff.Ack(ctx, dry.ID, incident.ID); err != nil {
		t.Fatal(err)
	}
	service = newTestService(t, reader, handoff, source, network, 2, caller, newFakeLogSubscriber(), newFakeHeadSubscriber(), true)
	if err := service.scanFast(ctx); err != nil {
		t.Fatal(err)
	}
	fast := domain.NewFastTokenCandidate(network.ChainID, source, domain.Token{Address: token}, entry.BlockHash, entry.TxHash, 101, 0, false)
	assertCandidateIDs(t, handoff, network.ChainID, fast.ID)
	if _, found, err := handoff.LoadScanCursor(ctx, network.ChainID); err != nil || found {
		t.Fatal("fast path advanced finalized cursor")
	}
	if caller.calls.Load() != 0 {
		t.Fatal("token metadata RPC blocked fast discovery")
	}
	if err := handoff.Close(); err != nil {
		t.Fatal(err)
	}
	handoff = openWatchStore(t, path, source, network, 2)
	service = newTestService(t, reader, handoff, source, network, 2, caller, newFakeLogSubscriber(), newFakeHeadSubscriber(), true)
	if err := service.scanFast(ctx); err != nil {
		t.Fatal(err)
	}
	assertCandidateIDs(t, handoff, network.ChainID, fast.ID)
	// Replace the early block before finality. Its pending balance-check hint
	// must not conflict with the authoritative scanner's new canonical branch.
	reader.addBlockWithLogs(101, testHash(12), testHash(10), nil)
	reader.finalized = 101
	if err := service.scanFast(ctx); err != nil {
		t.Fatal(err)
	}
	if err := service.scan(ctx); err != nil {
		t.Fatal(err)
	}
	assertCursor(t, handoff, network.ChainID, 101, testHash(12))
	if err := handoff.Close(); err != nil {
		t.Fatal(err)
	}
	handoff = openWatchStore(t, path, source, network, 2)
	defer handoff.Close()
	assertCursor(t, handoff, network.ChainID, 101, testHash(12))
}
