package rescue

import (
	"context"
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"guard-daemon/internal/domain"
	"guard-daemon/internal/store"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

func TestLocalChain_ClaimWithValueAndEmptySource(t *testing.T) {
	for _, scenario := range []string{"success", "expired", "payment exceeds budget"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			sourceKey := localChainPrivateKey(t, localChainSourceKeyHex)
			sponsorKey := localChainPrivateKey(t, localChainSponsorKeyHex)
			source := crypto.PubkeyToAddress(sourceKey.PublicKey)
			sponsor := crypto.PubkeyToAddress(sponsorKey.PublicKey)
			destination := crypto.PubkeyToAddress(localChainPrivateKey(t, localChainDestinationKeyHex).PublicKey)
			token, target := common.Address{0xa1}, common.Address{0xa2}
			content, err := os.ReadFile("testdata/claim-contracts.json")
			if err != nil {
				t.Fatal(err)
			}
			var fixture map[string]string
			if err := json.Unmarshal(content, &fixture); err != nil {
				t.Fatal(err)
			}
			backend := localChainBackend(t, types.GenesisAlloc{
				source: {Balance: big.NewInt(1)}, sponsor: {Balance: localChainEther(20)}, destination: {Balance: big.NewInt(1)},
				token:  {Balance: new(big.Int), Code: hexutil.MustDecode(fixture["StandardToken"])},
				target: {Balance: new(big.Int), Code: hexutil.MustDecode(fixture["ClaimTarget"])},
			})
			client := backend.Client()
			_, rescuer, _ := localChainDeployRescuer(t, ctx, backend, sponsorKey, destination)
			claimABI, err := abi.JSON(strings.NewReader(`[{"type":"function","name":"claim","stateMutability":"payable","inputs":[{"type":"address"},{"type":"uint256"}],"outputs":[]}]`))
			if err != nil {
				t.Fatal(err)
			}
			data, err := claimABI.Pack("claim", token, big.NewInt(7))
			if err != nil {
				t.Fatal(err)
			}
			plan := domain.ClaimPlan{
				Target: target, Data: data, Token: token, Value: *uint256.NewInt(123_456_789_000),
				MinimumReceived: *uint256.NewInt(7), GasLimit: 300_000, MaximumCost: *uint256.NewInt(1_000_000_000_000_000),
				ValidUntil: uint64(time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC).Unix()),
			}
			if scenario == "expired" {
				plan.ValidUntil = 1
			}
			if scenario == "payment exceeds budget" {
				plan.Value = plan.MaximumCost
			}
			coordinator, state := localChainCoordinator(t, sourceKey, sponsorKey, destination, rescuer, "atomic-claim", plan)
			primary, finality := &localChainPrimary{client: client}, &localChainFinality{client: client}
			broadcaster := &localChainBroadcaster{client: client, backend: backend}
			session, err := coordinator.NewSession(ctx, 1, primary, finality, broadcaster)
			if err != nil {
				t.Fatal(err)
			}
			candidate := domain.NewClaimCandidate(localChainID, source, plan)
			err = session.Handle(ctx, candidate)
			if scenario != "success" {
				if broadcaster.Transaction() != nil {
					t.Fatal("expired or over-budget claim was sent")
				}
				if scenario == "payment exceeds budget" && errorCode(err) != codeBudgetExceeded {
					t.Fatalf("claim budget error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			transaction := broadcaster.Transaction()
			if transaction == nil || transaction.Value().Cmp(plan.Value.ToBig()) != 0 {
				t.Fatal("claim payment was not included")
			}
			balance, err := client.BalanceAt(ctx, target, nil)
			if err != nil || balance.Cmp(plan.Value.ToBig()) != 0 {
				t.Fatalf("target payment = %v, %v", balance, err)
			}
			for account, expected := range map[common.Address]int64{source: 0, destination: 7} {
				data, err := coordinator.erc20.PackBalanceOf(account)
				if err != nil {
					t.Fatal(err)
				}
				result, err := client.CallContract(ctx, ethereum.CallMsg{To: &token, Data: data}, nil)
				if err != nil || new(big.Int).SetBytes(result).Cmp(big.NewInt(expected)) != 0 {
					t.Fatalf("token balance for %s: %x, %v", account, result, err)
				}
			}
			receipt, err := client.TransactionReceipt(ctx, transaction.Hash())
			if err != nil {
				t.Fatal(err)
			}
			cost := new(big.Int).Mul(new(big.Int).SetUint64(receipt.GasUsed), receipt.EffectiveGasPrice)
			cost.Add(cost, plan.Value.ToBig())
			snapshot, err := coordinator.budget.Snapshot(ctx)
			if err != nil || snapshot.Global.Spent.Cumulative.ToBig().Cmp(cost) != 0 {
				t.Fatalf("claim accounting: %+v, %v; want %s", snapshot.Global, err, cost)
			}
			incidentID := domain.NewAssetIncidentID(candidate.ID, domain.CandidateClaim, token)
			incident, found, err := state.RescueIncident(ctx, incidentID)
			if err != nil || !found || incident.Status != store.RescueTokenReported {
				t.Fatalf("claim outcome: %+v, %v", incident, err)
			}
			if err := state.PruneRescueIncidents(ctx, 0); err != nil {
				t.Fatal(err)
			}
			// Reopening the network session must not execute a completed request again,
			// even after ordinary sweep outcomes are pruned.
			session, err = coordinator.NewSession(ctx, 2, primary, finality, broadcaster)
			if err != nil {
				t.Fatal(err)
			}
			if err := session.Handle(ctx, candidate); err != nil {
				t.Fatal(err)
			}
			nonce, err := client.PendingNonceAt(ctx, sponsor)
			if err != nil || nonce != transaction.Nonce()+1 {
				t.Fatalf("completed claim was repeated: nonce=%d, error=%v", nonce, err)
			}
		})
	}
}
