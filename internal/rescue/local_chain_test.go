package rescue

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"guard-daemon/internal/clock"
	"guard-daemon/internal/contracts"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"
	guardrpc "guard-daemon/internal/rpc"
	"guard-daemon/internal/store"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
	"github.com/holiman/uint256"
)

const (
	localChainArtifactPath = "../../artifacts/contracts/RescuerV2.json"
	localChainID           = domain.NetworkID(1337)

	// Эти ключи используются только в данном детерминированном тесте локальной сети.
	localChainSourceKeyHex      = "1111111111111111111111111111111111111111111111111111111111111111"
	localChainSponsorKeyHex     = "2222222222222222222222222222222222222222222222222222222222222222"
	localChainDestinationKeyHex = "3333333333333333333333333333333333333333333333333333333333333333"
	localChainAttackerKeyHex    = "4444444444444444444444444444444444444444444444444444444444444444"
	localChainMaliciousKeyHex   = "5555555555555555555555555555555555555555555555555555555555555555"
)

var errLocalChainUnexpectedSleep = errors.New("local-chain test unexpectedly entered receipt polling")

// Тестовый байткод вредоносного контракта увеличивает нулевой слот хранилища
// в контексте вызывающей стороны: PUSH0 SLOAD PUSH1 1 ADD PUSH0 SSTORE STOP.
var localChainMaliciousRuntime = []byte{0x5f, 0x54, 0x60, 0x01, 0x01, 0x5f, 0x55, 0x00}

