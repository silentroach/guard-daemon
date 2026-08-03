package rescue

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"guard-daemon/internal/budget"
	"guard-daemon/internal/clock"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"
	"guard-daemon/internal/rpc"
	"guard-daemon/internal/store"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

func TestConcurrentNativeAndTokenCandidatesUseDistinctSponsorNonces(t *testing.T) {
	rig := newTestRig(t, rigOptions{tokens: []common.Address{testAddress(5)}, nativeThreshold: big.NewInt(0)})
	rig.chain.setToken(rig.tokens[0], rig.source, 7)
	rig.chain.setNative(rig.source, 1_000_000_000_000_000)

	candidates := []domain.RescueCandidate{rig.tokenCandidate(rig.tokens[0], 1), rig.nativeCandidate(2)}
	start := make(chan struct{})
	errorsByCandidate := make(chan error, len(candidates))
	var workers sync.WaitGroup
	for _, candidate := range candidates {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			errorsByCandidate <- rig.session.Handle(context.Background(), candidate)
		}()
	}
	close(start)
	workers.Wait()
	close(errorsByCandidate)
	for err := range errorsByCandidate {
		if err != nil {
			t.Fatalf("Handle() error = %v", err)
		}
	}

	transactions := rig.broadcaster.snapshot()
	if len(transactions) != 2 {
		t.Fatalf("broadcast count = %d, want 2", len(transactions))
	}
	nonces := []uint64{transactions[0].Nonce(), transactions[1].Nonce()}
	sort.Slice(nonces, func(i, j int) bool { return nonces[i] < nonces[j] })
	if nonces[0] != 5 || nonces[1] != 6 {
		t.Fatalf("sponsor nonces = %v, want [5 6]", nonces)
	}
}

func TestExternalSponsorNonceJumpReconcilesBeforeSigning(t *testing.T) {
	firstToken, secondToken := testAddress(5), testAddress(6)
	rig := newTestRig(t, rigOptions{tokens: []common.Address{firstToken, secondToken}})
	rig.chain.setToken(firstToken, rig.source, 7)
	rig.chain.setToken(secondToken, rig.source, 9)

	if err := rig.session.Handle(context.Background(), rig.tokenCandidate(firstToken, 1)); err != nil {
		t.Fatalf("first Handle() error = %v", err)
	}
	rig.primary.mu.Lock()
	rig.primary.sponsorNonce = 12
	rig.primary.mu.Unlock()
	if err := rig.session.Handle(context.Background(), rig.tokenCandidate(secondToken, 2)); err != nil {
		t.Fatalf("second Handle() error = %v", err)
	}

	transactions := rig.broadcaster.snapshot()
	if len(transactions) != 2 || transactions[0].Nonce() != 5 || transactions[1].Nonce() != 12 {
		t.Fatalf("sponsor nonces after external jump = %v, want [5 12]", transactionNonces(transactions))
	}
}

func TestAtomicNativeHasAuthorizationAndCallDataWithoutProactiveRenewal(t *testing.T) {
	rig := newTestRig(t, rigOptions{nativeThreshold: big.NewInt(0)})
	rig.chain.setNative(rig.source, 1_000_000_000_000_000)
	if len(rig.broadcaster.snapshot()) != 0 || rig.authorizer.count() != 0 {
		t.Fatal("NewSession performed proactive delegation renewal")
	}

	candidate := rig.nativeCandidate(1)
	if err := rig.session.Handle(context.Background(), candidate); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	transactions := rig.broadcaster.snapshot()
	if len(transactions) != 1 {
		t.Fatalf("broadcast count = %d, want 1", len(transactions))
	}
	transaction := transactions[0]
	if transaction.Type() != types.SetCodeTxType || transaction.To() == nil || *transaction.To() != rig.source || len(transaction.Data()) == 0 || len(transaction.SetCodeAuthorizations()) != 1 {
		t.Fatal("native rescue was not one atomic SetCode transaction")
	}
	incident := rig.incident(candidate, domain.CandidateNative, common.Address{})
	floor, _ := rig.state.NonceFloor(context.Background())
	if incident.Policy.FinalityTimeout < 30*time.Minute || len(incident.SignedTransaction) != 0 || floor != transaction.Nonce()+1 {
		t.Fatalf("terminal policy/payload/floor = %s/%d/%d", incident.Policy.FinalityTimeout, len(incident.SignedTransaction), floor)
	}
}

func TestAuthorizationFailureCannotCreateDelegationOnlyTransaction(t *testing.T) {
	rig := newTestRig(t, rigOptions{nativeThreshold: big.NewInt(0), authorizationError: errors.New("sign failed")})
	rig.chain.setNative(rig.source, 1_000_000_000_000_000)
	err := rig.session.Handle(context.Background(), rig.nativeCandidate(1))
	assertCode(t, err, codeSigning)
	if rig.transactioner.count() != 0 || len(rig.broadcaster.snapshot()) != 0 {
		t.Fatal("failed authorization produced a sponsor transaction")
	}
}

func TestSourceZeroWithoutDestinationDeltaIsNotSuccess(t *testing.T) {
	rig := newTestRig(t, rigOptions{tokens: []common.Address{testAddress(5)}, zeroSourceWithoutDelta: true})
	token := rig.tokens[0]
	rig.chain.setToken(token, rig.source, 9)
	candidate := rig.tokenCandidate(token, 1)
	err := rig.session.Handle(context.Background(), candidate)
	assertCode(t, err, codePostcondition)
	incident := rig.incident(candidate, domain.CandidateToken, token)
	if incident.Status != store.RescueLostRace {
		t.Fatalf("incident status = %v, want RescueLostRace", incident.Status)
	}
}

func TestSuccessfulReceiptWithPostconditionRPCErrorIsAmbiguous(t *testing.T) {
	rig := newTestRig(t, rigOptions{tokens: []common.Address{testAddress(5)}, postCodeError: errors.New("quorum unavailable")})
	token := rig.tokens[0]
	rig.chain.setToken(token, rig.source, 5)
	candidate := rig.tokenCandidate(token, 1)
	err := rig.session.Handle(context.Background(), candidate)
	assertAmbiguous(t, err)
	if status := rig.incident(candidate, domain.CandidateToken, token).Status; status != store.RescueAmbiguous {
		t.Fatalf("incident status = %v, want RescueAmbiguous", status)
	}
	snapshot, err := rig.coordinator.budget.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Global.Spent.Cumulative.IsZero() || !snapshot.Global.Reserved.Cumulative.IsZero() {
		t.Fatalf("finalized receipt cost was not committed before postcondition: %+v", snapshot.Global)
	}
	rig.clock.advance(time.Second)
	assertAmbiguous(t, rig.session.Handle(context.Background(), candidate))
	if status := rig.incident(candidate, domain.CandidateToken, token).Status; status != store.RescueAmbiguous {
		t.Fatalf("postcondition reconciliation status = %v, want RescueAmbiguous", status)
	}
}

func TestUnknownTokenNeverGetsTrustedSuccess(t *testing.T) {
	trusted := testAddress(5)
	unknown := testAddress(9)
	rig := newTestRig(t, rigOptions{tokens: []common.Address{trusted}, allowUnknown: true})
	rig.chain.setToken(unknown, rig.source, 4)
	candidate := domain.NewTokenReconciliationCandidate(rig.chainID, rig.source, domain.Token{Address: unknown, Symbol: "configured-looking", Decimals: 18}, 1, 1)
	if err := rig.session.Handle(context.Background(), candidate); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	incident := rig.incident(candidate, domain.CandidateToken, unknown)
	if incident.Trusted || incident.Status != store.RescueTokenReported {
		t.Fatalf("unknown incident trust/status = %t/%v, want false/RescueTokenReported", incident.Trusted, incident.Status)
	}
}

func TestConfiguredTokenOutcomeRemainsTokenReported(t *testing.T) {
	token := testAddress(5)
	rig := newTestRig(t, rigOptions{tokens: []common.Address{token}})
	rig.chain.setToken(token, rig.source, 4)
	candidate := rig.tokenCandidate(token, 1)
	if err := rig.session.Handle(context.Background(), candidate); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	incident := rig.incident(candidate, domain.CandidateToken, token)
	if !incident.Trusted || incident.Status != store.RescueTokenReported {
		t.Fatalf("configured token trust/status = %t/%v, want economic trust with token-reported outcome", incident.Trusted, incident.Status)
	}
}

func TestFakeTransferHintWithoutBalanceStopsBeforeSigning(t *testing.T) {
	token := testAddress(5)
	rig := newTestRig(t, rigOptions{tokens: []common.Address{token}})
	candidate := rig.tokenCandidate(token, 1)
	if err := rig.session.Handle(context.Background(), candidate); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	incident := rig.incident(candidate, domain.CandidateToken, token)
	if incident.Status != store.RescueFailed || incident.LastCode != codeNoPaidAction || rig.authorizer.count() != 0 || rig.transactioner.count() != 0 || len(rig.broadcaster.snapshot()) != 0 {
		t.Fatalf("fake hint status/code/paid actions = %v/%s/%d/%d/%d", incident.Status, incident.LastCode, rig.authorizer.count(), rig.transactioner.count(), len(rig.broadcaster.snapshot()))
	}
}

func TestExplicitlyAllowlistedUnknownTokenGetsOnlyTokenReportedOutcome(t *testing.T) {
	token := testAddress(7)
	rig := newTestRig(t, rigOptions{tokens: []common.Address{token}})
	delete(rig.coordinator.trustedTokens, token)
	delete(rig.coordinator.trustedTokenValues, token)
	rig.coordinator.allowedUntrustedTokens[token] = struct{}{}
	rig.chain.setToken(token, rig.source, 5)
	candidate := rig.tokenCandidate(token, 1)
	if err := rig.session.Handle(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	incident := rig.incident(candidate, domain.CandidateToken, token)
	if incident.Trusted || incident.Status != store.RescueTokenReported {
		t.Fatalf("allowlisted unknown trust/status = %t/%v", incident.Trusted, incident.Status)
	}
}

func TestExhaustedIncidentDoesNotBlockNewCandidate(t *testing.T) {
	rig := newTestRig(t, rigOptions{tokens: []common.Address{testAddress(5)}, headerError: errors.New("header unavailable")})
	token := rig.tokens[0]
	rig.chain.setToken(token, rig.source, 8)
	first := rig.tokenCandidate(token, 1)
	for attempt := 0; attempt < 3; attempt++ {
		assertCode(t, rig.session.Handle(context.Background(), first), codeFeeRead)
		rig.clock.advance(time.Second)
	}
	if err := rig.session.Handle(context.Background(), first); err != nil {
		t.Fatalf("exhausted replay error = %v", err)
	}
	if incident := rig.incident(first, domain.CandidateToken, token); incident.Status != store.RescueExhausted || incident.Attempts != 3 {
		t.Fatalf("first incident = status %v attempts %d", incident.Status, incident.Attempts)
	}

	second := rig.tokenCandidate(token, 2)
	assertCode(t, rig.session.Handle(context.Background(), second), codeFeeRead)
	if incident := rig.incident(second, domain.CandidateToken, token); incident.Status != store.RescueRetryable || incident.Attempts != 1 {
		t.Fatalf("new incident = status %v attempts %d", incident.Status, incident.Attempts)
	}
}

func TestPrestateFailuresAreDurableAndBounded(t *testing.T) {
	token := testAddress(5)
	tests := []struct {
		name    string
		options rigOptions
		kind    domain.CandidateKind
		code    domain.ErrorCode
	}{
		{name: "finalized", options: rigOptions{nativeThreshold: big.NewInt(0), finalizedErrorAfter: 1}, kind: domain.CandidateNative, code: codeFinalityRead},
		{name: "native balance", options: rigOptions{nativeThreshold: big.NewInt(0), preBalanceError: errors.New("balance unavailable")}, kind: domain.CandidateNative, code: codeBalanceRead},
		{name: "token decode", options: rigOptions{tokens: []common.Address{token}, invalidTokenBalance: true}, kind: domain.CandidateToken, code: codeBalanceDecode},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rig := newTestRig(t, test.options)
			var candidate domain.RescueCandidate
			if test.kind == domain.CandidateToken {
				candidate = rig.tokenCandidate(token, 1)
			} else {
				candidate = rig.nativeCandidate(1)
			}
			for attempt := 1; attempt <= 3; attempt++ {
				assertCode(t, rig.session.Handle(context.Background(), candidate), test.code)
				incident := rig.incident(candidate, test.kind, candidate.Token.Address)
				wantStatus := store.RescueRetryable
				if incident.Attempts != uint32(attempt) || incident.Status != wantStatus || incident.LastCode != test.code || incident.SnapshotBlockHash == (common.Hash{}) {
					t.Fatalf("attempt %d incident = attempts %d status %v code %s snapshot %s", attempt, incident.Attempts, incident.Status, incident.LastCode, incident.SnapshotBlockHash)
				}
				rig.clock.advance(time.Second)
			}
			if err := rig.session.Handle(context.Background(), candidate); err != nil {
				t.Fatalf("exhausted replay error = %v", err)
			}
			if incident := rig.incident(candidate, test.kind, candidate.Token.Address); incident.Status != store.RescueExhausted {
				t.Fatalf("terminal status = %v, want RescueExhausted", incident.Status)
			}
			if rig.authorizer.count() != 0 || rig.transactioner.count() != 0 || len(rig.broadcaster.snapshot()) != 0 {
				t.Fatal("prestate failure reached signing or broadcast")
			}
		})
	}
}

