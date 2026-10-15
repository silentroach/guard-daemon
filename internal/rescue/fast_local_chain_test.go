package rescue

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"guard-daemon/internal/domain"
	"guard-daemon/internal/rpc"
	"guard-daemon/internal/store"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

type delayedFinality struct {
	*localChainFinality
	finalized      rpc.BlockRef
	orphan         uint64
	latestCalls    atomic.Int32
	finalizedCalls atomic.Int32
	nativeReads    atomic.Int32
	tokenReads     atomic.Int32
}

func (reader *delayedFinality) Latest(ctx context.Context) (rpc.BlockRef, error) {
	reader.latestCalls.Add(1)
	return reader.localChainFinality.Finalized(ctx)
}
func (reader *delayedFinality) Finalized(context.Context) (rpc.BlockRef, error) {
	reader.finalizedCalls.Add(1)
	return reader.finalized, nil
}
func (reader *delayedFinality) Header(ctx context.Context, number uint64) (rpc.BlockRef, error) {
	block, err := reader.localChainFinality.Header(ctx, number)
	if reader.orphan != 0 && number == reader.orphan {
		block.Hash = common.Hash{0xff}
	}
	return block, err
}
func (reader *delayedFinality) Receipt(ctx context.Context, hash common.Hash) (*types.Receipt, error) {
	receipt, err := reader.localChainFinality.Receipt(ctx, hash)
	if err == nil && receipt.BlockNumber.Uint64() > reader.finalized.Number {
		return nil, rpc.ErrUnfinalizedReceipt
	}
	return receipt, err
}
func (reader *delayedFinality) MinedReceipt(ctx context.Context, hash common.Hash) (*types.Receipt, error) {
	return reader.localChainFinality.Receipt(ctx, hash)
}
func (reader *delayedFinality) BalanceAt(ctx context.Context, block rpc.BlockRef, address common.Address) (*big.Int, error) {
	reader.nativeReads.Add(1)
	return reader.localChainFinality.BalanceAt(ctx, block, address)
}
func (reader *delayedFinality) CallContract(ctx context.Context, block rpc.BlockRef, call ethereum.CallMsg) ([]byte, error) {
	if len(call.Data) >= 4 && hexutil.Encode(call.Data[:4]) == "0x70a08231" {
		reader.tokenReads.Add(1)
	}
	return reader.localChainFinality.CallContract(ctx, block, call)
}

type prefetchedPrimary struct {
	*localChainPrimary
	source                      common.Address
	sourceReads, sponsorReads   atomic.Int32
	headerStarted, priceStarted chan struct{}
	headerOnce, priceOnce       sync.Once
}

type acceptedWithoutInclusion struct{ transactions []*types.Transaction }

func (sender *acceptedWithoutInclusion) SendTransaction(_ context.Context, transaction *types.Transaction) error {
	sender.transactions = append(sender.transactions, transaction)
	return nil
}

func (reader *prefetchedPrimary) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	reader.headerOnce.Do(func() { close(reader.headerStarted) })
	select {
	case <-reader.priceStarted:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return reader.localChainPrimary.HeaderByNumber(ctx, number)
}
func (reader *prefetchedPrimary) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	reader.priceOnce.Do(func() { close(reader.priceStarted) })
	select {
	case <-reader.headerStarted:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return reader.localChainPrimary.SuggestGasPrice(ctx)
}
func (reader *prefetchedPrimary) PendingNonceAt(ctx context.Context, address common.Address) (uint64, error) {
	if address == reader.source {
		reader.sourceReads.Add(1)
	} else {
		reader.sponsorReads.Add(1)
	}
	return reader.localChainPrimary.PendingNonceAt(ctx, address)
}