func TestLocalChain_AtomicNative(t *testing.T) {
	ctx := context.Background()
	sourceKey := localChainPrivateKey(t, localChainSourceKeyHex)
	sponsorKey := localChainPrivateKey(t, localChainSponsorKeyHex)
	destinationKey := localChainPrivateKey(t, localChainDestinationKeyHex)
	source := crypto.PubkeyToAddress(sourceKey.PublicKey)
	sponsor := crypto.PubkeyToAddress(sponsorKey.PublicKey)
	destination := crypto.PubkeyToAddress(destinationKey.PublicKey)

	initialSourceBalance := new(big.Int).Mul(big.NewInt(3), big.NewInt(1_000_000_000_000_000_000))
	initialSponsorBalance := new(big.Int).Mul(big.NewInt(20), big.NewInt(1_000_000_000_000_000_000))
	backend := simulated.NewBackend(types.GenesisAlloc{
		source:      {Balance: new(big.Int).Set(initialSourceBalance)},
		sponsor:     {Balance: new(big.Int).Set(initialSponsorBalance)},
		destination: {Balance: big.NewInt(1)},
	})
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Errorf("backend.Close() error = %v", err)
		}
	})
	client := backend.Client()

	chainID, err := client.ChainID(ctx)
	if err != nil {
		t.Fatalf("ChainID() returned an error: %v", err)
	}
	if chainID.Cmp(big.NewInt(int64(localChainID))) != 0 {
		t.Fatalf("network ID = %s, want %d", chainID, localChainID)
	}

	contractABI, bytecode := localChainArtifact(t)
	deploymentNonce, err := client.PendingNonceAt(ctx, sponsor)
	if err != nil {
		t.Fatalf("PendingNonceAt() during deployment returned an error: %v", err)
	}
	constructor, err := contractABI.Pack("", destination, sponsor)
	if err != nil {
		t.Fatalf("packing RescuerV2 constructor: %v", err)
	}
	deploymentData := append(append([]byte(nil), bytecode...), constructor...)
	deploymentTransaction := localChainDeploymentTransaction(t, ctx, client, sponsorKey, sponsor, chainID, deploymentNonce, deploymentData)
	if err := client.SendTransaction(ctx, deploymentTransaction); err != nil {
		t.Fatalf("sending RescuerV2 deployment: %v", err)
	}
	deploymentBlockHash := backend.Commit()
	deploymentReceipt, err := client.TransactionReceipt(ctx, deploymentTransaction.Hash())
	if err != nil {
		t.Fatalf("deployment receipt: %v", err)
	}
	rescuer := crypto.CreateAddress(sponsor, deploymentNonce)
	if deploymentReceipt.Status != types.ReceiptStatusSuccessful || deploymentReceipt.BlockHash != deploymentBlockHash || deploymentReceipt.ContractAddress != rescuer {
		t.Fatalf("deployment receipt status/block/contract = %d/%s/%s, want success/%s/%s", deploymentReceipt.Status, deploymentReceipt.BlockHash, deploymentReceipt.ContractAddress, deploymentBlockHash, rescuer)
	}
	rescuerCode, err := client.CodeAt(ctx, rescuer, deploymentReceipt.BlockNumber)
	if err != nil {
		t.Fatalf("CodeAt() for deployed RescuerV2 returned an error: %v", err)
	}
	if len(rescuerCode) == 0 {
		t.Fatal("deployed RescuerV2 has empty code")
	}

	coordinatorSponsorNonce, err := client.PendingNonceAt(ctx, sponsor)
	if err != nil {
		t.Fatalf("PendingNonceAt() after deployment returned an error: %v", err)
	}
	if coordinatorSponsorNonce != deploymentNonce+1 {
		t.Fatalf("sponsor nonce after deployment = %d, want %d", coordinatorSponsorNonce, deploymentNonce+1)
	}

	testClock := localChainClock{now: time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)}
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"), store.OpenOptions{
		Network:             localChainID,
		Source:              source,
		Sponsor:             sponsor,
		Destination:         destination,
		Rescuer:             rescuer,
		PolicyFingerprint:   sha256.Sum256([]byte("local-chain-atomic-native-policy-v1")),
		MaxPending:          16,
		MaxDiscoveredTokens: 16,
		Clock:               testClock,
	})
	if err != nil {
		t.Fatalf("store.Open() returned an error: %v", err)
	}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Errorf("state.Close() error = %v", err)
		}
	})
	lease, err := state.Acquire(ctx, store.LeaseKey{Network: localChainID, Sponsor: sponsor}, "local-chain-test", time.Hour)
	if err != nil {
		t.Fatalf("Acquire() returned an error: %v", err)
	}
	leaseReleased := false
	t.Cleanup(func() {
		if !leaseReleased {
			if err := state.Release(context.Background(), lease); err != nil {
				t.Errorf("lease Release() error = %v", err)
			}
		}
	})

	authorizer, err := NewPrivateKeyAuthorizationSigner(sourceKey)
	if err != nil {
		t.Fatalf("NewPrivateKeyAuthorizationSigner() returned an error: %v", err)
	}
	transactioner, err := NewPrivateKeyTransactionSigner(sponsorKey)
	if err != nil {
		t.Fatalf("NewPrivateKeyTransactionSigner() returned an error: %v", err)
	}
	coordinatorConfig := withTestEconomicPolicy(t, Config{
		Network: domain.Network{
			Name:       "deterministic-local-chain",
			ChainID:    localChainID,
			Rescuer:    rescuer,
			HasRescuer: true,
		},
		Source:          source,
		Sponsor:         sponsor,
		Destination:     destination,
		NativeThreshold: big.NewInt(0),
		StartupTimeout:  time.Second,
		FeeReadTimeout:  time.Second,
		State:           state,
		LeaseManager:    state,
		Lease:           lease,
		ProcessFence:    newFakeProcessFence(),
		LeaseTTL:        time.Hour,
		MaxAttempts:     1,
		RetryDelay:      time.Second,
		ReceiptTimeout:  time.Second,
	}, testClock)
	coordinator, err := NewCoordinator(coordinatorConfig, authorizer, transactioner, testClock, observability.Discard{})
	if err != nil {
		t.Fatalf("NewCoordinator() returned an error: %v", err)
	}

	primary := &localChainPrimary{client: client}
	finality := &localChainFinality{client: client}
	broadcaster := &localChainBroadcaster{client: client, backend: backend}
	session, err := coordinator.NewSession(ctx, 1, primary, finality, broadcaster)
	if err != nil {
		t.Fatalf("NewSession() returned an error: %v", err)
	}
	prestate, err := finality.Finalized(ctx)
	if err != nil {
		t.Fatalf("Finalized() for prestate returned an error: %v", err)
	}
	sourceBefore, err := finality.BalanceAt(ctx, prestate, source)
	if err != nil {
		t.Fatalf("BalanceAt() for source in prestate returned an error: %v", err)
	}
	destinationBefore, err := finality.BalanceAt(ctx, prestate, destination)
	if err != nil {
		t.Fatalf("BalanceAt() for destination in prestate returned an error: %v", err)
	}
	if sourceBefore.Cmp(initialSourceBalance) != 0 {
		t.Fatalf("source balance in prestate = %s, want %s", sourceBefore, initialSourceBalance)
	}

	candidate := domain.NewBlockCandidate(localChainID, domain.CandidateNative, source, prestate.Hash, prestate.Number)
	if err := session.Handle(ctx, candidate); err != nil {
		transaction := broadcaster.Transaction()
		if transaction == nil {
			t.Fatalf("Handle() returned error %v before broadcast", err)
		}
		receipt, receiptErr := client.TransactionReceipt(ctx, transaction.Hash())
		code, codeErr := client.CodeAt(ctx, source, nil)
		receiptSummary := "nil"
		if receipt != nil {
			receiptSummary = fmt.Sprintf("status=%d gasUsed=%d block=%s", receipt.Status, receipt.GasUsed, receipt.BlockHash)
		}
		t.Fatalf("Handle() returned error %v; transaction gas = %d; receipt = %s; receipt error = %v; source code = %x; code error = %v", err, transaction.Gas(), receiptSummary, receiptErr, code, codeErr)
	}
	transaction := broadcaster.Transaction()
	if transaction == nil {
		t.Fatal("coordinator did not broadcast a transaction")
	}
	if transaction.Type() != types.SetCodeTxType || transaction.To() == nil || *transaction.To() != source {
		t.Fatalf("broadcast transaction type/destination = %d/%v, want SetCodeTx/%s", transaction.Type(), transaction.To(), source)
	}
	if transaction.Nonce() != coordinatorSponsorNonce {
		t.Fatalf("coordinator sponsor nonce = %d, want next nonce %d", transaction.Nonce(), coordinatorSponsorNonce)
	}
	wantSweepEth, err := contractABI.Pack("sweepEth")
	if err != nil {
		t.Fatalf("packing sweepEth: %v", err)
	}
	if len(transaction.Data()) == 0 || !bytes.Equal(transaction.Data(), wantSweepEth) {
		t.Fatalf("SetCodeTx data = %x, want sweepEth data %x", transaction.Data(), wantSweepEth)
	}
	authorizations := transaction.SetCodeAuthorizations()
	if len(authorizations) != 1 {
		t.Fatalf("SetCodeTx authorization count = %d, want 1", len(authorizations))
	}
	authorization := authorizations[0]
	authority, err := authorization.Authority()
	if err != nil {
		t.Fatalf("authorization Authority() returned an error: %v", err)
	}
	if authority != source || authorization.Address != rescuer || authorization.ChainID.ToBig().Cmp(chainID) != 0 || authorization.Nonce != 0 {
		t.Fatalf("authorization authority/target/network/nonce = %s/%s/%s/%d, want %s/%s/%s/0", authority, authorization.Address, authorization.ChainID.ToBig(), authorization.Nonce, source, rescuer, chainID)
	}
	transactionSender, err := types.Sender(types.LatestSignerForChainID(chainID), transaction)
	if err != nil {
		t.Fatalf("recovering transaction sender: %v", err)
	}
	if transactionSender != sponsor {
		t.Fatalf("transaction sender = %s, want sponsor %s", transactionSender, sponsor)
	}

	receipt, err := finality.Receipt(ctx, transaction.Hash())
	if err != nil {
		t.Fatalf("finalized canonical receipt: %v", err)
	}
	if receipt.Status != types.ReceiptStatusSuccessful || receipt.BlockHash != broadcaster.BlockHash() {
		t.Fatalf("rescue receipt status/block = %d/%s, want success/%s", receipt.Status, receipt.BlockHash, broadcaster.BlockHash())
	}
	poststate, err := finality.Finalized(ctx)
	if err != nil {
		t.Fatalf("Finalized() for poststate returned an error: %v", err)
	}
	if poststate.Number < receipt.BlockNumber.Uint64() {
		t.Fatalf("finalized block %d precedes receipt block %d", poststate.Number, receipt.BlockNumber.Uint64())
	}
	delegationCode, err := finality.CodeAt(ctx, poststate, source)
	if err != nil {
		t.Fatalf("CodeAt() for source in poststate returned an error: %v", err)
	}
	delegationTarget, err := contracts.ParseDelegation(delegationCode)
	if err != nil {
		t.Fatalf("source code is not an EIP-7702 delegation: %v", err)
	}
	if delegationTarget != rescuer {
		t.Fatalf("source delegation target = %s, want %s", delegationTarget, rescuer)
	}
	destinationAfter, err := finality.BalanceAt(ctx, poststate, destination)
	if err != nil {
		t.Fatalf("BalanceAt() for destination in poststate returned an error: %v", err)
	}
	destinationDelta := new(big.Int).Sub(destinationAfter, destinationBefore)
	if destinationDelta.Cmp(initialSourceBalance) != 0 {
		t.Fatalf("destination balance delta = %s, want initial source balance %s", destinationDelta, initialSourceBalance)
	}

	incidentID := domain.NewAssetIncidentID(candidate.ID, domain.CandidateNative, common.Address{})
	incident, found, err := state.RescueIncident(ctx, incidentID)
	if err != nil {
		t.Fatalf("RescueIncident() returned an error: %v", err)
	}
	if !found || incident.Status != store.RescueTrustedSuccess || incident.TxHash != transaction.Hash() || incident.SponsorNonce != coordinatorSponsorNonce {
		t.Fatalf("persistent incident found/status/hash/nonce = %t/%v/%s/%d, want true/%v/%s/%d", found, incident.Status, incident.TxHash, incident.SponsorNonce, store.RescueTrustedSuccess, transaction.Hash(), coordinatorSponsorNonce)
	}
	if err := coordinator.ReleaseLease(ctx); err != nil {
		t.Fatalf("ReleaseLease() returned an error: %v", err)
	}
	leaseReleased = true
}