func TestSendErrorAndReceiptTimeoutPersistAmbiguous(t *testing.T) {
	tests := []struct {
		name    string
		options rigOptions
	}{
		{name: "send error", options: rigOptions{tokens: []common.Address{testAddress(5)}, sendError: errors.New("send unavailable")}},
		{name: "receipt timeout", options: rigOptions{tokens: []common.Address{testAddress(5)}, receiptError: errors.New("not finalized")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rig := newTestRig(t, test.options)
			token := rig.tokens[0]
			rig.chain.setToken(token, rig.source, 3)
			candidate := rig.tokenCandidate(token, 1)
			err := rig.session.Handle(context.Background(), candidate)
			assertAmbiguous(t, err)
			incident := rig.incident(candidate, domain.CandidateToken, token)
			if incident.Status != store.RescueAmbiguous || incident.TxHash == (common.Hash{}) {
				t.Fatalf("incident status/hash = %v/%s", incident.Status, incident.TxHash)
			}
			rig.clock.advance(time.Second)
			assertAmbiguous(t, rig.session.Handle(context.Background(), candidate))
			if status := rig.incident(candidate, domain.CandidateToken, token).Status; status != store.RescueAmbiguous {
				t.Fatalf("reconciliation status = %v, want RescueAmbiguous", status)
			}
		})
	}
}

func TestDelayedFinalitySucceedsOnReplayWithoutResigning(t *testing.T) {
	token := testAddress(5)
	rig := newTestRig(t, rigOptions{tokens: []common.Address{token}, receiptError: errors.New("not finalized")})
	rig.chain.setToken(token, rig.source, 7)
	candidate := rig.tokenCandidate(token, 1)
	assertAmbiguous(t, rig.session.Handle(context.Background(), candidate))
	if status := rig.incident(candidate, domain.CandidateToken, token).Status; status != store.RescueAmbiguous {
		t.Fatalf("first status = %v, want RescueAmbiguous", status)
	}
	rig.chain.mu.Lock()
	rig.chain.receiptError = nil
	rig.chain.mu.Unlock()
	rig.clock.advance(time.Second)
	if err := rig.session.Handle(context.Background(), candidate); err != nil {
		t.Fatalf("delayed finality replay error = %v", err)
	}
	incident := rig.incident(candidate, domain.CandidateToken, token)
	if incident.Status != store.RescueTokenReported || rig.authorizer.count() != 1 || rig.transactioner.count() != 1 || len(rig.broadcaster.snapshot()) != 1 {
		t.Fatalf("delayed outcome/sign/broadcast = %v/%d/%d/%d", incident.Status, rig.authorizer.count(), rig.transactioner.count(), len(rig.broadcaster.snapshot()))
	}
}

func TestReconcileDeadlineLeavesPayloadAmbiguous(t *testing.T) {
	token := testAddress(5)
	nextToken := testAddress(6)
	rig := newTestRig(t, rigOptions{tokens: []common.Address{token, nextToken}, receiptError: errors.New("not finalized")})
	rig.chain.setToken(token, rig.source, 5)
	candidate := rig.tokenCandidate(token, 1)
	assertAmbiguous(t, rig.session.Handle(context.Background(), candidate))
	incidentID := domain.NewAssetIncidentID(candidate.ID, domain.CandidateToken, token)
	incident := rig.incident(candidate, domain.CandidateToken, token)
	rig.state.mu.Lock()
	incident.Status = store.RescueBroadcast
	incident.LastCode = ""
	incident.RetryAt = time.Time{}
	rig.state.incidents[incidentID] = cloneTestIncident(incident)
	rig.state.mu.Unlock()
	rig.clock.advance(incident.ReconcileUntil.Sub(rig.clock.Now()) + time.Second)
	if err := rig.session.Handle(context.Background(), candidate); err != nil {
		t.Fatalf("expired reconciliation error = %v, want nil Ack", err)
	}
	incident = rig.incident(candidate, domain.CandidateToken, token)
	if incident.Status != store.RescueAmbiguous || len(incident.SignedTransaction) == 0 || incident.ReconcileUntil.IsZero() {
		t.Fatalf("expired durable state = %v payload %d deadline %s", incident.Status, len(incident.SignedTransaction), incident.ReconcileUntil)
	}
	rig.chain.setToken(nextToken, rig.source, 4)
	beforeSignatures := rig.transactioner.count()
	nextCandidate := rig.tokenCandidate(nextToken, 2)
	assertCode(t, rig.session.Handle(context.Background(), nextCandidate), codeRetryPending)
	if rig.transactioner.count() != beforeSignatures {
		t.Fatal("expired ambiguous nonce fence allowed a new signature")
	}
	if next := rig.incident(nextCandidate, domain.CandidateToken, nextToken); next.Status != store.RescuePending || next.Attempts != 0 {
		t.Fatalf("blocked candidate state = %v attempts %d", next.Status, next.Attempts)
	}
}

func TestRunReconciliationResolvesLateReceiptAndClearsNonceFence(t *testing.T) {
	firstToken, secondToken := testAddress(5), testAddress(6)
	rig := newTestRig(t, rigOptions{tokens: []common.Address{firstToken, secondToken}, sendError: errors.New("send outcome unknown")})
	rig.chain.setToken(firstToken, rig.source, 5)
	firstCandidate := rig.tokenCandidate(firstToken, 1)
	assertAmbiguous(t, rig.session.Handle(context.Background(), firstCandidate))
	firstIncident := rig.incident(firstCandidate, domain.CandidateToken, firstToken)
	rig.clock.advance(firstIncident.ReconcileUntil.Sub(rig.clock.Now()) + time.Second)

	ticker := &fakeTicker{ticks: make(chan time.Time, 2)}
	rig.clock.ticker = ticker
	updates := make(chan store.RescueStatus, 4)
	rig.state.mu.Lock()
	rig.state.updateHook = func(incident store.RescueIncident) {
		if incident.ID == firstIncident.ID {
			updates <- incident.Status
		}
	}
	rig.state.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- rig.session.RunReconciliation(ctx) }()

	ticker.ticks <- rig.clock.Now()
	if status := receiveRescueUpdate(t, updates); status != store.RescueAmbiguous {
		t.Fatalf("first sparse reconciliation status = %v, want RescueAmbiguous", status)
	}
	if got := len(rig.broadcaster.snapshot()); got != 1 {
		t.Fatalf("expired reconciliation broadcast count = %d, want 1", got)
	}

	rig.chain.setToken(secondToken, rig.source, 4)
	secondCandidate := rig.tokenCandidate(secondToken, 2)
	assertCode(t, rig.session.Handle(context.Background(), secondCandidate), codeRetryPending)
	if rig.transactioner.count() != 1 {
		t.Fatal("expired ambiguous operation did not retain the nonce fence")
	}

	rig.chain.accept(rig.broadcaster.snapshot()[0])
	ticker.ticks <- rig.clock.Now()
	if status := receiveRescueUpdate(t, updates); status != store.RescueTokenReported {
		t.Fatalf("late receipt status = %v, want RescueTokenReported", status)
	}
	if got := len(rig.broadcaster.snapshot()); got != 1 {
		t.Fatalf("late receipt reconciliation rebroadcast count = %d, want 1", got)
	}

	rig.broadcaster.setError(nil)
	if err := rig.session.Handle(context.Background(), secondCandidate); err != nil {
		t.Fatalf("next candidate after late receipt error = %v", err)
	}
	transactions := rig.broadcaster.snapshot()
	if len(transactions) != 2 || transactions[1].Nonce() != firstIncident.SponsorNonce+1 {
		t.Fatalf("next transactions/nonces = %v, want next nonce %d", transactionNonces(transactions), firstIncident.SponsorNonce+1)
	}

	cancel()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("RunReconciliation() cancellation error = %v", err)
	}
}

func TestRunReconciliationFailsOnFenceLossAndCorruptPayload(t *testing.T) {
	t.Run("process fence", func(t *testing.T) {
		rig := newTestRig(t, rigOptions{})
		ticker := &fakeTicker{ticks: make(chan time.Time, 1)}
		rig.clock.ticker = ticker
		rig.fence.lose()
		done := make(chan error, 1)
		go func() { done <- rig.session.RunReconciliation(context.Background()) }()
		ticker.ticks <- rig.clock.Now()
		if err := <-done; !IsLeaseLost(err) || !errors.Is(err, store.ErrFenceLost) {
			t.Fatalf("RunReconciliation() fence error = %v", err)
		}
	})

	t.Run("signed payload", func(t *testing.T) {
		token := testAddress(5)
		rig := newTestRig(t, rigOptions{tokens: []common.Address{token}, sendError: errors.New("send outcome unknown")})
		rig.chain.setToken(token, rig.source, 5)
		candidate := rig.tokenCandidate(token, 1)
		assertAmbiguous(t, rig.session.Handle(context.Background(), candidate))
		incident := rig.incident(candidate, domain.CandidateToken, token)
		rig.clock.advance(incident.ReconcileUntil.Sub(rig.clock.Now()) + time.Second)
		incident.SignedTransaction[0] ^= 0xff
		rig.state.mu.Lock()
		rig.state.incidents[incident.ID] = cloneTestIncident(incident)
		rig.state.mu.Unlock()
		ticker := &fakeTicker{ticks: make(chan time.Time, 1)}
		rig.clock.ticker = ticker
		done := make(chan error, 1)
		go func() { done <- rig.session.RunReconciliation(context.Background()) }()
		ticker.ticks <- rig.clock.Now()
		if err := <-done; errorCode(err) != codeSignedPayloadInvalid {
			t.Fatalf("RunReconciliation() corrupt payload error = %v", err)
		}
		if got := len(rig.broadcaster.snapshot()); got != 1 {
			t.Fatalf("corrupt payload reconciliation broadcast count = %d, want 1", got)
		}
	})
}

func TestReconciliationIntervalIsSparseAndBounded(t *testing.T) {
	tests := []struct {
		retry time.Duration
		want  time.Duration
	}{
		{retry: time.Millisecond, want: 30 * time.Second},
		{retry: 2 * time.Minute, want: 2 * time.Minute},
		{retry: time.Duration(1<<63 - 1), want: 5 * time.Minute},
	}
	for _, test := range tests {
		if got := reconciliationInterval(test.retry); got != test.want {
			t.Fatalf("reconciliationInterval(%s) = %s, want %s", test.retry, got, test.want)
		}
	}
}

func TestSignerCryptographicIdentityIsEnforced(t *testing.T) {
	token := testAddress(5)
	tests := []struct {
		name   string
		change func(*testRig)
	}{
		{name: "authorization authority", change: func(rig *testRig) { rig.authorizer.key = testPrivateKey(9) }},
		{name: "transaction sender", change: func(rig *testRig) { rig.transactioner.key = testPrivateKey(9) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rig := newTestRig(t, rigOptions{tokens: []common.Address{token}})
			rig.chain.setToken(token, rig.source, 5)
			test.change(rig)
			assertCode(t, rig.session.Handle(context.Background(), rig.tokenCandidate(token, 1)), codeSignerMismatch)
			if len(rig.broadcaster.snapshot()) != 0 {
				t.Fatal("wrong signer identity reached broadcast")
			}
		})
	}
}

func TestMaxNoncesFailBeforeSigning(t *testing.T) {
	token := testAddress(5)
	tests := []struct {
		name   string
		change func(*fakePrimary)
	}{
		{name: "sponsor", change: func(primary *fakePrimary) { primary.sponsorNonce = ^uint64(0) }},
		{name: "source", change: func(primary *fakePrimary) { primary.sourceNonce = ^uint64(0) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rig := newTestRig(t, rigOptions{tokens: []common.Address{token}})
			rig.chain.setToken(token, rig.source, 5)
			test.change(rig.primary)
			candidate := rig.tokenCandidate(token, 1)
			assertCode(t, rig.session.Handle(context.Background(), candidate), codeNonceRead)
			incident := rig.incident(candidate, domain.CandidateToken, token)
			if incident.Status != store.RescueRetryable || rig.authorizer.count() != 0 || rig.transactioner.count() != 0 {
				t.Fatalf("max nonce status/signatures = %v/%d/%d", incident.Status, rig.authorizer.count(), rig.transactioner.count())
			}
		})
	}
}