func TestLocalChain_FastUnknownSweepPrefetchAndFinalAccounting(t *testing.T) {
	for _, name := range []string{"finalized result", "orphaned prestate still charges fee", "unmined accepted transaction is retried", "reorg before signing stops payment", "persisted pending after disabling latest"} {
		reorg := name == "orphaned prestate still charges fee"
		unmined := name == "unmined accepted transaction is retried"
		preSignReorg := name == "reorg before signing stops payment"
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			sourceKey := localChainPrivateKey(t, localChainSourceKeyHex)
			sponsorKey := localChainPrivateKey(t, localChainSponsorKeyHex)
			source, sponsor := crypto.PubkeyToAddress(sourceKey.PublicKey), crypto.PubkeyToAddress(sponsorKey.PublicKey)
			destination := crypto.PubkeyToAddress(localChainPrivateKey(t, localChainDestinationKeyHex).PublicKey)
			token := common.Address{0xa1}
			content, err := os.ReadFile("testdata/claim-contracts.json")
			if err != nil {
				t.Fatal(err)
			}
			var fixture map[string]string
			if err := json.Unmarshal(content, &fixture); err != nil {
				t.Fatal(err)
			}
			backend := localChainBackend(t, types.GenesisAlloc{
				source: {Balance: big.NewInt(1), Nonce: 7}, sponsor: {Balance: localChainEther(20)}, destination: {Balance: big.NewInt(1)},
				token: {Balance: new(big.Int), Code: hexutil.MustDecode(fixture["StandardToken"])},
			})
			client := backend.Client()
			_, rescuer, nonce := localChainDeployRescuer(t, ctx, backend, sponsorKey, destination)
			finality := &delayedFinality{localChainFinality: &localChainFinality{client: client}}
			finality.finalized, err = finality.localChainFinality.Finalized(ctx)
			if err != nil {
				t.Fatal(err)
			}
			mintABI, err := abi.JSON(strings.NewReader(`[{"type":"function","name":"mint","inputs":[{"type":"address"},{"type":"uint256"}],"outputs":[]}]`))
			if err != nil {
				t.Fatal(err)
			}
			data, err := mintABI.Pack("mint", source, big.NewInt(100))
			if err != nil {
				t.Fatal(err)
			}
			depositGas, err := client.EstimateGas(ctx, ethereum.CallMsg{From: sponsor, To: &token, Data: data, GasFeeCap: big.NewInt(3_000_000_000), GasTipCap: big.NewInt(1_000_000_000)})
			if err != nil {
				t.Fatal(err)
			}
			deposit := types.NewTx(&types.DynamicFeeTx{ChainID: big.NewInt(int64(localChainID)), Nonce: nonce, To: &token, Gas: depositGas, GasFeeCap: big.NewInt(3_000_000_000), GasTipCap: big.NewInt(1_000_000_000), Data: data})
			deposit, err = types.SignTx(deposit, types.LatestSignerForChainID(deposit.ChainId()), sponsorKey)
			if err != nil {
				t.Fatal(err)
			}
			if err := client.SendTransaction(ctx, deposit); err != nil {
				t.Fatal(err)
			}
			backend.Commit()
			depositReceipt, err := client.TransactionReceipt(ctx, deposit.Hash())
			if err != nil || depositReceipt.Status != types.ReceiptStatusSuccessful {
				t.Fatalf("deposit: %v, %v", depositReceipt, err)
			}
			coordinator, state := localChainCoordinator(t, sourceKey, sponsorKey, destination, rescuer, "fast-unknown")
			coordinator.latestState = true
			coordinator.network.AllowUnknownTokens = true
			primary := &prefetchedPrimary{localChainPrimary: &localChainPrimary{client: client}, source: source, headerStarted: make(chan struct{}), priceStarted: make(chan struct{})}
			broadcaster := &localChainBroadcaster{client: client, backend: backend}
			var sender rpc.Broadcaster = broadcaster
			accepted := &acceptedWithoutInclusion{}
			if unmined {
				sender = accepted
			}
			session, err := coordinator.NewSession(ctx, 1, primary, finality, sender)
			if err != nil {
				t.Fatal(err)
			}
			finality.finalizedCalls.Store(0)
			candidate := domain.NewFastTokenCandidate(localChainID, source, domain.Token{Address: token}, depositReceipt.BlockHash, deposit.Hash(), depositReceipt.BlockNumber.Uint64(), 0, false)
			if name == "persisted pending after disabling latest" {
				now := coordinator.clock.Now()
				_, err := state.PutRescueIncident(ctx, store.RescueIncident{
					ID: domain.NewAssetIncidentID(candidate.ID, domain.CandidateToken, token), Parent: domain.NewIncidentID(candidate.ID),
					Candidate: candidate.ID, Network: localChainID, Kind: domain.CandidateToken, Asset: token, Generation: 1,
					Policy: store.RescuePolicySnapshot{LatestState: true, MaxAttempts: 1, RetryDelay: time.Second, FinalityTimeout: time.Second},
					Status: store.RescuePending, CreatedAt: now, UpdatedAt: now,
				})
				if err != nil {
					t.Fatal(err)
				}
				coordinator.latestState = false
				session, err = coordinator.NewSession(ctx, 2, primary, finality, sender)
				if err != nil {
					t.Fatal(err)
				}
				finality.finalizedCalls.Store(0)
			}
			if preSignReorg {
				finality.orphan = depositReceipt.BlockNumber.Uint64()
			}
			handleErr := session.Handle(ctx, candidate)
			if preSignReorg {
				if handleErr == nil || broadcaster.Transaction() != nil {
					t.Fatalf("orphaned preflight was sent: %v", handleErr)
				}
				usage, err := coordinator.budget.Snapshot(ctx)
				if err != nil || !usage.Global.Spent.Cumulative.IsZero() || !usage.Global.Reserved.Cumulative.IsZero() {
					t.Fatalf("unused preflight reservation leaked: %+v, %v", usage.Global, err)
				}
				return
			}
			if handleErr != nil {
				t.Fatal(handleErr)
			}
			transaction := broadcaster.Transaction()
			if unmined && len(accepted.transactions) > 0 {
				transaction = accepted.transactions[0]
			}
			if transaction == nil {
				t.Fatal("fresh deposit was not swept before finality")
			}
			if primary.sourceReads.Load() != 1 || primary.sponsorReads.Load() != 1 {
				t.Fatalf("nonce RPC counts: source=%d sponsor=%d", primary.sourceReads.Load(), primary.sponsorReads.Load())
			}
			if finality.latestCalls.Load() != 1 || finality.finalizedCalls.Load() != 0 {
				t.Fatalf("head RPC counts: latest=%d finalized=%d", finality.latestCalls.Load(), finality.finalizedCalls.Load())
			}
			if finality.nativeReads.Load() != 1 || finality.tokenReads.Load() != 2 {
				t.Fatalf("balance RPC counts: native=%d token=%d", finality.nativeReads.Load(), finality.tokenReads.Load())
			}
			incidentID := domain.NewAssetIncidentID(candidate.ID, domain.CandidateToken, token)
			incident, found, err := state.RescueIncident(ctx, incidentID)
			if err != nil || !found || incident.Status != store.RescueBroadcast || incident.SourceNonce != 7 || !incident.Policy.LatestState {
				t.Fatalf("unfinalized outcome: %+v, %v", incident, err)
			}
			budget, err := coordinator.budget.Snapshot(ctx)
			if err != nil || !budget.Global.Spent.Cumulative.IsZero() || budget.Global.Reserved.Cumulative.IsZero() {
				t.Fatalf("unfinalized fee was settled: %+v, %v", budget.Global, err)
			}
			// The corresponding finalized candidate must not trigger another payment
			// while the earlier operation for the same asset is unresolved.
			duplicate := domain.NewLogCandidate(localChainID, source, domain.Token{Address: token}, depositReceipt.BlockHash, deposit.Hash(), depositReceipt.BlockNumber.Uint64(), 0)
			if name != "persisted pending after disabling latest" {
				if err := session.Handle(ctx, duplicate); err != nil {
					t.Fatalf("finalized duplicate: %v", err)
				}
				periodic := domain.NewTokenReconciliationCandidate(localChainID, source, domain.Token{Address: token}, 1, 7)
				var pending fastPendingError
				if err := session.Handle(ctx, periodic); !errors.As(err, &pending) {
					t.Fatalf("same-asset retry was not deferred: %v", err)
				}
				if primary.sourceReads.Load() != 1 {
					t.Fatal("duplicate signed a new authorization")
				}
			}
			if unmined {
				session.lastReceiptScan = finality.finalized
				coordinator.clock = localChainClock{now: coordinator.clock.Now().Add(2 * time.Second)}
				if err := session.reconcileFast(ctx); err != nil {
					t.Fatal(err)
				}
				if len(accepted.transactions) != 2 || accepted.transactions[1].Hash() != transaction.Hash() {
					t.Fatal("accepted transaction was lost or retried with different bytes")
				}
				if primary.sourceReads.Load() != 1 {
					t.Fatal("retry created another authorization")
				}
				return
			}
			finality.finalized, err = finality.localChainFinality.Finalized(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if reorg {
				finality.orphan = depositReceipt.BlockNumber.Uint64()
				// Receipt handling follows the persisted prestate mode, even if
				// the operator changes the mode for newly created operations.
				coordinator.latestState = false
			}
			if err := session.reconcileFast(ctx); err != nil {
				t.Fatal(err)
			}
			incident, _, err = state.RescueIncident(ctx, incidentID)
			want := store.RescueTokenReported
			if reorg {
				want = store.RescueFailed
			}
			if err != nil || incident.Status != want {
				t.Fatalf("final outcome: %+v, %v", incident, err)
			}
			budget, err = coordinator.budget.Snapshot(ctx)
			if err != nil || budget.Global.Spent.Cumulative.IsZero() || !budget.Global.Reserved.Cumulative.IsZero() {
				t.Fatalf("final fee was not accounted: %+v, %v", budget.Global, err)
			}
		})
	}
}