func TestLocalChain_NoProactiveRenewalWithMaliciousDelegation(t *testing.T) {
	ctx := context.Background()
	sourceKey := localChainPrivateKey(t, localChainSourceKeyHex)
	sponsorKey := localChainPrivateKey(t, localChainSponsorKeyHex)
	destinationKey := localChainPrivateKey(t, localChainDestinationKeyHex)
	maliciousKey := localChainPrivateKey(t, localChainMaliciousKeyHex)
	source := crypto.PubkeyToAddress(sourceKey.PublicKey)
	sponsor := crypto.PubkeyToAddress(sponsorKey.PublicKey)
	destination := crypto.PubkeyToAddress(destinationKey.PublicKey)
	maliciousDelegate := crypto.PubkeyToAddress(maliciousKey.PublicKey)
	maliciousDelegation := localChainDelegation(maliciousDelegate)

	backend := localChainBackend(t, types.GenesisAlloc{
		source:            {Balance: localChainEther(3), Code: maliciousDelegation, Storage: localChainCounterStorage(1)},
		sponsor:           {Balance: localChainEther(20)},
		destination:       {Balance: big.NewInt(1)},
		maliciousDelegate: {Balance: new(big.Int), Code: append([]byte(nil), localChainMaliciousRuntime...)},
	})
	client := backend.Client()
	_, rescuer, _ := localChainDeployRescuer(t, ctx, backend, sponsorKey, destination)
	coordinator, _ := localChainCoordinator(t, sourceKey, sponsorKey, destination, rescuer, "no-proactive-renewal")
	primary := &localChainPrimary{client: client}
	finality := &localChainFinality{client: client}
	broadcaster := &localChainRejectBroadcaster{}

	counterBefore := localChainCounter(t, ctx, client, source)
	sourceNonceBefore, err := client.PendingNonceAt(ctx, source)
	if err != nil {
		t.Fatalf("PendingNonceAt() for source before session returned an error: %v", err)
	}
	if _, err := coordinator.NewSession(ctx, 1, primary, finality, broadcaster); err != nil {
		t.Fatalf("NewSession() returned an error: %v", err)
	}
	if calls := broadcaster.Calls(); calls != 0 {
		t.Fatalf("NewSession() broadcast %d transactions, want 0", calls)
	}
	counterAfter := localChainCounter(t, ctx, client, source)
	if counterBefore.Cmp(big.NewInt(1)) != 0 || counterAfter.Cmp(counterBefore) != 0 {
		t.Fatalf("source fallback handler counter before/after NewSession = %s/%s, want unchanged value 1", counterBefore, counterAfter)
	}
	sourceNonceAfter, err := client.PendingNonceAt(ctx, source)
	if err != nil {
		t.Fatalf("PendingNonceAt() for source after session returned an error: %v", err)
	}
	if sourceNonceAfter != sourceNonceBefore {
		t.Fatalf("source nonce after NewSession = %d, want unchanged value %d", sourceNonceAfter, sourceNonceBefore)
	}
	code, err := client.CodeAt(ctx, source, nil)
	if err != nil {
		t.Fatalf("CodeAt() for source after session returned an error: %v", err)
	}
	target, parseErr := contracts.ParseDelegation(code)
	if parseErr != nil || !bytes.Equal(code, maliciousDelegation) || target != maliciousDelegate {
		t.Fatalf("source delegation after session = %x, target %s, error %v, want malicious target %s", code, target, parseErr, maliciousDelegate)
	}
}