func TestUnchangedSourceWithDestinationDeltaIsNotSuccess(t *testing.T) {
	token := testAddress(5)
	rig := newTestRig(t, rigOptions{tokens: []common.Address{token}, keepSourceAfter: true})
	rig.chain.setToken(token, rig.source, 5)
	candidate := rig.tokenCandidate(token, 1)
	assertCode(t, rig.session.Handle(context.Background(), candidate), codePostcondition)
	if status := rig.incident(candidate, domain.CandidateToken, token).Status; status != store.RescueLostRace {
		t.Fatalf("status = %v, want RescueLostRace", status)
	}
}

func TestCanonicalRechecksBlockReorgAndFinalityChange(t *testing.T) {
	token := testAddress(5)
	tests := []struct {
		name    string
		options rigOptions
	}{
		{name: "snapshot reorg", options: rigOptions{tokens: []common.Address{token}, headerMismatchNumber: 10, headerMismatchAfter: 1}},
		{name: "finality changed during balances", options: rigOptions{tokens: []common.Address{token}, advanceDuringPostRead: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rig := newTestRig(t, test.options)
			rig.chain.setToken(token, rig.source, 5)
			candidate := rig.tokenCandidate(token, 1)
			assertAmbiguous(t, rig.session.Handle(context.Background(), candidate))
			if status := rig.incident(candidate, domain.CandidateToken, token).Status; status != store.RescueAmbiguous {
				t.Fatalf("status = %v, want RescueAmbiguous", status)
			}
		})
	}
}

func TestFinalizedRevertIsNonRetryableAndClearsPayload(t *testing.T) {
	token := testAddress(5)
	failed := uint64(types.ReceiptStatusFailed)
	rig := newTestRig(t, rigOptions{tokens: []common.Address{token}, receiptStatus: &failed})
	rig.chain.setToken(token, rig.source, 5)
	candidate := rig.tokenCandidate(token, 1)
	err := rig.session.Handle(context.Background(), candidate)
	assertCode(t, err, codeReverted)
	var classified *domain.ClassifiedError
	if !errors.As(err, &classified) || classified.Retryable {
		t.Fatalf("revert classification = %#v", classified)
	}
	incident := rig.incident(candidate, domain.CandidateToken, token)
	if incident.Status != store.RescueFailed || len(incident.SignedTransaction) != 0 || !incident.ReconcileUntil.IsZero() {
		t.Fatalf("revert durable state = %v payload %d deadline %s", incident.Status, len(incident.SignedTransaction), incident.ReconcileUntil)
	}
}

func TestTerminalPruneFailureNacksOnceWithoutResigning(t *testing.T) {
	token := testAddress(5)
	state := newMemoryState()
	state.pruneError = errors.New("storage pressure")
	rig := newTestRig(t, rigOptions{tokens: []common.Address{token}, state: state})
	rig.chain.setToken(token, rig.source, 5)
	candidate := rig.tokenCandidate(token, 1)
	assertCode(t, rig.session.Handle(context.Background(), candidate), codeStateWrite)
	if status := rig.incident(candidate, domain.CandidateToken, token).Status; status != store.RescueTokenReported {
		t.Fatalf("persisted terminal status = %v", status)
	}
	state.mu.Lock()
	state.pruneError = nil
	state.mu.Unlock()
	if err := rig.session.Handle(context.Background(), candidate); err != nil {
		t.Fatalf("terminal replay prune error = %v", err)
	}
	if rig.authorizer.count() != 1 || rig.transactioner.count() != 1 {
		t.Fatal("terminal prune replay resigned transaction")
	}
}

func TestRestartRebroadcastsExactSignedTransactionWithoutSigning(t *testing.T) {
	token := testAddress(5)
	state := newMemoryState()
	serviceClock := newFakeClock()
	first := newTestRig(t, rigOptions{tokens: []common.Address{token}, state: state, clock: serviceClock, sendError: errors.New("process crashed before send result")})
	first.chain.setToken(token, first.source, 5)
	candidate := first.tokenCandidate(token, 1)
	assertAmbiguous(t, first.session.Handle(context.Background(), candidate))
	incidentID := domain.NewAssetIncidentID(candidate.ID, domain.CandidateToken, token)
	incident := first.incident(candidate, domain.CandidateToken, token)
	wantPayload := append([]byte(nil), incident.SignedTransaction...)
	wantHash := incident.TxHash
	state.mu.Lock()
	incident.Status = store.RescueSigned
	incident.LastCode = ""
	incident.RetryAt = time.Time{}
	state.incidents[incidentID] = cloneTestIncident(incident)
	state.nonceFloor = 0
	state.mu.Unlock()

	restarted := newTestRig(t, rigOptions{tokens: []common.Address{token}, state: state, clock: serviceClock, receiptError: errors.New("still not finalized")})
	transactions := restarted.broadcaster.snapshot()
	if len(transactions) != 1 {
		t.Fatalf("restart broadcast count = %d, want 1", len(transactions))
	}
	gotPayload, err := transactions[0].MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary() error = %v", err)
	}
	if !bytes.Equal(gotPayload, wantPayload) || transactions[0].Hash() != wantHash {
		t.Fatalf("restart payload/hash changed: hash %s want %s", transactions[0].Hash(), wantHash)
	}
	if restarted.authorizer.count() != 0 || restarted.transactioner.count() != 0 {
		t.Fatal("restart rebroadcast called a signer")
	}
	floor, _ := state.NonceFloor(context.Background())
	if floor != incident.SponsorNonce+1 {
		t.Fatalf("recovery nonce floor = %d, want %d", floor, incident.SponsorNonce+1)
	}
}

func TestPersistedNonceFloorSurvivesRestart(t *testing.T) {
	newToken := testAddress(6)
	state := newMemoryState()
	state.nonceFloor = 13
	rig := newTestRig(t, rigOptions{tokens: []common.Address{newToken}, state: state})
	rig.chain.setToken(newToken, rig.source, 6)
	if err := rig.session.Handle(context.Background(), rig.tokenCandidate(newToken, 2)); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	transactions := rig.broadcaster.snapshot()
	if len(transactions) != 1 || transactions[0].Nonce() != 13 {
		t.Fatalf("restart sponsor nonce = %v, want 13", transactionNonces(transactions))
	}
}

func TestCoordinatorRequiresAndValidatesProcessFence(t *testing.T) {
	rig := newTestRig(t, rigOptions{})
	config := func(fence store.ProcessFence) Config {
		return withTestEconomicPolicy(t, Config{
			Network: rig.coordinator.network, Source: rig.source, Sponsor: rig.sponsor, Destination: rig.destination,
			State: rig.state, LeaseManager: rig.lease, Lease: rig.coordinator.currentLease(), ProcessFence: fence,
			LeaseTTL: time.Second, RetryDelay: time.Millisecond, ReceiptTimeout: time.Millisecond,
		}, rig.clock)
	}
	if coordinator, err := NewCoordinator(config(nil), rig.authorizer, rig.transactioner, rig.clock, observability.Discard{}); coordinator != nil || errorCode(err) != codeInvalidConfig {
		t.Fatalf("NewCoordinator() without process fence = (%v, %v)", coordinator, err)
	}

	lostFence := newFakeProcessFence()
	lostFence.lose()
	coordinator, err := NewCoordinator(config(lostFence), rig.authorizer, rig.transactioner, rig.clock, observability.Discard{})
	if coordinator != nil || !errors.Is(err, store.ErrFenceLost) || !IsLeaseLost(err) || lostFence.validationCount() != 1 {
		t.Fatalf("NewCoordinator() with lost process fence = (%v, %v), validations=%d", coordinator, err, lostFence.validationCount())
	}
}

func TestProcessFencePrecedesLeaseInSessionAndSigningGuards(t *testing.T) {
	fence := newFakeProcessFence()
	rig := newTestRig(t, rigOptions{fence: fence})
	if fence.validationCount() < 2 {
		t.Fatalf("constructor/session process fence validations = %d", fence.validationCount())
	}
	fence.lose()
	leaseValidations := rig.lease.validationCount()
	if err := rig.coordinator.guardSigning(context.Background()); !errors.Is(err, store.ErrFenceLost) || !IsLeaseLost(err) {
		t.Fatalf("guardSigning() after process fence loss = %v", err)
	}
	if rig.lease.validationCount() != leaseValidations {
		t.Fatal("guardSigning() validated persistent lease after process fence loss")
	}

	_, err := rig.coordinator.NewSession(context.Background(), 2, rig.primary, rig.chain, rig.broadcaster)
	if !errors.Is(err, store.ErrFenceLost) || rig.lease.validationCount() != leaseValidations {
		t.Fatalf("NewSession() after process fence loss = %v, lease validations=%d", err, rig.lease.validationCount())
	}
}

func TestProcessFenceLossClosesPostSignerAndSendWindows(t *testing.T) {
	token := testAddress(5)
	t.Run("after transaction signer", func(t *testing.T) {
		fence := newFakeProcessFence()
		rig := newTestRig(t, rigOptions{tokens: []common.Address{token}, fence: fence})
		rig.chain.setToken(token, rig.source, 5)
		rig.transactioner.afterSign = fence.lose
		candidate := rig.tokenCandidate(token, 1)
		err := rig.session.Handle(context.Background(), candidate)
		if !errors.Is(err, store.ErrFenceLost) || !IsLeaseLost(err) {
			t.Fatalf("Handle() after signer fence loss = %v", err)
		}
		incident := rig.incident(candidate, domain.CandidateToken, token)
		if incident.Status != store.RescueRetryable || incident.TxHash != (common.Hash{}) || len(incident.SignedTransaction) != 0 || len(rig.broadcaster.snapshot()) != 0 {
			t.Fatalf("post-signer fence loss state = %v hash %s payload %d broadcasts %d", incident.Status, incident.TxHash, len(incident.SignedTransaction), len(rig.broadcaster.snapshot()))
		}
	})

	t.Run("after signed persistence before send", func(t *testing.T) {
		fence := newFakeProcessFence()
		state := newMemoryState()
		rig := newTestRig(t, rigOptions{tokens: []common.Address{token}, fence: fence, state: state})
		rig.chain.setToken(token, rig.source, 5)
		state.mu.Lock()
		state.raiseHook = fence.lose
		state.mu.Unlock()
		candidate := rig.tokenCandidate(token, 1)
		err := rig.session.Handle(context.Background(), candidate)
		if !errors.Is(err, store.ErrFenceLost) || !IsLeaseLost(err) {
			t.Fatalf("Handle() before send fence loss = %v", err)
		}
		incident := rig.incident(candidate, domain.CandidateToken, token)
		if incident.Status != store.RescueSigned || incident.LastCode != "" || len(incident.SignedTransaction) == 0 || len(rig.broadcaster.snapshot()) != 0 {
			t.Fatalf("pre-send fence loss state = %v code %s payload %d broadcasts %d", incident.Status, incident.LastCode, len(incident.SignedTransaction), len(rig.broadcaster.snapshot()))
		}
	})
}

func TestReplacedProcessFenceStopsExistingCoordinatorBeforeSend(t *testing.T) {
	token := testAddress(5)
	rig := newTestRig(t, rigOptions{tokens: []common.Address{token}})
	chainID := domain.NetworkID(time.Now().UnixNano() & int64(^uint64(0)>>1))
	if chainID == 0 {
		chainID = 1
	}
	key := store.LeaseKey{Network: chainID, Sponsor: rig.sponsor}
	fence, err := store.AcquireProcessFence(key)
	if err != nil {
		t.Fatal(err)
	}
	defer fence.Release()

	network := rig.coordinator.network
	network.ChainID = chainID
	lease := rig.coordinator.currentLease()
	lease.Key = key
	coordinatorConfig := withTestEconomicPolicy(t, Config{
		Network: network, Source: rig.source, Sponsor: rig.sponsor, Destination: rig.destination,
		State: rig.state, LeaseManager: rig.lease, Lease: lease, ProcessFence: fence,
		LeaseTTL: time.Second, RetryDelay: time.Millisecond, ReceiptTimeout: time.Millisecond,
	}, rig.clock)
	coordinator, err := NewCoordinator(coordinatorConfig, rig.authorizer, rig.transactioner, rig.clock, observability.Discard{})
	if err != nil {
		t.Fatal(err)
	}
	primary := &fakePrimary{chainID: big.NewInt(int64(chainID)), source: rig.source, sponsor: rig.sponsor, sponsorNonce: 5, sourceNonce: 9}
	session, err := coordinator.NewSession(context.Background(), 1, primary, rig.chain, rig.broadcaster)
	if err != nil {
		t.Fatal(err)
	}
	rig.chain.setToken(token, rig.source, 5)

	if err := os.Remove(externalProcessFencePath(key)); err != nil {
		t.Fatal(err)
	}
	replacement, err := store.AcquireProcessFence(key)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Release()
	if err := fence.Validate(); !errors.Is(err, store.ErrFenceLost) {
		t.Fatalf("old fence Validate() after replacement = %v", err)
	}
	candidate := domain.NewTokenReconciliationCandidate(chainID, rig.source, domain.Token{Address: token}, 1, 1)
	err = session.Handle(context.Background(), candidate)
	if !errors.Is(err, store.ErrFenceLost) || !IsLeaseLost(err) || len(rig.broadcaster.snapshot()) != 0 {
		t.Fatalf("Handle() with replaced process fence = %v, broadcasts=%d", err, len(rig.broadcaster.snapshot()))
	}
}