func TestLocalChain_CompetingAuthorizationIsLostRace(t *testing.T) {
	ctx := context.Background()
	sourceKey := localChainPrivateKey(t, localChainSourceKeyHex)
	sponsorKey := localChainPrivateKey(t, localChainSponsorKeyHex)
	destinationKey := localChainPrivateKey(t, localChainDestinationKeyHex)
	attackerKey := localChainPrivateKey(t, localChainAttackerKeyHex)
	maliciousKey := localChainPrivateKey(t, localChainMaliciousKeyHex)
	source := crypto.PubkeyToAddress(sourceKey.PublicKey)
	sponsor := crypto.PubkeyToAddress(sponsorKey.PublicKey)
	destination := crypto.PubkeyToAddress(destinationKey.PublicKey)
	attacker := crypto.PubkeyToAddress(attackerKey.PublicKey)
	maliciousDelegate := crypto.PubkeyToAddress(maliciousKey.PublicKey)
	initialSourceBalance := localChainEther(3)

	backend := localChainBackend(t, types.GenesisAlloc{
		source:            {Balance: new(big.Int).Set(initialSourceBalance), Storage: localChainCounterStorage(1)},
		sponsor:           {Balance: localChainEther(20)},
		destination:       {Balance: big.NewInt(1)},
		attacker:          {Balance: localChainEther(20)},
		maliciousDelegate: {Balance: new(big.Int), Code: append([]byte(nil), localChainMaliciousRuntime...)},
	})
	client := backend.Client()
	contractABI, rescuer, coordinatorSponsorNonce := localChainDeployRescuer(t, ctx, backend, sponsorKey, destination)
	coordinator, state := localChainCoordinator(t, sourceKey, sponsorKey, destination, rescuer, "competing-authorization")
	primary := &localChainPrimary{client: client}
	finality := &localChainFinality{client: client}
	broadcaster := &localChainCompetingBroadcaster{
		client: client, backend: backend, source: source, sourceKey: sourceKey,
		attacker: attacker, attackerKey: attackerKey, maliciousDelegate: maliciousDelegate,
	}
	session, err := coordinator.NewSession(ctx, 1, primary, finality, broadcaster)
	if err != nil {
		t.Fatalf("NewSession() returned an error: %v", err)
	}
	prestate, err := finality.Finalized(ctx)
	if err != nil {
		t.Fatalf("Finalized() for prestate returned an error: %v", err)
	}
	destinationBefore, err := finality.BalanceAt(ctx, prestate, destination)
	if err != nil {
		t.Fatalf("BalanceAt() for destination in prestate returned an error: %v", err)
	}
	sourceNonce, err := client.PendingNonceAt(ctx, source)
	if err != nil {
		t.Fatalf("PendingNonceAt() for source in prestate returned an error: %v", err)
	}
	if counter := localChainCounter(t, ctx, client, source); counter.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("source fallback handler counter in prestate = %s, want 1", counter)
	}

	candidate := domain.NewBlockCandidate(localChainID, domain.CandidateNative, source, prestate.Hash, prestate.Number)
	handleErr := session.Handle(ctx, candidate)
	var classified *domain.ClassifiedError
	if !errors.As(handleErr, &classified) || classified.Class != domain.ErrorPostcondition || classified.Code != codeLostRace || classified.Retryable || classified.Ambiguous {
		receiptSummary := "unavailable"
		transactions := broadcaster.Evidence().production
		if len(transactions) == 1 {
			if receipt, receiptErr := client.TransactionReceipt(ctx, transactions[0].Hash()); receiptErr == nil {
				receiptSummary = fmt.Sprintf("status=%d gas=%d/%d", receipt.Status, receipt.GasUsed, transactions[0].Gas())
			} else {
				receiptSummary = receiptErr.Error()
			}
		}
		t.Fatalf("Handle() classification = %#v, error %v, receipt %s, want terminal LostRace", classified, handleErr, receiptSummary)
	}
	evidence := broadcaster.Evidence()
	if len(evidence.production) != 1 {
		t.Fatalf("production broadcast count = %d, want 1", len(evidence.production))
	}
	production := evidence.production[0]
	wantSweepEth, err := contractABI.Pack("sweepEth")
	if err != nil {
		t.Fatalf("packing sweepEth: %v", err)
	}
	if production.Type() != types.SetCodeTxType || production.To() == nil || *production.To() != source || len(production.Data()) == 0 || !bytes.Equal(production.Data(), wantSweepEth) {
		t.Fatalf("production transaction type/destination/data = %d/%v/%x, want atomic SetCodeTx to source with sweepEth", production.Type(), production.To(), production.Data())
	}
	if production.Nonce() != coordinatorSponsorNonce {
		t.Fatalf("production transaction sponsor nonce = %d, want %d", production.Nonce(), coordinatorSponsorNonce)
	}
	productionAuthorizations := production.SetCodeAuthorizations()
	if len(productionAuthorizations) != 1 {
		t.Fatalf("production authorization count = %d, want 1", len(productionAuthorizations))
	}
	if productionAuthorizations[0].Nonce != sourceNonce || productionAuthorizations[0].Address != rescuer {
		t.Fatalf("production authorization nonce/target = %d/%s, want %d/%s", productionAuthorizations[0].Nonce, productionAuthorizations[0].Address, sourceNonce, rescuer)
	}
	if evidence.competing == nil {
		t.Fatal("competing transaction was not broadcast")
	}
	if evidence.competing.To() == nil || *evidence.competing.To() == source || len(evidence.competing.Data()) != 0 {
		t.Fatalf("competing external transaction destination/data = %v/%x, want non-source destination and empty data", evidence.competing.To(), evidence.competing.Data())
	}
	competingAuthorizations := evidence.competing.SetCodeAuthorizations()
	if len(competingAuthorizations) != 1 {
		t.Fatalf("competing authorization count = %d, want 1", len(competingAuthorizations))
	}
	if competingAuthorizations[0].Nonce != sourceNonce || competingAuthorizations[0].Address != maliciousDelegate {
		t.Fatalf("competing authorization nonce/target = %d/%s, want %d/%s", competingAuthorizations[0].Nonce, competingAuthorizations[0].Address, sourceNonce, maliciousDelegate)
	}
	if evidence.competingReceipt == nil || evidence.competingReceipt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("competing receipt = %+v, want status 1", evidence.competingReceipt)
	}
	if counter := new(big.Int).SetBytes(evidence.storageAfterCompeting); counter.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("source counter after competing external call not to source = %s, want unchanged value 1", counter)
	}

	receipt, err := finality.Receipt(ctx, production.Hash())
	if err != nil {
		t.Fatalf("finalized production receipt: %v", err)
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("production external receipt status = %d, want 1 despite LostRace result", receipt.Status)
	}
	poststate, err := finality.Finalized(ctx)
	if err != nil {
		t.Fatalf("Finalized() for poststate returned an error: %v", err)
	}
	code, err := finality.CodeAt(ctx, poststate, source)
	if err != nil {
		t.Fatalf("CodeAt() for source in poststate returned an error: %v", err)
	}
	target, parseErr := contracts.ParseDelegation(code)
	if parseErr != nil || target != maliciousDelegate {
		t.Fatalf("source delegation target/error in poststate = %s/%v, want malicious target %s", target, parseErr, maliciousDelegate)
	}
	if counter := localChainCounter(t, ctx, client, source); counter.Cmp(big.NewInt(2)) != 0 {
		t.Fatalf("source fallback handler counter after production asset call = %s, want 2", counter)
	}
	sourceNonceAfter, err := client.NonceAt(ctx, source, nil)
	if err != nil {
		t.Fatalf("NonceAt() for source in poststate returned an error: %v", err)
	}
	if sourceNonceAfter != sourceNonce+1 {
		t.Fatalf("source nonce after competing and skipped production authorizations = %d, want %d", sourceNonceAfter, sourceNonce+1)
	}
	destinationAfter, err := finality.BalanceAt(ctx, poststate, destination)
	if err != nil {
		t.Fatalf("BalanceAt() for destination in poststate returned an error: %v", err)
	}
	if delta := new(big.Int).Sub(destinationAfter, destinationBefore); delta.Sign() != 0 || delta.Cmp(initialSourceBalance) >= 0 {
		t.Fatalf("destination delta after lost race = %s, want zero and less than source balance %s", delta, initialSourceBalance)
	}
	sourceAfter, err := finality.BalanceAt(ctx, poststate, source)
	if err != nil {
		t.Fatalf("BalanceAt() for source in poststate returned an error: %v", err)
	}
	if sourceAfter.Cmp(initialSourceBalance) != 0 {
		t.Fatalf("source balance after malicious fallback handler = %s, want unchanged %s", sourceAfter, initialSourceBalance)
	}

	incidentID := domain.NewAssetIncidentID(candidate.ID, domain.CandidateNative, common.Address{})
	incident, found, err := state.RescueIncident(ctx, incidentID)
	if err != nil {
		t.Fatalf("RescueIncident() returned an error: %v", err)
	}
	if !found || incident.Status != store.RescueLostRace || incident.Status == store.RescueTrustedSuccess || incident.LastCode != codeLostRace || incident.TxHash != production.Hash() {
		t.Fatalf("persistent incident found/status/code/hash = %t/%v/%s/%s, want LostRace/%s/%s", found, incident.Status, incident.LastCode, incident.TxHash, codeLostRace, production.Hash())
	}
}

func localChainBackend(t *testing.T, alloc types.GenesisAlloc) *simulated.Backend {
	t.Helper()
	backend := simulated.NewBackend(alloc)
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Errorf("backend.Close() error = %v", err)
		}
	})
	return backend
}

func localChainEther(amount int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(amount), big.NewInt(1_000_000_000_000_000_000))
}

func localChainDelegation(target common.Address) []byte {
	return append([]byte{0xef, 0x01, 0x00}, target.Bytes()...)
}

func localChainCounterStorage(counter int64) map[common.Hash]common.Hash {
	return map[common.Hash]common.Hash{{}: common.BigToHash(big.NewInt(counter))}
}

func localChainDeployRescuer(t *testing.T, ctx context.Context, backend *simulated.Backend, sponsorKey *ecdsa.PrivateKey, destination common.Address) (abi.ABI, common.Address, uint64) {
	t.Helper()
	client := backend.Client()
	chainID, err := client.ChainID(ctx)
	if err != nil {
		t.Fatalf("ChainID() during deployment returned an error: %v", err)
	}
	if chainID.Cmp(big.NewInt(int64(localChainID))) != 0 {
		t.Fatalf("network ID = %s, want %d", chainID, localChainID)
	}
	sponsor := crypto.PubkeyToAddress(sponsorKey.PublicKey)
	contractABI, bytecode := localChainArtifact(t)
	deploymentNonce, err := client.PendingNonceAt(ctx, sponsor)
	if err != nil {
		t.Fatalf("PendingNonceAt() during deployment returned an error: %v", err)
	}
	constructor, err := contractABI.Pack("", destination, sponsor)
	if err != nil {
		t.Fatalf("packing RescuerV2 constructor: %v", err)
	}
	data := append(append([]byte(nil), bytecode...), constructor...)
	transaction := localChainDeploymentTransaction(t, ctx, client, sponsorKey, sponsor, chainID, deploymentNonce, data)
	if err := client.SendTransaction(ctx, transaction); err != nil {
		t.Fatalf("sending RescuerV2 deployment: %v", err)
	}
	blockHash := backend.Commit()
	receipt, err := client.TransactionReceipt(ctx, transaction.Hash())
	if err != nil {
		t.Fatalf("deployment receipt: %v", err)
	}
	rescuer := crypto.CreateAddress(sponsor, deploymentNonce)
	if receipt.Status != types.ReceiptStatusSuccessful || receipt.BlockHash != blockHash || receipt.ContractAddress != rescuer {
		t.Fatalf("deployment receipt status/block/contract = %d/%s/%s, want success/%s/%s", receipt.Status, receipt.BlockHash, receipt.ContractAddress, blockHash, rescuer)
	}
	code, err := client.CodeAt(ctx, rescuer, receipt.BlockNumber)
	if err != nil {
		t.Fatalf("CodeAt() for deployed RescuerV2 returned an error: %v", err)
	}
	if len(code) == 0 {
		t.Fatal("deployed RescuerV2 has empty code")
	}
	nextNonce, err := client.PendingNonceAt(ctx, sponsor)
	if err != nil {
		t.Fatalf("PendingNonceAt() after deployment returned an error: %v", err)
	}
	if nextNonce != deploymentNonce+1 {
		t.Fatalf("sponsor nonce after deployment = %d, want %d", nextNonce, deploymentNonce+1)
	}
	return contractABI, rescuer, nextNonce
}