func TestLeaseContentionAndLossBlockSigning(t *testing.T) {
	t.Run("contention", func(t *testing.T) {
		lease := newFakeLease()
		lease.valid = false
		_, err := buildTestRig(t, rigOptions{lease: lease})
		if !IsLeaseLost(err) {
			t.Fatalf("NewSession() error = %v, want lease loss", err)
		}
	})

	t.Run("loss before signing", func(t *testing.T) {
		lease := newFakeLease()
		rig := newTestRig(t, rigOptions{tokens: []common.Address{testAddress(5)}, lease: lease})
		token := rig.tokens[0]
		rig.chain.setToken(token, rig.source, 5)
		lease.lose()
		err := rig.session.Handle(context.Background(), rig.tokenCandidate(token, 1))
		if !IsLeaseLost(err) || rig.authorizer.count() != 0 || rig.transactioner.count() != 0 {
			t.Fatalf("lease loss error/signatures = %v/%d/%d", err, rig.authorizer.count(), rig.transactioner.count())
		}
	})
}

func TestLeaseLossClosesPostSignerAndSendWindows(t *testing.T) {
	token := testAddress(5)
	t.Run("during transaction signer", func(t *testing.T) {
		lease := newFakeLease()
		rig := newTestRig(t, rigOptions{tokens: []common.Address{token}, lease: lease})
		rig.chain.setToken(token, rig.source, 5)
		rig.transactioner.afterSign = lease.lose
		candidate := rig.tokenCandidate(token, 1)
		err := rig.session.Handle(context.Background(), candidate)
		if !IsLeaseLost(err) {
			t.Fatalf("Handle() error = %v, want lease loss", err)
		}
		incident := rig.incident(candidate, domain.CandidateToken, token)
		if incident.Status != store.RescueRetryable || incident.TxHash != (common.Hash{}) || len(incident.SignedTransaction) != 0 || len(rig.broadcaster.snapshot()) != 0 {
			t.Fatalf("post-signer loss state = %v hash %s payload %d broadcasts %d", incident.Status, incident.TxHash, len(incident.SignedTransaction), len(rig.broadcaster.snapshot()))
		}
	})

	t.Run("after signed persistence before initial send", func(t *testing.T) {
		lease := newFakeLease()
		state := newMemoryState()
		rig := newTestRig(t, rigOptions{tokens: []common.Address{token}, lease: lease, state: state})
		rig.chain.setToken(token, rig.source, 5)
		state.mu.Lock()
		state.raiseHook = lease.lose
		state.mu.Unlock()
		candidate := rig.tokenCandidate(token, 1)
		err := rig.session.Handle(context.Background(), candidate)
		if !IsLeaseLost(err) {
			t.Fatalf("Handle() error = %v, want lease loss", err)
		}
		incident := rig.incident(candidate, domain.CandidateToken, token)
		if incident.Status != store.RescueSigned || incident.LastCode != "" || len(incident.SignedTransaction) == 0 || len(rig.broadcaster.snapshot()) != 0 {
			t.Fatalf("pre-send loss state = %v code %s payload %d broadcasts %d", incident.Status, incident.LastCode, len(incident.SignedTransaction), len(rig.broadcaster.snapshot()))
		}
	})

	t.Run("before exact rebroadcast", func(t *testing.T) {
		state := newMemoryState()
		serviceClock := newFakeClock()
		first := newTestRig(t, rigOptions{tokens: []common.Address{token}, state: state, clock: serviceClock, sendError: errors.New("send unknown")})
		first.chain.setToken(token, first.source, 5)
		assertAmbiguous(t, first.session.Handle(context.Background(), first.tokenCandidate(token, 1)))

		lease := newFakeLease()
		state.mu.Lock()
		state.raiseHook = lease.lose
		state.mu.Unlock()
		restarted := newTestRig(t, rigOptions{tokens: []common.Address{token}, state: state, clock: serviceClock, lease: lease, receiptError: errors.New("not finalized")})
		if len(restarted.broadcaster.snapshot()) != 0 || restarted.authorizer.count() != 0 || restarted.transactioner.count() != 0 {
			t.Fatalf("lease-lost recovery broadcast/signatures = %d/%d/%d", len(restarted.broadcaster.snapshot()), restarted.authorizer.count(), restarted.transactioner.count())
		}
		incident := restarted.incident(first.tokenCandidate(token, 1), domain.CandidateToken, token)
		if incident.Status != store.RescueAmbiguous || incident.LastCode != CodeLeaseLost {
			t.Fatalf("rebroadcast lease state = %v code %s", incident.Status, incident.LastCode)
		}
	})
}

func TestNonceFenceDistinguishesSendErrorFromAcceptedTimeout(t *testing.T) {
	firstToken, secondToken := testAddress(5), testAddress(6)
	t.Run("exact send error blocks", func(t *testing.T) {
		rig := newTestRig(t, rigOptions{tokens: []common.Address{firstToken, secondToken}, sendError: errors.New("send unknown")})
		rig.chain.setToken(firstToken, rig.source, 5)
		rig.chain.setToken(secondToken, rig.source, 4)
		assertAmbiguous(t, rig.session.Handle(context.Background(), rig.tokenCandidate(firstToken, 1)))
		before := rig.transactioner.count()
		second := rig.tokenCandidate(secondToken, 2)
		assertCode(t, rig.session.Handle(context.Background(), second), codeRetryPending)
		if rig.transactioner.count() != before {
			t.Fatal("send-error nonce fence allowed next signature")
		}
		if incident := rig.incident(second, domain.CandidateToken, secondToken); incident.Status != store.RescuePending || incident.Attempts != 0 {
			t.Fatalf("blocked second incident = %v attempts %d", incident.Status, incident.Attempts)
		}
	})

	t.Run("accepted send timeout does not block", func(t *testing.T) {
		rig := newTestRig(t, rigOptions{tokens: []common.Address{firstToken, secondToken}, receiptError: errors.New("not finalized")})
		rig.chain.setToken(firstToken, rig.source, 5)
		rig.chain.setToken(secondToken, rig.source, 4)
		assertAmbiguous(t, rig.session.Handle(context.Background(), rig.tokenCandidate(firstToken, 1)))
		assertAmbiguous(t, rig.session.Handle(context.Background(), rig.tokenCandidate(secondToken, 2)))
		if rig.transactioner.count() != 2 {
			t.Fatalf("accepted timeout signature count = %d, want 2", rig.transactioner.count())
		}
	})
}

func TestRestartReceiptClearsExpiredNonceFence(t *testing.T) {
	firstToken, secondToken := testAddress(5), testAddress(6)
	state := newMemoryState()
	serviceClock := newFakeClock()
	first := newTestRig(t, rigOptions{tokens: []common.Address{firstToken, secondToken}, state: state, clock: serviceClock, receiptError: errors.New("not finalized")})
	first.chain.setToken(firstToken, first.source, 5)
	firstCandidate := first.tokenCandidate(firstToken, 1)
	assertAmbiguous(t, first.session.Handle(context.Background(), firstCandidate))
	incident := first.incident(firstCandidate, domain.CandidateToken, firstToken)
	serviceClock.advance(incident.ReconcileUntil.Sub(serviceClock.Now()) + time.Second)
	if err := first.session.Handle(context.Background(), firstCandidate); err != nil {
		t.Fatalf("expired Handle() error = %v", err)
	}
	first.chain.mu.Lock()
	first.chain.receiptError = nil
	first.chain.mu.Unlock()

	restarted := newTestRig(t, rigOptions{tokens: []common.Address{firstToken, secondToken}, state: state, clock: serviceClock, chain: first.chain, budget: first.coordinator.budget})
	if status := restarted.incident(firstCandidate, domain.CandidateToken, firstToken).Status; status != store.RescueTokenReported {
		t.Fatalf("recovered old status = %v, want RescueTokenReported", status)
	}
	restarted.chain.setToken(secondToken, restarted.source, 4)
	if err := restarted.session.Handle(context.Background(), restarted.tokenCandidate(secondToken, 2)); err != nil {
		t.Fatalf("next candidate after receipt proof error = %v", err)
	}
	transactions := restarted.broadcaster.snapshot()
	if len(transactions) != 1 || transactions[0].Nonce() != incident.SponsorNonce+1 {
		t.Fatalf("next transactions/nonces = %v, want one nonce %d", transactionNonces(transactions), incident.SponsorNonce+1)
	}
}

func TestReleaseLeaseUsesLatestLeaseAndStopsSigning(t *testing.T) {
	leaseManager := newFakeLease()
	rig := newTestRig(t, rigOptions{lease: leaseManager})
	renewed := rig.coordinator.currentLease()
	renewed.ExpiresAt = time.Unix(5, 0)
	rig.coordinator.leaseMu.Lock()
	rig.coordinator.lease = renewed
	rig.coordinator.leaseMu.Unlock()

	if err := rig.coordinator.ReleaseLease(context.Background()); err != nil {
		t.Fatalf("ReleaseLease() error = %v", err)
	}
	if released := leaseManager.releasedLease(); released != renewed {
		t.Fatalf("released lease = %#v, want renewed %#v", released, renewed)
	}
	if rig.fence.releaseCount() != 0 {
		t.Fatal("ReleaseLease() released daemon-owned process fence")
	}
	if err := rig.coordinator.guardSigning(context.Background()); !IsLeaseLost(err) {
		t.Fatalf("guardSigning() error = %v, want permanent stop", err)
	}
}

func TestReleaseLeaseFailureIsSafelyClassified(t *testing.T) {
	leaseManager := newFakeLease()
	leaseManager.releaseError = errors.New("private store detail")
	rig := newTestRig(t, rigOptions{lease: leaseManager})
	err := rig.coordinator.ReleaseLease(context.Background())
	assertCode(t, err, CodeLeaseReleaseFailed)
	if !errors.Is(err, ErrLeaseReleaseFailed) || errors.Is(err, leaseManager.releaseError) {
		t.Fatalf("ReleaseLease() error classification exposed unsafe cause: %v", err)
	}
}