func localChainCoordinator(t *testing.T, sourceKey, sponsorKey *ecdsa.PrivateKey, destination, rescuer common.Address, policy string) (*Coordinator, *store.BoltStore) {
	t.Helper()
	ctx := context.Background()
	source := crypto.PubkeyToAddress(sourceKey.PublicKey)
	sponsor := crypto.PubkeyToAddress(sponsorKey.PublicKey)
	testClock := localChainClock{now: time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)}
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"), store.OpenOptions{
		Network:             localChainID,
		Source:              source,
		Sponsor:             sponsor,
		Destination:         destination,
		Rescuer:             rescuer,
		PolicyFingerprint:   sha256.Sum256([]byte("local-chain-" + policy + "-policy-v1")),
		MaxPending:          16,
		MaxDiscoveredTokens: 16,
		Clock:               testClock,
	})
	if err != nil {
		t.Fatalf("store.Open() returned an error: %v", err)
	}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Errorf("state.Close() error = %v", err)
		}
	})
	lease, err := state.Acquire(ctx, store.LeaseKey{Network: localChainID, Sponsor: sponsor}, "local-chain-"+policy, time.Hour)
	if err != nil {
		t.Fatalf("Acquire() returned an error: %v", err)
	}
	t.Cleanup(func() {
		if err := state.Release(context.Background(), lease); err != nil {
			t.Errorf("lease Release() error = %v", err)
		}
	})
	authorizer, err := NewPrivateKeyAuthorizationSigner(sourceKey)
	if err != nil {
		t.Fatalf("NewPrivateKeyAuthorizationSigner() returned an error: %v", err)
	}
	transactioner, err := NewPrivateKeyTransactionSigner(sponsorKey)
	if err != nil {
		t.Fatalf("NewPrivateKeyTransactionSigner() returned an error: %v", err)
	}
	coordinatorConfig := withTestEconomicPolicy(t, Config{
		Network: domain.Network{
			Name:       "deterministic-local-chain",
			ChainID:    localChainID,
			Rescuer:    rescuer,
			HasRescuer: true,
		},
		Source:          source,
		Sponsor:         sponsor,
		Destination:     destination,
		NativeThreshold: big.NewInt(0),
		StartupTimeout:  time.Second,
		FeeReadTimeout:  time.Second,
		State:           state,
		LeaseManager:    state,
		Lease:           lease,
		ProcessFence:    newFakeProcessFence(),
		LeaseTTL:        time.Hour,
		MaxAttempts:     1,
		RetryDelay:      time.Second,
		ReceiptTimeout:  time.Second,
	}, testClock)
	coordinator, err := NewCoordinator(coordinatorConfig, authorizer, transactioner, testClock, observability.Discard{})
	if err != nil {
		t.Fatalf("NewCoordinator() returned an error: %v", err)
	}
	return coordinator, state
}

func localChainCounter(t *testing.T, ctx context.Context, client simulated.Client, source common.Address) *big.Int {
	t.Helper()
	value, err := client.StorageAt(ctx, source, common.Hash{}, nil)
	if err != nil {
		t.Fatalf("StorageAt(source slot 0) returned an error: %v", err)
	}
	return new(big.Int).SetBytes(value)
}

func localChainPrivateKey(t *testing.T, encoded string) *ecdsa.PrivateKey {
	t.Helper()
	key, err := crypto.HexToECDSA(encoded)
	if err != nil {
		t.Fatalf("parsing deterministic test private key: %v", err)
	}
	return key
}

func localChainArtifact(t *testing.T) (abi.ABI, []byte) {
	t.Helper()
	data, err := os.ReadFile(localChainArtifactPath)
	if err != nil {
		t.Fatalf("reading canonical RescuerV2 artifact %q: %v", localChainArtifactPath, err)
	}
	var artifact struct {
		ABI      json.RawMessage `json:"abi"`
		Bytecode string          `json:"bytecode"`
	}
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("decoding canonical RescuerV2 artifact: %v", err)
	}
	contractABI, err := abi.JSON(bytes.NewReader(artifact.ABI))
	if err != nil {
		t.Fatalf("decoding RescuerV2 ABI: %v", err)
	}
	bytecode, err := hexutil.Decode(artifact.Bytecode)
	if err != nil {
		t.Fatalf("decoding RescuerV2 bytecode: %v", err)
	}
	if len(contractABI.Constructor.Inputs) != 2 || len(bytecode) == 0 {
		t.Fatal("canonical RescuerV2 artifact is missing constructor inputs or bytecode")
	}
	return contractABI, bytecode
}

func localChainDeploymentTransaction(t *testing.T, ctx context.Context, client simulated.Client, sponsorKey *ecdsa.PrivateKey, sponsor common.Address, chainID *big.Int, nonce uint64, data []byte) *types.Transaction {
	t.Helper()
	header, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		t.Fatalf("HeaderByNumber() during deployment returned an error: %v", err)
	}
	if header.BaseFee == nil {
		t.Fatal("deployment header has no base fee")
	}
	tip := big.NewInt(1_000_000_000)
	feeCap := new(big.Int).Add(new(big.Int).Mul(header.BaseFee, big.NewInt(2)), tip)
	gas, err := client.EstimateGas(ctx, ethereum.CallMsg{From: sponsor, GasTipCap: tip, GasFeeCap: feeCap, Data: data})
	if err != nil {
		t.Fatalf("estimating gas for RescuerV2 deployment: %v", err)
	}
	transaction := types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		GasTipCap: tip,
		GasFeeCap: feeCap,
		Gas:       gas,
		Data:      data,
	})
	signed, err := types.SignTx(transaction, types.LatestSignerForChainID(chainID), sponsorKey)
	if err != nil {
		t.Fatalf("signing RescuerV2 deployment: %v", err)
	}
	return signed
}