func TestMaintainLeaseCancellationDoesNotMarkLeaseLost(t *testing.T) {
	t.Run("before loop", func(t *testing.T) {
		rig := newTestRig(t, rigOptions{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := rig.coordinator.MaintainLease(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("MaintainLease() error = %v, want context cancellation", err)
		}
		if err := rig.coordinator.guardSigning(context.Background()); err != nil {
			t.Fatalf("cancellation marked lease lost: %v", err)
		}
	})

	t.Run("during renewal", func(t *testing.T) {
		leaseManager := newFakeLease()
		rig := newTestRig(t, rigOptions{lease: leaseManager})
		ticker := &fakeTicker{ticks: make(chan time.Time, 1)}
		rig.clock.ticker = ticker
		ctx, cancel := context.WithCancel(context.Background())
		leaseManager.renewCall = func() error {
			cancel()
			return context.Canceled
		}
		result := make(chan error, 1)
		go func() { result <- rig.coordinator.MaintainLease(ctx) }()
		ticker.ticks <- time.Now()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("MaintainLease() error = %v, want renewal cancellation", err)
		}
		if err := rig.coordinator.guardSigning(context.Background()); err != nil {
			t.Fatalf("renewal cancellation marked lease lost: %v", err)
		}
	})
}

func TestAttackerAuthorizationRaceCodeMismatchIsLostRace(t *testing.T) {
	rig := newTestRig(t, rigOptions{tokens: []common.Address{testAddress(5)}, delegatedTarget: testAddress(99)})
	token := rig.tokens[0]
	rig.chain.setToken(token, rig.source, 5)
	candidate := rig.tokenCandidate(token, 1)
	assertCode(t, rig.session.Handle(context.Background(), candidate), codeLostRace)
	if status := rig.incident(candidate, domain.CandidateToken, token).Status; status != store.RescueLostRace {
		t.Fatalf("incident status = %v, want RescueLostRace", status)
	}
}

func TestFinalityOrReorgErrorCannotProduceSuccess(t *testing.T) {
	rig := newTestRig(t, rigOptions{tokens: []common.Address{testAddress(5)}, finalizedErrorAt: 8})
	token := rig.tokens[0]
	rig.chain.setToken(token, rig.source, 5)
	candidate := rig.tokenCandidate(token, 1)
	assertAmbiguous(t, rig.session.Handle(context.Background(), candidate))
	if status := rig.incident(candidate, domain.CandidateToken, token).Status; status != store.RescueAmbiguous {
		t.Fatalf("incident status = %v, want RescueAmbiguous", status)
	}
}

func TestPeriodicCreatesSeparateNativeAndTokenIncidents(t *testing.T) {
	tokens := []common.Address{testAddress(5), testAddress(6)}
	rig := newTestRig(t, rigOptions{tokens: tokens, nativeThreshold: big.NewInt(0)})
	rig.chain.setNative(rig.source, 1_000_000_000_000_000)
	for _, token := range tokens {
		rig.chain.setToken(token, rig.source, 3)
	}
	candidate := domain.NewPeriodicCandidate(rig.chainID, rig.source, 1, 1)
	if err := rig.session.Handle(context.Background(), candidate); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	incidents := rig.state.snapshot()
	if len(incidents) != 3 {
		t.Fatalf("periodic incident count = %d, want 3", len(incidents))
	}
	parent := domain.NewIncidentID(candidate.ID)
	for _, incident := range incidents {
		if incident.Parent != parent {
			t.Fatalf("child parent = %s, want %s", incident.Parent, parent)
		}
	}
}

func TestFeeRPCErrorFailsClosedBeforeSigning(t *testing.T) {
	rig := newTestRig(t, rigOptions{tokens: []common.Address{testAddress(5)}, gasPriceError: errors.New("fee unavailable")})
	token := rig.tokens[0]
	rig.chain.setToken(token, rig.source, 5)
	assertCode(t, rig.session.Handle(context.Background(), rig.tokenCandidate(token, 1)), codeFeeRead)
	if rig.authorizer.count() != 0 || rig.transactioner.count() != 0 || len(rig.broadcaster.snapshot()) != 0 {
		t.Fatal("fee RPC error used a fallback and reached signing")
	}
}

func TestBudgetExhaustionStopsBeforeEverySignatureAndKeepsIncident(t *testing.T) {
	token := testAddress(5)
	rig := newTestRig(t, rigOptions{tokens: []common.Address{token}})
	rig.chain.setToken(token, rig.source, 5)
	blocker := domain.NewBlockCandidate(rig.chainID, domain.CandidateNative, rig.source, testHash(0xee), 999)
	_, err := rig.coordinator.budget.Reserve(context.Background(), budget.ReservationRequest{
		Network: rig.chainID, Sponsor: rig.sponsor, Candidate: blocker.ID,
		Attempt:        budget.Attempt{Incident: domain.NewAssetIncidentID(blocker.ID, domain.CandidateNative, common.Address{}), Number: 1},
		Quote:          budget.CostQuote{GasLimit: 1, MaxFeePerGas: *uint256.NewInt(1_000_000_000_000_000_000)},
		SponsorBalance: *uint256.NewInt(2_000_000_000_000_000_000),
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate := rig.tokenCandidate(token, 1)
	assertCode(t, rig.session.Handle(context.Background(), candidate), codeBudgetExceeded)
	incident := rig.incident(candidate, domain.CandidateToken, token)
	if incident.Status != store.RescuePrepared || incident.Attempts != 1 || rig.authorizer.count() != 0 || rig.transactioner.count() != 0 || len(rig.broadcaster.snapshot()) != 0 {
		t.Fatalf("budget block: status=%v attempts=%d signatures=%d/%d broadcasts=%d", incident.Status, incident.Attempts, rig.authorizer.count(), rig.transactioner.count(), len(rig.broadcaster.snapshot()))
	}
}

func TestSponsorReserveAndEmergencyStopProduceNoPaidActions(t *testing.T) {
	token := testAddress(5)
	t.Run("reserve", func(t *testing.T) {
		rig := newTestRig(t, rigOptions{tokens: []common.Address{token}})
		rig.chain.setToken(token, rig.source, 5)
		rig.chain.setNative(rig.sponsor, 1)
		assertCode(t, rig.session.Handle(context.Background(), rig.tokenCandidate(token, 1)), codeSponsorReserve)
		if rig.authorizer.count() != 0 || rig.transactioner.count() != 0 || len(rig.broadcaster.snapshot()) != 0 {
			t.Fatal("sponsor reserve block reached a paid action")
		}
	})
	t.Run("emergency stop", func(t *testing.T) {
		rig := newTestRig(t, rigOptions{tokens: []common.Address{token}})
		rig.chain.setToken(token, rig.source, 5)
		rig.coordinator.gate.Stop()
		candidate := rig.tokenCandidate(token, 1)
		assertCode(t, rig.session.Handle(context.Background(), candidate), codePaidActionsStopped)
		if _, found, _ := rig.state.RescueIncident(context.Background(), domain.NewAssetIncidentID(candidate.ID, domain.CandidateToken, token)); found {
			t.Fatal("emergency stop создал attempt вместо сохранения candidate в очереди")
		}
		if rig.authorizer.count() != 0 || rig.transactioner.count() != 0 || len(rig.broadcaster.snapshot()) != 0 {
			t.Fatal("emergency stop reached a paid action")
		}
	})
}

func TestSimulationFailureReleasesReservationBeforeSponsorSignature(t *testing.T) {
	token := testAddress(5)
	rig := newTestRig(t, rigOptions{tokens: []common.Address{token}})
	rig.chain.setToken(token, rig.source, 5)
	rig.primary.estimateError = errors.New("simulation rejected")
	assertCode(t, rig.session.Handle(context.Background(), rig.tokenCandidate(token, 1)), codeSimulation)
	snapshot, err := rig.coordinator.budget.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rig.authorizer.count() != 1 || rig.transactioner.count() != 0 || len(rig.broadcaster.snapshot()) != 0 || !snapshot.Global.Reserved.Cumulative.IsZero() {
		t.Fatalf("simulation failure: signatures=%d/%d broadcasts=%d reserved=%s", rig.authorizer.count(), rig.transactioner.count(), len(rig.broadcaster.snapshot()), snapshot.Global.Reserved.Cumulative.String())
	}
}

func TestQuorumSimulationFailureStopsBeforeSponsorSignature(t *testing.T) {
	token := testAddress(5)
	rig := newTestRig(t, rigOptions{tokens: []common.Address{token}, quorumSimulationError: errors.New("quorum rejected")})
	rig.chain.setToken(token, rig.source, 5)
	assertCode(t, rig.session.Handle(context.Background(), rig.tokenCandidate(token, 1)), codeSimulation)
	if rig.authorizer.count() != 1 || rig.transactioner.count() != 0 || len(rig.broadcaster.snapshot()) != 0 {
		t.Fatalf("quorum simulation failure: signatures=%d/%d broadcasts=%d", rig.authorizer.count(), rig.transactioner.count(), len(rig.broadcaster.snapshot()))
	}
}

func TestCoordinatorRejectsUnboundedAdditionalFeeModel(t *testing.T) {
	_, err := buildTestRig(t, rigOptions{tokens: []common.Address{testAddress(5)}, unboundedAdditionalFees: true})
	assertCode(t, err, codeInvalidConfig)
}

func TestTerminalReplayDoesNotClearRPCDegradationWithoutRead(t *testing.T) {
	token := testAddress(5)
	rig := newTestRig(t, rigOptions{tokens: []common.Address{token}})
	health, err := observability.NewHealth([]domain.NetworkID{rig.chainID})
	if err != nil {
		t.Fatal(err)
	}
	rig.coordinator.health = health
	rig.chain.setToken(token, rig.source, 5)
	candidate := rig.tokenCandidate(token, 1)
	if err := rig.session.Handle(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	rig.coordinator.recordFailure(candidate.ID, common.Hash{}, newError("test.rpc", domain.ErrorRPCTransient, codeFinalityRead, true, true, nil))
	if err := rig.session.Handle(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	if !health.Snapshot().Chains[rig.chainID].Conditions.RPCDegraded {
		t.Fatal("terminal replay cleared RPC degradation without a successful read")
	}
}

func TestPeriodicFailureLogIsBoundToFailingChildIncident(t *testing.T) {
	token := testAddress(5)
	rig := newTestRig(t, rigOptions{tokens: []common.Address{token}, nativeThreshold: new(big.Int)})
	rig.chain.setNative(rig.source, 1)
	rig.chain.setToken(token, rig.source, 5)
	capture := &capturingStructuredObserver{}
	rig.coordinator.structured = capture
	candidate := domain.NewPeriodicCandidate(rig.chainID, rig.source, 1, 1)
	assertCode(t, rig.session.Handle(context.Background(), candidate), codeMinimumValue)
	nativeID := domain.NewAssetIncidentID(candidate.ID, domain.CandidateNative, common.Address{})
	tokenID := domain.NewAssetIncidentID(candidate.ID, domain.CandidateToken, token)
	var failures []domain.IncidentID
	for _, event := range capture.events {
		if event.Level == observability.LevelError && event.Error != nil && event.Incident != (domain.IncidentID{}) {
			failures = append(failures, event.Incident)
		}
	}
	if len(failures) != 1 || failures[0] != nativeID {
		t.Fatalf("periodic failure incidents = %v, want [%s] and never %s", failures, nativeID, tokenID)
	}
}

func TestEveryPeriodicChildFailureUpdatesRPCTelemetry(t *testing.T) {
	token := testAddress(5)
	rig := newTestRig(t, rigOptions{tokens: []common.Address{token}, nativeThreshold: new(big.Int)})
	rig.chain.setNative(rig.source, 1)
	rig.chain.setToken(token, rig.source, 5)
	rig.chain.tokenBalanceError = token
	metrics, err := observability.NewMetrics([]domain.NetworkID{rig.chainID})
	if err != nil {
		t.Fatal(err)
	}
	health, err := observability.NewHealth([]domain.NetworkID{rig.chainID})
	if err != nil {
		t.Fatal(err)
	}
	if err := health.SetCondition(rig.chainID, observability.ConditionRPCDegraded, false); err != nil {
		t.Fatal(err)
	}
	rig.coordinator.metrics = metrics
	rig.coordinator.health = health
	candidate := domain.NewPeriodicCandidate(rig.chainID, rig.source, 1, 1)
	assertCode(t, rig.session.Handle(context.Background(), candidate), codeMinimumValue)
	if got := metrics.Snapshot().Chains[rig.chainID].RPCErrors; got != 1 {
		t.Fatalf("RPC errors = %d, want token child error", got)
	}
	if !health.Snapshot().Chains[rig.chainID].Conditions.RPCDegraded {
		t.Fatal("later periodic token RPC error did not degrade health")
	}
}

func TestFinalizedReceiptCommitsActualBudgetAndClearsReservation(t *testing.T) {
	token := testAddress(5)
	rig := newTestRig(t, rigOptions{tokens: []common.Address{token}})
	rig.chain.setToken(token, rig.source, 5)
	if err := rig.session.Handle(context.Background(), rig.tokenCandidate(token, 1)); err != nil {
		t.Fatal(err)
	}
	snapshot, err := rig.coordinator.budget.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Global.Spent.Cumulative.IsZero() || !snapshot.Global.Reserved.Cumulative.IsZero() {
		t.Fatalf("finalized budget: spent=%s reserved=%s", snapshot.Global.Spent.Cumulative.String(), snapshot.Global.Reserved.Cumulative.String())
	}
}

func TestFinalizedBudgetUsesReceiptFeeInsteadOfUnrelatedSponsorDebit(t *testing.T) {
	token := testAddress(5)
	rig := newTestRig(t, rigOptions{tokens: []common.Address{token}})
	rig.chain.setToken(token, rig.source, 5)
	rig.chain.extraSponsorDebit = big.NewInt(123_456)
	if err := rig.session.Handle(context.Background(), rig.tokenCandidate(token, 1)); err != nil {
		t.Fatal(err)
	}
	transactions := rig.broadcaster.snapshot()
	if len(transactions) != 1 {
		t.Fatalf("broadcasts = %d", len(transactions))
	}
	want := new(big.Int).Mul(new(big.Int).SetUint64(transactions[0].Gas()), transactions[0].GasFeeCap())
	snapshot, err := rig.coordinator.budget.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Global.Spent.Cumulative.ToBig().Cmp(want) != 0 {
		t.Fatalf("spent = %s, want canonical receipt fee %s", snapshot.Global.Spent.Cumulative.String(), want)
	}
}

func TestCoordinatorUsesDurableStoreTransitions(t *testing.T) {
	serviceClock := newFakeClock()
	source, sponsor := testSignerAddress(1), testSignerAddress(2)
	destination, rescuer, token := testAddress(3), testAddress(4), testAddress(5)
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"), store.OpenOptions{
		Network: 901, Source: source, Sponsor: sponsor, Destination: destination, Rescuer: rescuer,
		MaxPending: 16, MaxDiscoveredTokens: 16, Clock: serviceClock,
	})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer state.Close()
	lease, err := state.Acquire(context.Background(), store.LeaseKey{Network: 901, Sponsor: sponsor}, "test-owner", time.Second)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}

	chain := newFakeFinality(source, sponsor, destination, rescuer)
	chain.native[sponsor] = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	chain.setToken(token, source, 7)
	authorizer := &fakeAuthorizationSigner{address: source, key: testPrivateKey(1)}
	transactioner := &fakeTransactionSigner{address: sponsor, key: testPrivateKey(2)}
	broadcaster := &fakeBroadcaster{chain: chain}
	coordinatorConfig := withTestEconomicPolicy(t, Config{
		Network: domain.Network{Name: "durable-test", ChainID: 901, Rescuer: rescuer, HasRescuer: true, Tokens: []domain.Token{{Address: token}}},
		Source:  source, Sponsor: sponsor, Destination: destination, State: state, LeaseManager: state, Lease: lease,
		ProcessFence: newFakeProcessFence(),
		LeaseTTL:     time.Second, RetryDelay: time.Millisecond, ReceiptTimeout: time.Millisecond,
	}, serviceClock)
	coordinator, err := NewCoordinator(coordinatorConfig, authorizer, transactioner, serviceClock, observability.Discard{})
	if err != nil {
		t.Fatalf("NewCoordinator() error = %v", err)
	}
	primary := &fakePrimary{chainID: big.NewInt(901), source: source, sponsor: sponsor, sponsorNonce: 5, sourceNonce: 9}
	session, err := coordinator.NewSession(context.Background(), 1, primary, chain, broadcaster)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	candidate := domain.NewTokenReconciliationCandidate(901, source, domain.Token{Address: token}, 1, 1)
	if err := session.Handle(context.Background(), candidate); err != nil {
		incident, _, _ := state.RescueIncident(context.Background(), domain.NewAssetIncidentID(candidate.ID, domain.CandidateToken, token))
		t.Fatalf("Handle() error = %v after status %v attempts %d code %s", err, incident.Status, incident.Attempts, incident.LastCode)
	}
	incident, found, err := state.RescueIncident(context.Background(), domain.NewAssetIncidentID(candidate.ID, domain.CandidateToken, token))
	if err != nil || !found || incident.Status != store.RescueTokenReported {
		t.Fatalf("durable incident = found %t status %v error %v", found, incident.Status, err)
	}

	chain.setToken(token, source, 7)
	primary.headerError = errors.New("header unavailable")
	retryCandidate := domain.NewTokenReconciliationCandidate(901, source, domain.Token{Address: token}, 1, 2)
	assertCode(t, session.Handle(context.Background(), retryCandidate), codeFeeRead)
	retryIncident, found, err := state.RescueIncident(context.Background(), domain.NewAssetIncidentID(retryCandidate.ID, domain.CandidateToken, token))
	if err != nil || !found || retryIncident.Status != store.RescueRetryable || retryIncident.Attempts != 1 {
		t.Fatalf("durable retry = found %t status %v attempts %d error %v", found, retryIncident.Status, retryIncident.Attempts, err)
	}
	serviceClock.advance(time.Millisecond)
	chain.advanceBlock()
	chain.setToken(token, source, 9)
	primary.headerError = nil
	primary.sponsorNonce = 7
	primary.sourceNonce = 10
	if err := session.Handle(context.Background(), retryCandidate); err != nil {
		t.Fatalf("retry Handle() error = %v", err)
	}
	retryIncident, found, err = state.RescueIncident(context.Background(), domain.NewAssetIncidentID(retryCandidate.ID, domain.CandidateToken, token))
	if err != nil || !found || retryIncident.Status != store.RescueTokenReported || retryIncident.Attempts != 2 ||
		retryIncident.SponsorNonce != 7 || retryIncident.SourceNonce != 10 || retryIncident.SnapshotBlockHash != testHash(12) {
		t.Fatalf("reprepared incident = found %t status %v attempts %d nonces %d/%d snapshot %s error %v", found, retryIncident.Status, retryIncident.Attempts, retryIncident.SponsorNonce, retryIncident.SourceNonce, retryIncident.SnapshotBlockHash, err)
	}

	chain.setToken(token, source, 7)
	primary.sponsorNonceError = errors.New("nonce unavailable")
	nonceCandidate := domain.NewTokenReconciliationCandidate(901, source, domain.Token{Address: token}, 1, 3)
	assertCode(t, session.Handle(context.Background(), nonceCandidate), codeNonceRead)
	nonceIncident, found, err := state.RescueIncident(context.Background(), domain.NewAssetIncidentID(nonceCandidate.ID, domain.CandidateToken, token))
	if err != nil || !found || nonceIncident.Status != store.RescueRetryable || nonceIncident.LastCode != codeNonceRead {
		t.Fatalf("durable nonce error = found %t status %v code %s error %v", found, nonceIncident.Status, nonceIncident.LastCode, err)
	}

	primary.sponsorNonceError = nil
	chain.mu.Lock()
	chain.finalizedErrorAfter = chain.finalizedCalls
	chain.mu.Unlock()
	prestateCandidate := domain.NewBlockCandidate(901, domain.CandidateNative, source, testHash(90), 90)
	for attempt := 0; attempt < 3; attempt++ {
		handleErr := session.Handle(context.Background(), prestateCandidate)
		if errorCode(handleErr) != codeFinalityRead {
			stored, _, _ := state.RescueIncident(context.Background(), domain.NewAssetIncidentID(prestateCandidate.ID, domain.CandidateNative, common.Address{}))
			t.Fatalf("prestate attempt %d error = %v after status %v attempts %d code %s retry %s", attempt+1, handleErr, stored.Status, stored.Attempts, stored.LastCode, stored.RetryAt)
		}
		serviceClock.advance(time.Millisecond)
	}
	if err := session.Handle(context.Background(), prestateCandidate); err != nil {
		t.Fatalf("prestate exhausted replay error = %v", err)
	}
	prestateIncident, found, err := state.RescueIncident(context.Background(), domain.NewAssetIncidentID(prestateCandidate.ID, domain.CandidateNative, common.Address{}))
	if err != nil || !found || prestateIncident.Status != store.RescueExhausted || prestateIncident.Attempts != 3 {
		t.Fatalf("durable prestate failure = found %t status %v attempts %d error %v", found, prestateIncident.Status, prestateIncident.Attempts, err)
	}
}

type rigOptions struct {
	tokens                  []common.Address
	allowUnknown            bool
	nativeThreshold         *big.Int
	state                   *memoryState
	clock                   *fakeClock
	chain                   *fakeFinality
	lease                   *fakeLeaseManager
	fence                   *fakeProcessFence
	authorizationError      error
	headerError             error
	gasPriceError           error
	sendError               error
	receiptError            error
	postCodeError           error
	zeroSourceWithoutDelta  bool
	delegatedTarget         common.Address
	finalizedErrorAt        int
	finalizedErrorAfter     int
	preBalanceError         error
	invalidTokenBalance     bool
	keepSourceAfter         bool
	advanceDuringPostRead   bool
	headerMismatchNumber    uint64
	headerMismatchAfter     int
	receiptStatus           *uint64
	budget                  budget.Ledger
	quorumSimulationError   error
	unboundedAdditionalFees bool
}

type testRig struct {
	chainID       domain.NetworkID
	source        common.Address
	sponsor       common.Address
	destination   common.Address
	rescuer       common.Address
	tokens        []common.Address
	clock         *fakeClock
	state         *memoryState
	chain         *fakeFinality
	authorizer    *fakeAuthorizationSigner
	transactioner *fakeTransactionSigner
	broadcaster   *fakeBroadcaster
	coordinator   *Coordinator
	lease         *fakeLeaseManager
	fence         *fakeProcessFence
	primary       *fakePrimary
	session       *Session
}

type capturingStructuredObserver struct {
	events []observability.SafeEvent
}

func (observer *capturingStructuredObserver) Write(event observability.SafeEvent) error {
	observer.events = append(observer.events, event)
	return nil
}

func newTestRig(t *testing.T, options rigOptions) *testRig {
	t.Helper()
	rig, err := buildTestRig(t, options)
	if err != nil {
		t.Fatalf("buildTestRig() error = %v", err)
	}
	return rig
}

func buildTestRig(t testing.TB, options rigOptions) (*testRig, error) {
	sourceKey := testPrivateKey(1)
	sponsorKey := testPrivateKey(2)
	serviceClock := options.clock
	if serviceClock == nil {
		serviceClock = newFakeClock()
	}
	rig := &testRig{
		chainID: 901, source: crypto.PubkeyToAddress(sourceKey.PublicKey), sponsor: crypto.PubkeyToAddress(sponsorKey.PublicKey), destination: testAddress(3), rescuer: testAddress(4),
		tokens: append([]common.Address(nil), options.tokens...), clock: serviceClock, state: options.state,
	}
	if rig.state == nil {
		rig.state = newMemoryState()
	}
	lease := options.lease
	if lease == nil {
		lease = newFakeLease()
	}
	fence := options.fence
	if fence == nil {
		fence = newFakeProcessFence()
	}
	rig.chain = options.chain
	if rig.chain == nil {
		rig.chain = newFakeFinality(rig.source, rig.sponsor, rig.destination, rig.rescuer)
		rig.chain.native[rig.sponsor] = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	}
	rig.chain.receiptError = options.receiptError
	rig.chain.postCodeError = options.postCodeError
	rig.chain.zeroSourceWithoutDelta = options.zeroSourceWithoutDelta
	rig.chain.finalizedErrorAt = options.finalizedErrorAt
	rig.chain.finalizedErrorAfter = options.finalizedErrorAfter
	rig.chain.preBalanceError = options.preBalanceError
	rig.chain.invalidTokenBalance = options.invalidTokenBalance
	rig.chain.keepSourceAfter = options.keepSourceAfter
	rig.chain.advanceDuringPostRead = options.advanceDuringPostRead
	rig.chain.headerMismatchNumber = options.headerMismatchNumber
	rig.chain.headerMismatchAfter = options.headerMismatchAfter
	rig.chain.receiptStatus = options.receiptStatus
	rig.chain.quorumSimulationError = options.quorumSimulationError
	if options.delegatedTarget != (common.Address{}) {
		rig.chain.delegatedTarget = options.delegatedTarget
	}
	primary := &fakePrimary{chainID: big.NewInt(int64(rig.chainID)), source: rig.source, sponsor: rig.sponsor, sponsorNonce: 5, sourceNonce: 9, headerError: options.headerError, gasPriceError: options.gasPriceError}
	rig.primary = primary
	rig.authorizer = &fakeAuthorizationSigner{address: rig.source, key: sourceKey, err: options.authorizationError}
	rig.transactioner = &fakeTransactionSigner{address: rig.sponsor, key: sponsorKey}
	rig.broadcaster = &fakeBroadcaster{chain: rig.chain, err: options.sendError}

	network := domain.Network{Name: "local-test", ChainID: rig.chainID, Rescuer: rig.rescuer, HasRescuer: true, AllowUnknownTokens: options.allowUnknown}
	for _, token := range rig.tokens {
		network.Tokens = append(network.Tokens, domain.Token{Address: token})
	}
	coordinatorConfig := withTestEconomicPolicy(t, Config{
		Network: network, Source: rig.source, Sponsor: rig.sponsor, Destination: rig.destination, NativeThreshold: options.nativeThreshold,
		State: rig.state, LeaseManager: lease, Lease: store.Lease{Key: store.LeaseKey{Network: rig.chainID, Sponsor: rig.sponsor}, Owner: "test-owner"}, ProcessFence: fence,
		LeaseTTL: time.Second, MaxAttempts: 3, RetryDelay: time.Millisecond, ReceiptTimeout: time.Millisecond,
		Budget: options.budget,
	}, rig.clock)
	coordinatorConfig.FeePolicy.UnboundedAdditionalFees = options.unboundedAdditionalFees
	coordinator, err := NewCoordinator(coordinatorConfig, rig.authorizer, rig.transactioner, rig.clock, observability.Discard{})
	if err != nil {
		return nil, err
	}
	rig.coordinator = coordinator
	rig.lease = lease
	rig.fence = fence
	rig.session, err = coordinator.NewSession(context.Background(), 1, primary, rig.chain, rig.broadcaster)
	if err != nil {
		return nil, err
	}
	return rig, nil
}

func withTestEconomicPolicy(t testing.TB, config Config, serviceClock clock.Clock) Config {
	t.Helper()
	limit := *uint256.NewInt(1_000_000_000_000_000_000)
	transactionCap := *uint256.NewInt(1_000_000_000_000_000)
	ledger := config.Budget
	if ledger == nil {
		var err error
		ledger, err = budget.Open(filepath.Join(t.TempDir(), "budget.db"), budget.OpenOptions{
			Policy: budget.Policy{
				Global: budget.Limits{PerTransaction: limit, PerHour: limit, PerDay: limit, Cumulative: limit},
				Networks: []budget.NetworkPolicy{{
					Network: config.Network.ChainID, Sponsor: config.Sponsor,
					Limits:                  budget.Limits{PerTransaction: limit, PerHour: limit, PerDay: limit, Cumulative: limit},
					EmergencySponsorReserve: *uint256.NewInt(1),
				}},
			},
			PolicyFingerprint: sha256.Sum256([]byte("deterministic-test-budget-policy")),
			Now:               serviceClock.Now,
		})
		if err != nil {
			t.Fatalf("budget.Open() error = %v", err)
		}
		t.Cleanup(func() {
			if err := ledger.Close(); err != nil {
				t.Errorf("budget.Close() error = %v", err)
			}
		})
	}
	admission, err := NewAdmissionController(AdmissionConfig{
		Window: time.Hour, RateLimit: 1024, MaxAttemptsPerToken: 1024,
		MaxAttemptsPerSourceEvent: 1024, MaxNewUnknownTokens: 1024, Capacity: 1024,
	}, serviceClock)
	if err != nil {
		t.Fatalf("NewAdmissionController() error = %v", err)
	}
	config.Budget = ledger
	config.Gate = observability.NewPaidActionGate()
	config.Admission = admission
	config.FeePolicy = FeePolicy{
		Network:      config.Network.ChainID,
		MaxFeePerGas: *uint256.NewInt(100_000_000_000), MaxPriorityFeePerGas: *uint256.NewInt(5_000_000_000),
		TokenGasLimit: 220_000, NativeGasLimit: 80_000,
		UnknownTokenCostCap: transactionCap, TransactionCostCap: transactionCap,
	}
	config.TrustedTokens = make([]common.Address, 0, len(config.Network.Tokens))
	config.TrustedTokenValues = make(map[common.Address]TrustedTokenValuePolicy, len(config.Network.Tokens))
	for _, token := range config.Network.Tokens {
		config.TrustedTokens = append(config.TrustedTokens, token.Address)
		config.TrustedTokenValues[token.Address] = TrustedTokenValuePolicy{MinimumBalance: *uint256.NewInt(1), MaximumCost: transactionCap}
	}
	return config
}

func (rig *testRig) tokenCandidate(token common.Address, observation uint64) domain.RescueCandidate {
	return domain.NewTokenReconciliationCandidate(rig.chainID, rig.source, domain.Token{Address: token}, 1, observation)
}

func (rig *testRig) nativeCandidate(number uint64) domain.RescueCandidate {
	return domain.NewBlockCandidate(rig.chainID, domain.CandidateNative, rig.source, testHash(byte(number)), number)
}

func (rig *testRig) incident(candidate domain.RescueCandidate, kind domain.CandidateKind, asset common.Address) store.RescueIncident {
	incident, ok, err := rig.state.RescueIncident(context.Background(), domain.NewAssetIncidentID(candidate.ID, kind, asset))
	if err != nil || !ok {
		panic("test incident missing")
	}
	return incident
}

type memoryState struct {
	mu         sync.Mutex
	incidents  map[domain.IncidentID]store.RescueIncident
	nonceFloor uint64
	pruneError error
	raiseHook  func()
	updateHook func(store.RescueIncident)
}

func newMemoryState() *memoryState {
	return &memoryState{incidents: make(map[domain.IncidentID]store.RescueIncident)}
}

func (state *memoryState) PutRescueIncident(_ context.Context, incident store.RescueIncident) (store.RescueIncident, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if stored, ok := state.incidents[incident.ID]; ok {
		return cloneTestIncident(stored), nil
	}
	state.incidents[incident.ID] = cloneTestIncident(incident)
	return cloneTestIncident(incident), nil
}

func (state *memoryState) UpdateRescueIncident(_ context.Context, incident store.RescueIncident) error {
	state.mu.Lock()
	state.incidents[incident.ID] = cloneTestIncident(incident)
	hook := state.updateHook
	state.mu.Unlock()
	if hook != nil {
		hook(cloneTestIncident(incident))
	}
	return nil
}

func (state *memoryState) RescueIncident(_ context.Context, id domain.IncidentID) (store.RescueIncident, bool, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	incident, ok := state.incidents[id]
	return cloneTestIncident(incident), ok, nil
}

func (state *memoryState) RescueIncidents(_ context.Context, network domain.NetworkID) ([]store.RescueIncident, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	var incidents []store.RescueIncident
	for _, incident := range state.incidents {
		if incident.Network == network {
			incidents = append(incidents, cloneTestIncident(incident))
		}
	}
	return incidents, nil
}

func (state *memoryState) NonceFloor(context.Context) (uint64, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.nonceFloor, nil
}

func (state *memoryState) RaiseNonceFloor(_ context.Context, floor uint64) error {
	state.mu.Lock()
	if floor > state.nonceFloor {
		state.nonceFloor = floor
	}
	hook := state.raiseHook
	state.raiseHook = nil
	state.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

func (state *memoryState) PruneRescueIncidents(context.Context, int) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.pruneError
}

func cloneTestIncident(incident store.RescueIncident) store.RescueIncident {
	incident.SignedTransaction = append([]byte(nil), incident.SignedTransaction...)
	return incident
}

func (state *memoryState) snapshot() []store.RescueIncident {
	incidents, _ := state.RescueIncidents(context.Background(), 901)
	return incidents
}

type fakeLeaseManager struct {
	mu           sync.Mutex
	valid        bool
	lost         chan struct{}
	released     store.Lease
	releaseError error
	renewCall    func() error
	validations  int
}

func newFakeLease() *fakeLeaseManager {
	return &fakeLeaseManager{valid: true, lost: make(chan struct{})}
}

func (*fakeLeaseManager) Acquire(_ context.Context, key store.LeaseKey, owner string, ttl time.Duration) (store.Lease, error) {
	return store.Lease{Key: key, Owner: owner, ExpiresAt: time.Now().Add(ttl)}, nil
}

func (manager *fakeLeaseManager) Renew(_ context.Context, lease store.Lease, ttl time.Duration) (store.Lease, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if !manager.valid {
		return store.Lease{}, errors.New("lease lost")
	}
	if manager.renewCall != nil {
		if err := manager.renewCall(); err != nil {
			return store.Lease{}, err
		}
	}
	lease.ExpiresAt = lease.ExpiresAt.Add(ttl)
	return lease, nil
}

func (manager *fakeLeaseManager) Validate(context.Context, store.Lease) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.validations++
	if !manager.valid {
		return errors.New("lease invalid")
	}
	return nil
}

func (manager *fakeLeaseManager) validationCount() int {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.validations
}

func (manager *fakeLeaseManager) Lost(store.Lease) <-chan struct{} { return manager.lost }

func (manager *fakeLeaseManager) Release(_ context.Context, lease store.Lease) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.released = lease
	return manager.releaseError
}

func (manager *fakeLeaseManager) releasedLease() store.Lease {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.released
}

func (manager *fakeLeaseManager) lose() {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.valid {
		manager.valid = false
		close(manager.lost)
	}
}

type fakeProcessFence struct {
	mu          sync.Mutex
	valid       bool
	validations int
	releases    int
}

func newFakeProcessFence() *fakeProcessFence {
	return &fakeProcessFence{valid: true}
}

func (fence *fakeProcessFence) Validate() error {
	fence.mu.Lock()
	defer fence.mu.Unlock()
	fence.validations++
	if !fence.valid {
		return store.ErrFenceLost
	}
	return nil
}

func (fence *fakeProcessFence) Release() error {
	fence.mu.Lock()
	defer fence.mu.Unlock()
	fence.releases++
	fence.valid = false
	return nil
}

func (fence *fakeProcessFence) lose() {
	fence.mu.Lock()
	fence.valid = false
	fence.mu.Unlock()
}

func (fence *fakeProcessFence) validationCount() int {
	fence.mu.Lock()
	defer fence.mu.Unlock()
	return fence.validations
}

func (fence *fakeProcessFence) releaseCount() int {
	fence.mu.Lock()
	defer fence.mu.Unlock()
	return fence.releases
}

type fakePrimary struct {
	mu                sync.Mutex
	chainID           *big.Int
	source            common.Address
	sponsor           common.Address
	sponsorNonce      uint64
	sourceNonce       uint64
	sponsorNonceError error
	headerError       error
	gasPriceError     error
	estimateError     error
	estimate          uint64
}