type localChainPrimary struct {
	client simulated.Client
}

func (reader *localChainPrimary) ChainID(ctx context.Context) (*big.Int, error) {
	return reader.client.ChainID(ctx)
}

func (reader *localChainPrimary) PendingNonceAt(ctx context.Context, account common.Address) (uint64, error) {
	return reader.client.PendingNonceAt(ctx, account)
}

func (reader *localChainPrimary) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	return reader.client.HeaderByNumber(ctx, number)
}

func (reader *localChainPrimary) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	return reader.client.SuggestGasPrice(ctx)
}

func (reader *localChainPrimary) EstimateGas(ctx context.Context, call ethereum.CallMsg) (uint64, error) {
	return reader.client.EstimateGas(ctx, call)
}

type localChainBroadcaster struct {
	client  simulated.Client
	backend *simulated.Backend

	mu          sync.Mutex
	transaction *types.Transaction
	blockHash   common.Hash
}

func (broadcaster *localChainBroadcaster) SendTransaction(ctx context.Context, transaction *types.Transaction) error {
	if err := broadcaster.client.SendTransaction(ctx, transaction); err != nil {
		return err
	}
	blockHash := broadcaster.backend.Commit()
	broadcaster.mu.Lock()
	broadcaster.transaction = transaction
	broadcaster.blockHash = blockHash
	broadcaster.mu.Unlock()
	return nil
}

func (broadcaster *localChainBroadcaster) Transaction() *types.Transaction {
	broadcaster.mu.Lock()
	defer broadcaster.mu.Unlock()
	return broadcaster.transaction
}

func (broadcaster *localChainBroadcaster) BlockHash() common.Hash {
	broadcaster.mu.Lock()
	defer broadcaster.mu.Unlock()
	return broadcaster.blockHash
}

type localChainRejectBroadcaster struct {
	mu    sync.Mutex
	calls int
}

func (broadcaster *localChainRejectBroadcaster) SendTransaction(context.Context, *types.Transaction) error {
	broadcaster.mu.Lock()
	broadcaster.calls++
	broadcaster.mu.Unlock()
	return errors.New("unexpected proactive local-chain transaction")
}

func (broadcaster *localChainRejectBroadcaster) Calls() int {
	broadcaster.mu.Lock()
	defer broadcaster.mu.Unlock()
	return broadcaster.calls
}

type localChainCompetitionEvidence struct {
	production              []*types.Transaction
	competing               *types.Transaction
	competingReceipt        *types.Receipt
	storageAfterCompeting   []byte
	productionCommittedHash common.Hash
}

type localChainCompetingBroadcaster struct {
	client            simulated.Client
	backend           *simulated.Backend
	source            common.Address
	sourceKey         *ecdsa.PrivateKey
	attacker          common.Address
	attackerKey       *ecdsa.PrivateKey
	maliciousDelegate common.Address

	mu       sync.Mutex
	evidence localChainCompetitionEvidence
}

func (broadcaster *localChainCompetingBroadcaster) SendTransaction(ctx context.Context, transaction *types.Transaction) error {
	broadcaster.mu.Lock()
	broadcaster.evidence.production = append(broadcaster.evidence.production, transaction)
	broadcaster.mu.Unlock()
	authorizations := transaction.SetCodeAuthorizations()
	if len(authorizations) != 1 {
		return fmt.Errorf("production authorization count = %d, want 1", len(authorizations))
	}
	competingAuthorization, err := types.SignSetCode(broadcaster.sourceKey, types.SetCodeAuthorization{
		ChainID: authorizations[0].ChainID,
		Address: broadcaster.maliciousDelegate,
		Nonce:   authorizations[0].Nonce,
	})
	if err != nil {
		return fmt.Errorf("sign competing authorization: %w", err)
	}
	attackerNonce, err := broadcaster.client.PendingNonceAt(ctx, broadcaster.attacker)
	if err != nil {
		return fmt.Errorf("attacker PendingNonceAt: %w", err)
	}
	competing := types.NewTx(&types.SetCodeTx{
		ChainID:   uint256.MustFromBig(transaction.ChainId()),
		Nonce:     attackerNonce,
		To:        broadcaster.attacker,
		Gas:       transaction.Gas(),
		GasTipCap: uint256.MustFromBig(transaction.GasTipCap()),
		GasFeeCap: uint256.MustFromBig(transaction.GasFeeCap()),
		AuthList:  []types.SetCodeAuthorization{competingAuthorization},
	})
	competing, err = types.SignTx(competing, types.LatestSignerForChainID(transaction.ChainId()), broadcaster.attackerKey)
	if err != nil {
		return fmt.Errorf("sign competing transaction: %w", err)
	}
	if competing.To() == nil || *competing.To() == broadcaster.source {
		return errors.New("competing outer transaction targets source")
	}
	if err := broadcaster.client.SendTransaction(ctx, competing); err != nil {
		return fmt.Errorf("send competing transaction: %w", err)
	}
	competingBlockHash := broadcaster.backend.Commit()
	competingReceipt, err := broadcaster.client.TransactionReceipt(ctx, competing.Hash())
	if err != nil {
		return fmt.Errorf("competing receipt: %w", err)
	}
	if competingReceipt.Status != types.ReceiptStatusSuccessful || competingReceipt.BlockHash != competingBlockHash {
		return fmt.Errorf("competing receipt status/block = %d/%s, want success/%s", competingReceipt.Status, competingReceipt.BlockHash, competingBlockHash)
	}
	storageAfterCompeting, err := broadcaster.client.StorageAt(ctx, broadcaster.source, common.Hash{}, nil)
	if err != nil {
		return fmt.Errorf("source storage after competing transaction: %w", err)
	}
	if err := broadcaster.client.SendTransaction(ctx, transaction); err != nil {
		return fmt.Errorf("send production transaction: %w", err)
	}
	productionCommittedHash := broadcaster.backend.Commit()
	broadcaster.mu.Lock()
	broadcaster.evidence.competing = competing
	broadcaster.evidence.competingReceipt = competingReceipt
	broadcaster.evidence.storageAfterCompeting = append([]byte(nil), storageAfterCompeting...)
	broadcaster.evidence.productionCommittedHash = productionCommittedHash
	broadcaster.mu.Unlock()
	return nil
}