func (reader *fakePrimary) ChainID(context.Context) (*big.Int, error) {
	return new(big.Int).Set(reader.chainID), nil
}

func (reader *fakePrimary) PendingNonceAt(_ context.Context, address common.Address) (uint64, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if address == reader.source {
		return reader.sourceNonce, nil
	}
	if reader.sponsorNonceError != nil {
		return 0, reader.sponsorNonceError
	}
	return reader.sponsorNonce, nil
}

func (reader *fakePrimary) HeaderByNumber(context.Context, *big.Int) (*types.Header, error) {
	if reader.headerError != nil {
		return nil, reader.headerError
	}
	return &types.Header{BaseFee: big.NewInt(1_000_000)}, nil
}

func (reader *fakePrimary) SuggestGasPrice(context.Context) (*big.Int, error) {
	if reader.gasPriceError != nil {
		return nil, reader.gasPriceError
	}
	return big.NewInt(2_000_000), nil
}

func (reader *fakePrimary) EstimateGas(_ context.Context, call ethereum.CallMsg) (uint64, error) {
	if reader.estimateError != nil {
		return 0, reader.estimateError
	}
	if reader.estimate != 0 {
		return reader.estimate, nil
	}
	if call.Gas == 0 {
		return 0, errors.New("gas limit не задан")
	}
	return call.Gas, nil
}

type fakeFinality struct {
	mu                     sync.Mutex
	source                 common.Address
	sponsor                common.Address
	destination            common.Address
	rescuer                common.Address
	delegatedTarget        common.Address
	block                  rpc.BlockRef
	headers                map[uint64]rpc.BlockRef
	headerCalls            map[uint64]int
	native                 map[common.Address]*big.Int
	tokens                 map[common.Address]map[common.Address]*big.Int
	receipts               map[common.Hash]*types.Receipt
	receiptError           error
	postCodeError          error
	preBalanceError        error
	invalidTokenBalance    bool
	zeroSourceWithoutDelta bool
	keepSourceAfter        bool
	advanceDuringPostRead  bool
	postReadAdvanced       bool
	headerMismatchNumber   uint64
	headerMismatchAfter    int
	receiptStatus          *uint64
	finalizedCalls         int
	finalizedErrorAt       int
	finalizedErrorAfter    int
	broadcasts             int
	extraSponsorDebit      *big.Int
	quorumSimulationError  error
	tokenBalanceError      common.Address
}

func newFakeFinality(source, sponsor, destination, rescuer common.Address) *fakeFinality {
	return &fakeFinality{
		source: source, sponsor: sponsor, destination: destination, rescuer: rescuer, delegatedTarget: rescuer,
		block: rpc.BlockRef{Number: 10, Hash: testHash(10), ParentHash: testHash(9)}, native: make(map[common.Address]*big.Int),
		headers:     map[uint64]rpc.BlockRef{10: {Number: 10, Hash: testHash(10), ParentHash: testHash(9)}},
		headerCalls: make(map[uint64]int),
		tokens:      make(map[common.Address]map[common.Address]*big.Int), receipts: make(map[common.Hash]*types.Receipt),
	}
}

func (chain *fakeFinality) Finalized(context.Context) (rpc.BlockRef, error) {
	chain.mu.Lock()
	defer chain.mu.Unlock()
	chain.finalizedCalls++
	if chain.finalizedErrorAt != 0 && chain.finalizedCalls == chain.finalizedErrorAt {
		return rpc.BlockRef{}, errors.New("canonical quorum changed")
	}
	if chain.finalizedErrorAfter != 0 && chain.finalizedCalls > chain.finalizedErrorAfter {
		return rpc.BlockRef{}, errors.New("canonical quorum unavailable")
	}
	return chain.block, nil
}

func (chain *fakeFinality) Header(_ context.Context, number uint64) (rpc.BlockRef, error) {
	chain.mu.Lock()
	defer chain.mu.Unlock()
	header, ok := chain.headers[number]
	if !ok {
		return rpc.BlockRef{}, errors.New("header unavailable")
	}
	chain.headerCalls[number]++
	if number == chain.headerMismatchNumber && chain.headerCalls[number] > chain.headerMismatchAfter {
		header.Hash = testHash(0xfe)
	}
	return header, nil
}

func (chain *fakeFinality) BalanceAt(_ context.Context, _ rpc.BlockRef, account common.Address) (*big.Int, error) {
	chain.mu.Lock()
	defer chain.mu.Unlock()
	if chain.preBalanceError != nil {
		return nil, chain.preBalanceError
	}
	chain.advanceDuringPostReadLocked()
	return cloneAmount(chain.native[account]), nil
}

func (chain *fakeFinality) CodeAt(context.Context, rpc.BlockRef, common.Address) ([]byte, error) {
	chain.mu.Lock()
	defer chain.mu.Unlock()
	if chain.broadcasts > 0 && chain.postCodeError != nil {
		return nil, chain.postCodeError
	}
	return append([]byte{0xef, 0x01, 0x00}, chain.delegatedTarget.Bytes()...), nil
}

func (chain *fakeFinality) CallContract(_ context.Context, _ rpc.BlockRef, call ethereum.CallMsg) ([]byte, error) {
	chain.mu.Lock()
	defer chain.mu.Unlock()
	if call.To == nil {
		return nil, errors.New("missing target")
	}
	if *call.To == chain.rescuer {
		return common.LeftPadBytes(chain.destination.Bytes(), 32), nil
	}
	if *call.To == chain.source && len(call.AuthorizationList) == 1 {
		if chain.quorumSimulationError != nil {
			return nil, chain.quorumSimulationError
		}
		return nil, nil
	}
	if *call.To == chain.tokenBalanceError {
		return nil, errors.New("token balance unavailable")
	}
	if chain.broadcasts > 0 && chain.postCodeError != nil {
		return nil, chain.postCodeError
	}
	if chain.invalidTokenBalance {
		return []byte{1}, nil
	}
	chain.advanceDuringPostReadLocked()
	if len(call.Data) < 20 {
		return nil, errors.New("invalid balance call")
	}
	account := common.BytesToAddress(call.Data[len(call.Data)-20:])
	return common.LeftPadBytes(cloneAmount(chain.tokens[*call.To][account]).Bytes(), 32), nil
}

func (chain *fakeFinality) Receipt(_ context.Context, hash common.Hash) (*types.Receipt, error) {
	chain.mu.Lock()
	defer chain.mu.Unlock()
	if chain.receiptError != nil {
		return nil, chain.receiptError
	}
	receipt, ok := chain.receipts[hash]
	if !ok {
		return nil, errors.New("receipt unavailable")
	}
	copy := *receipt
	return &copy, nil
}

func (chain *fakeFinality) setNative(account common.Address, amount int64) {
	chain.mu.Lock()
	defer chain.mu.Unlock()
	chain.native[account] = big.NewInt(amount)
}

func (chain *fakeFinality) setToken(token, account common.Address, amount int64) {
	chain.mu.Lock()
	defer chain.mu.Unlock()
	if chain.tokens[token] == nil {
		chain.tokens[token] = make(map[common.Address]*big.Int)
	}
	chain.tokens[token][account] = big.NewInt(amount)
}

func (chain *fakeFinality) advanceBlock() {
	chain.mu.Lock()
	defer chain.mu.Unlock()
	chain.advanceBlockLocked()
}

func (chain *fakeFinality) advanceBlockLocked() {
	chain.block = rpc.BlockRef{Number: chain.block.Number + 1, Hash: testHash(byte(chain.block.Number + 1)), ParentHash: chain.block.Hash}
	chain.headers[chain.block.Number] = chain.block
}

func (chain *fakeFinality) advanceDuringPostReadLocked() {
	if chain.advanceDuringPostRead && chain.broadcasts > 0 && !chain.postReadAdvanced {
		chain.postReadAdvanced = true
		chain.advanceBlockLocked()
	}
}

func (chain *fakeFinality) accept(transaction *types.Transaction) {
	chain.mu.Lock()
	defer chain.mu.Unlock()
	chain.broadcasts++
	fee := new(big.Int).Mul(new(big.Int).SetUint64(transaction.Gas()), transaction.GasFeeCap())
	fee.Add(fee, cloneAmount(chain.extraSponsorDebit))
	chain.native[chain.sponsor] = new(big.Int).Sub(cloneAmount(chain.native[chain.sponsor]), fee)
	chain.advanceBlockLocked()
	if len(transaction.Data()) > 4 {
		token := common.BytesToAddress(transaction.Data()[len(transaction.Data())-20:])
		if chain.tokens[token] == nil {
			chain.tokens[token] = make(map[common.Address]*big.Int)
		}
		amount := cloneAmount(chain.tokens[token][chain.source])
		if !chain.keepSourceAfter {
			chain.tokens[token][chain.source] = new(big.Int)
		}
		if !chain.zeroSourceWithoutDelta {
			chain.tokens[token][chain.destination] = new(big.Int).Add(cloneAmount(chain.tokens[token][chain.destination]), amount)
		}
	} else {
		amount := cloneAmount(chain.native[chain.source])
		if !chain.keepSourceAfter {
			chain.native[chain.source] = new(big.Int)
		}
		if !chain.zeroSourceWithoutDelta {
			chain.native[chain.destination] = new(big.Int).Add(cloneAmount(chain.native[chain.destination]), amount)
		}
	}
	status := uint64(types.ReceiptStatusSuccessful)
	if chain.receiptStatus != nil {
		status = *chain.receiptStatus
	}
	chain.receipts[transaction.Hash()] = &types.Receipt{
		Status: status, TxHash: transaction.Hash(), BlockHash: chain.block.Hash, BlockNumber: new(big.Int).SetUint64(chain.block.Number), Logs: []*types.Log{},
		GasUsed: transaction.Gas(), EffectiveGasPrice: new(big.Int).Set(transaction.GasFeeCap()),
	}
}

type fakeAuthorizationSigner struct {
	mu      sync.Mutex
	address common.Address
	key     *ecdsa.PrivateKey
	err     error
	calls   []types.SetCodeAuthorization
}

func (signer *fakeAuthorizationSigner) Address() common.Address { return signer.address }

func (signer *fakeAuthorizationSigner) SignAuthorization(_ context.Context, authorization types.SetCodeAuthorization) (types.SetCodeAuthorization, error) {
	signer.mu.Lock()
	defer signer.mu.Unlock()
	signer.calls = append(signer.calls, authorization)
	if signer.err != nil {
		return types.SetCodeAuthorization{}, signer.err
	}
	return types.SignSetCode(signer.key, authorization)
}

func (signer *fakeAuthorizationSigner) count() int {
	signer.mu.Lock()
	defer signer.mu.Unlock()
	return len(signer.calls)
}

type fakeTransactionSigner struct {
	mu        sync.Mutex
	address   common.Address
	key       *ecdsa.PrivateKey
	calls     []*types.Transaction
	afterSign func()
}

func (signer *fakeTransactionSigner) Address() common.Address { return signer.address }

func (signer *fakeTransactionSigner) SignTransaction(_ context.Context, transaction *types.Transaction, _ *big.Int) (*types.Transaction, error) {
	signer.mu.Lock()
	defer signer.mu.Unlock()
	signer.calls = append(signer.calls, transaction)
	signed, err := types.SignTx(transaction, types.LatestSignerForChainID(transaction.ChainId()), signer.key)
	if signer.afterSign != nil {
		signer.afterSign()
	}
	return signed, err
}

func (signer *fakeTransactionSigner) count() int {
	signer.mu.Lock()
	defer signer.mu.Unlock()
	return len(signer.calls)
}

type fakeBroadcaster struct {
	mu           sync.Mutex
	chain        *fakeFinality
	err          error
	transactions []*types.Transaction
}

func (broadcaster *fakeBroadcaster) SendTransaction(_ context.Context, transaction *types.Transaction) error {
	broadcaster.mu.Lock()
	broadcaster.transactions = append(broadcaster.transactions, transaction)
	err := broadcaster.err
	broadcaster.mu.Unlock()
	if err != nil {
		return err
	}
	broadcaster.chain.accept(transaction)
	return nil
}

func (broadcaster *fakeBroadcaster) setError(err error) {
	broadcaster.mu.Lock()
	broadcaster.err = err
	broadcaster.mu.Unlock()
}

func (broadcaster *fakeBroadcaster) snapshot() []*types.Transaction {
	broadcaster.mu.Lock()
	defer broadcaster.mu.Unlock()
	return append([]*types.Transaction(nil), broadcaster.transactions...)
}

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	ticker clock.Ticker
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1, 0)} }

func (serviceClock *fakeClock) Now() time.Time {
	serviceClock.mu.Lock()
	defer serviceClock.mu.Unlock()
	return serviceClock.now
}

func (serviceClock *fakeClock) Sleep(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	serviceClock.advance(duration)
	return nil
}

func (serviceClock *fakeClock) NewTicker(time.Duration) clock.Ticker { return serviceClock.ticker }
func (*fakeClock) NewTimer(time.Duration) clock.Timer                { return nil }

func (serviceClock *fakeClock) advance(duration time.Duration) {
	serviceClock.mu.Lock()
	serviceClock.now = serviceClock.now.Add(duration)
	serviceClock.mu.Unlock()
}

type fakeTicker struct {
	ticks chan time.Time
}

func (ticker *fakeTicker) C() <-chan time.Time { return ticker.ticks }
func (*fakeTicker) Stop()                      {}

func receiveRescueUpdate(t *testing.T, updates <-chan store.RescueStatus) store.RescueStatus {
	t.Helper()
	select {
	case status := <-updates:
		return status
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for rescue state update")
		return 0
	}
}

func cloneAmount(amount *big.Int) *big.Int {
	if amount == nil {
		return new(big.Int)
	}
	return new(big.Int).Set(amount)
}

func amountArray(amount int64) [32]byte {
	var value [32]byte
	big.NewInt(amount).FillBytes(value[:])
	return value
}

func testAddress(value byte) common.Address {
	var address common.Address
	address[len(address)-1] = value
	return address
}

func testPrivateKey(value byte) *ecdsa.PrivateKey {
	key, err := crypto.ToECDSA(common.LeftPadBytes([]byte{value}, 32))
	if err != nil {
		panic(err)
	}
	return key
}

func testSignerAddress(value byte) common.Address {
	return crypto.PubkeyToAddress(testPrivateKey(value).PublicKey)
}

func testHash(value byte) common.Hash {
	var hash common.Hash
	hash[len(hash)-1] = value
	return hash
}

func externalProcessFencePath(key store.LeaseKey) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("guard-daemon/process-fence/v1\x00"))
	var network [8]byte
	binary.BigEndian.PutUint64(network[:], uint64(key.Network))
	_, _ = hash.Write(network[:])
	_, _ = hash.Write(key.Sponsor[:])
	return filepath.Join("/var/tmp/guard-daemon-leases-v1", hex.EncodeToString(hash.Sum(nil)))
}

func transactionNonces(transactions []*types.Transaction) []uint64 {
	nonces := make([]uint64, len(transactions))
	for index, transaction := range transactions {
		nonces[index] = transaction.Nonce()
	}
	return nonces
}

func assertCode(t *testing.T, err error, code domain.ErrorCode) {
	t.Helper()
	var classified *domain.ClassifiedError
	if !errors.As(err, &classified) || classified.Code != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

func assertAmbiguous(t *testing.T, err error) {
	t.Helper()
	var classified *domain.ClassifiedError
	if !errors.As(err, &classified) || !classified.Ambiguous {
		t.Fatalf("error = %v, want ambiguous classification", err)
	}
}

var _ store.RescueStateStore = (*memoryState)(nil)
var _ store.LeaseManager = (*fakeLeaseManager)(nil)
var _ RPCReader = (*fakePrimary)(nil)
var _ FinalityReader = (*fakeFinality)(nil)