func (broadcaster *localChainCompetingBroadcaster) Evidence() localChainCompetitionEvidence {
	broadcaster.mu.Lock()
	defer broadcaster.mu.Unlock()
	evidence := broadcaster.evidence
	evidence.production = append([]*types.Transaction(nil), evidence.production...)
	evidence.storageAfterCompeting = append([]byte(nil), evidence.storageAfterCompeting...)
	return evidence
}

type localChainFinality struct {
	client simulated.Client
}

func (reader *localChainFinality) Finalized(ctx context.Context) (guardrpc.BlockRef, error) {
	header, err := reader.client.HeaderByNumber(ctx, nil)
	if err != nil {
		return guardrpc.BlockRef{}, err
	}
	if header == nil || header.Number == nil || !header.Number.IsUint64() {
		return guardrpc.BlockRef{}, errors.New("local-chain latest header is invalid")
	}
	block := guardrpc.BlockRef{Number: header.Number.Uint64(), Hash: header.Hash(), ParentHash: header.ParentHash}
	if err := reader.requireCanonical(ctx, block); err != nil {
		return guardrpc.BlockRef{}, err
	}
	return block, nil
}

func (reader *localChainFinality) Header(ctx context.Context, number uint64) (guardrpc.BlockRef, error) {
	header, err := reader.client.HeaderByNumber(ctx, new(big.Int).SetUint64(number))
	if err != nil {
		return guardrpc.BlockRef{}, err
	}
	if header == nil || header.Number == nil || !header.Number.IsUint64() || header.Number.Uint64() != number {
		return guardrpc.BlockRef{}, errors.New("local-chain header is invalid")
	}
	return guardrpc.BlockRef{Number: number, Hash: header.Hash(), ParentHash: header.ParentHash}, nil
}

func (reader *localChainFinality) BalanceAt(ctx context.Context, block guardrpc.BlockRef, account common.Address) (*big.Int, error) {
	number, err := reader.canonicalNumber(ctx, block)
	if err != nil {
		return nil, err
	}
	balance, err := reader.client.BalanceAt(ctx, account, number)
	if err != nil {
		return nil, err
	}
	if err := reader.requireCanonical(ctx, block); err != nil {
		return nil, err
	}
	return balance, nil
}

func (reader *localChainFinality) CodeAt(ctx context.Context, block guardrpc.BlockRef, account common.Address) ([]byte, error) {
	number, err := reader.canonicalNumber(ctx, block)
	if err != nil {
		return nil, err
	}
	code, err := reader.client.CodeAt(ctx, account, number)
	if err != nil {
		return nil, err
	}
	if err := reader.requireCanonical(ctx, block); err != nil {
		return nil, err
	}
	return code, nil
}

func (reader *localChainFinality) CallContract(ctx context.Context, block guardrpc.BlockRef, call ethereum.CallMsg) ([]byte, error) {
	number, err := reader.canonicalNumber(ctx, block)
	if err != nil {
		return nil, err
	}
	result, err := reader.client.CallContract(ctx, call, number)
	if err != nil {
		return nil, err
	}
	if err := reader.requireCanonical(ctx, block); err != nil {
		return nil, err
	}
	return result, nil
}

func (reader *localChainFinality) Receipt(ctx context.Context, hash common.Hash) (*types.Receipt, error) {
	receipt, err := reader.client.TransactionReceipt(ctx, hash)
	if err != nil {
		return nil, err
	}
	if receipt == nil || receipt.BlockNumber == nil || !receipt.BlockNumber.IsUint64() || receipt.BlockHash == (common.Hash{}) {
		return nil, errors.New("local-chain receipt is invalid")
	}
	latest, err := reader.Finalized(ctx)
	if err != nil {
		return nil, err
	}
	receiptBlock := guardrpc.BlockRef{Number: receipt.BlockNumber.Uint64(), Hash: receipt.BlockHash}
	if receiptBlock.Number > latest.Number {
		return nil, fmt.Errorf("local-chain receipt block %d is newer than finalized block %d", receiptBlock.Number, latest.Number)
	}
	if err := reader.requireCanonical(ctx, receiptBlock); err != nil {
		return nil, err
	}
	if err := reader.requireCanonical(ctx, latest); err != nil {
		return nil, err
	}
	return receipt, nil
}

func (reader *localChainFinality) canonicalNumber(ctx context.Context, block guardrpc.BlockRef) (*big.Int, error) {
	if err := reader.requireCanonical(ctx, block); err != nil {
		return nil, err
	}
	return new(big.Int).SetUint64(block.Number), nil
}

func (reader *localChainFinality) requireCanonical(ctx context.Context, block guardrpc.BlockRef) error {
	if block.Hash == (common.Hash{}) {
		return errors.New("local-chain block reference has empty hash")
	}
	header, err := reader.client.HeaderByNumber(ctx, new(big.Int).SetUint64(block.Number))
	if err != nil {
		return err
	}
	if header == nil || header.Hash() != block.Hash {
		return fmt.Errorf("local-chain block %d is not canonical at hash %s", block.Number, block.Hash)
	}
	return nil
}

type localChainClock struct {
	now time.Time
}

func (serviceClock localChainClock) Now() time.Time {
	return serviceClock.now
}

func (localChainClock) Sleep(context.Context, time.Duration) error {
	return errLocalChainUnexpectedSleep
}

func (localChainClock) NewTicker(time.Duration) clock.Ticker {
	return nil
}

func (localChainClock) NewTimer(time.Duration) clock.Timer {
	return nil
}

var _ RPCReader = (*localChainPrimary)(nil)
var _ guardrpc.Broadcaster = (*localChainBroadcaster)(nil)
var _ FinalityReader = (*localChainFinality)(nil)
var _ clock.Clock = localChainClock{}
