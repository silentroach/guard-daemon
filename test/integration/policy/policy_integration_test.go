package policy_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"guard-daemon/internal/budget"
	"guard-daemon/internal/clock"
	"guard-daemon/internal/config"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"
	"guard-daemon/internal/rescue"
	"guard-daemon/internal/rescue/dryrun"
	guardrpc "guard-daemon/internal/rpc"
	"guard-daemon/internal/store"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
)

var policyFingerprint = sha256.Sum256([]byte("policy integration binding v1"))

func TestPersistentSponsorBudgetCannotBeBypassedByRestartOrStateDirectory(t *testing.T) {
	root := t.TempDir()
	firstRuntime := loadRuntime(t, testEnvironment(filepath.Join(root, "state-a")))
	secondRuntime := loadRuntime(t, testEnvironment(filepath.Join(root, "state-b")))
	rescuer := testAddress(4)

	canonicalFirst, err := store.CanonicalBudgetPath(firstRuntime.SponsorAddress)
	if err != nil {
		t.Fatal(err)
	}
	canonicalSecond, err := store.CanonicalBudgetPath(secondRuntime.SponsorAddress)
	if err != nil {
		t.Fatal(err)
	}
	if canonicalFirst != canonicalSecond || strings.Contains(canonicalFirst, firstRuntime.Watch.StateDirectory) || strings.Contains(canonicalFirst, secondRuntime.Watch.StateDirectory) {
		t.Fatalf("canonical budget path changed with ordinary state directory: %q / %q", canonicalFirst, canonicalSecond)
	}

	firstStore := openStore(t, firstRuntime, rescuer)
	firstCandidate := domain.NewBlockCandidate(firstRuntime.Networks[0].ChainID, domain.CandidateNative, firstRuntime.SourceAddress, testHash(1), 1)
	if result, err := firstStore.Put(context.Background(), firstCandidate); err != nil || result != store.PutInserted {
		t.Fatalf("first state Put() = (%v, %v)", result, err)
	}

	ledgerPath := filepath.Join(root, "host-budget", filepath.Base(canonicalFirst))
	options := budget.OpenOptions{
		Policy:            runtimeBudgetPolicy(t, firstRuntime),
		PolicyFingerprint: policyFingerprint,
		Now:               func() time.Time { return time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC) },
		MaxRecords:        32,
	}
	ledger, err := budget.Open(ledgerPath, options)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := ledger.Reserve(context.Background(), reservationRequest(firstRuntime, firstCandidate, 1))
	if err != nil {
		t.Fatal(err)
	}
	txHash := testHash(9)
	if _, err := ledger.MarkExposed(context.Background(), reservation.ID, txHash); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.CommitFinalized(context.Background(), budget.FinalizedCharge{
		ReservationID: reservation.ID,
		TxHash:        txHash,
		Actual:        *uint256.NewInt(100),
	}); err != nil {
		t.Fatal(err)
	}
	closeLedger(t, ledger)
	closeStore(t, firstStore)

	restartedStore := openStore(t, firstRuntime, rescuer)
	replayed, err := restartedStore.Replay(context.Background(), firstRuntime.Networks[0].ChainID)
	if err != nil || len(replayed) != 1 || replayed[0].ID != firstCandidate.ID {
		t.Fatalf("restart replay = (%v, %v)", replayed, err)
	}
	restartedLedger, err := budget.Open(ledgerPath, options)
	if err != nil {
		t.Fatal(err)
	}
	restartCandidate := domain.NewBlockCandidate(firstRuntime.Networks[0].ChainID, domain.CandidateNative, firstRuntime.SourceAddress, testHash(2), 2)
	if _, err := restartedLedger.Reserve(context.Background(), reservationRequest(firstRuntime, restartCandidate, 1)); !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("restart Reserve() error = %v, want ErrBudgetExceeded", err)
	}
	closeStore(t, restartedStore)

	freshOrdinaryStore := openStore(t, secondRuntime, rescuer)
	stateDirectoryCandidate := domain.NewBlockCandidate(secondRuntime.Networks[0].ChainID, domain.CandidateNative, secondRuntime.SourceAddress, testHash(3), 3)
	if result, err := freshOrdinaryStore.Put(context.Background(), stateDirectoryCandidate); err != nil || result != store.PutInserted {
		t.Fatalf("fresh state Put() = (%v, %v)", result, err)
	}
	if _, err := restartedLedger.Reserve(context.Background(), reservationRequest(secondRuntime, stateDirectoryCandidate, 1)); !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("state-directory bypass Reserve() error = %v, want ErrBudgetExceeded", err)
	}
	snapshot, err := restartedLedger.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Global.Blocked() || !snapshot.Networks[secondRuntime.Networks[0].ChainID].Blocked() {
		t.Fatal("exhausted persistent budget is not reported as blocked")
	}
	closeStore(t, freshOrdinaryStore)
	closeLedger(t, restartedLedger)
}

func TestEmergencyStopReadOnlyPathHasZeroPaidSideEffects(t *testing.T) {
	values := testEnvironment(filepath.Join(t.TempDir(), "emergency-state"))
	values["DRY_RUN"] = "false"
	values["EMERGENCY_STOP"] = "true"
	values["RPC_BROADCAST_HTTP_BASE"] = "https://broadcast.invalid/rpc"
	values["SOURCE_PRIVATE_KEY"] = "source-private-key-canary"
	values["SPONSOR_PRIVATE_KEY"] = "sponsor-private-key-canary"
	runtimeConfig := loadRuntime(t, values)
	if !runtimeConfig.Mode.IsLive() || !runtimeConfig.Policy.EmergencyStop {
		t.Fatal("test config did not enable live emergency stop")
	}
	if source, sponsor, ok := runtimeConfig.LiveSecrets.PrivateKeys(); ok || source != nil || sponsor != nil {
		t.Fatal("emergency stop retained private keys")
	}

	rescuer := testAddress(4)
	handoff := openStore(t, runtimeConfig, rescuer)
	t.Cleanup(func() { closeStoreCleanup(t, handoff) })
	key := store.LeaseKey{Network: runtimeConfig.Networks[0].ChainID, Sponsor: runtimeConfig.SponsorAddress}
	lease, err := handoff.Acquire(context.Background(), key, "policy-integration", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := budget.Open(filepath.Join(t.TempDir(), "budget.db"), budget.OpenOptions{
		Policy:            runtimeBudgetPolicy(t, runtimeConfig),
		PolicyFingerprint: policyFingerprint,
		Now:               time.Now,
		MaxRecords:        32,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeLedgerCleanup(t, ledger) })
	admission, err := rescue.OpenAdmissionController(filepath.Join(t.TempDir(), "admission.db"), admissionConfig(runtimeConfig), wallClock{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admission.Close(); err != nil {
			t.Errorf("admission Close() error = %v", err)
		}
	})

	gate := observability.NewPaidActionGate()
	if runtimeConfig.Policy.EmergencyStop {
		gate.Stop()
	}
	authorizer, transactioner, broadcaster, attempts := dryrun.NewGuards(runtimeConfig.SourceAddress, runtimeConfig.SponsorAddress)
	var logs bytes.Buffer
	logger := observability.NewLogger(&logs)
	network := runtimeConfig.Networks[0].Domain(rescuer)
	coordinator, err := rescue.NewCoordinator(rescue.Config{
		Network:       network,
		Source:        runtimeConfig.SourceAddress,
		Sponsor:       runtimeConfig.SponsorAddress,
		Destination:   runtimeConfig.Destination,
		State:         handoff,
		LeaseManager:  handoff,
		Lease:         lease,
		ProcessFence:  validFence{},
		Budget:        ledger,
		Gate:          gate,
		Admission:     admission,
		FeePolicy:     runtimeFeePolicy(t, runtimeConfig.Networks[0]),
		NativeMinimum: toUint256(t, runtimeConfig.Networks[0].EconomicPolicy.NativeMinimumNetValueWei),
		TrustedTokens: append([]common.Address(nil), runtimeConfig.Networks[0].TrustedTokens...),
	}, authorizer, transactioner, clock.Real{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	reader := &readOnlyRPC{
		chainID:     big.NewInt(int64(network.ChainID)),
		destination: runtimeConfig.Destination,
		rescuer:     rescuer,
	}
	session, err := coordinator.NewSession(context.Background(), 1, reader, reader, broadcaster)
	if err != nil {
		t.Fatal(err)
	}
	candidate := domain.NewBlockCandidate(network.ChainID, domain.CandidateNative, runtimeConfig.SourceAddress, testHash(7), 7)
	if err := session.Handle(context.Background(), candidate); !errors.Is(err, observability.ErrPaidActionsStopped) {
		t.Fatalf("Handle() error = %v, want ErrPaidActionsStopped", err)
	}
	assertNoPaidAttempts(t, attempts)
	if reader.chainCalls.Load() != 1 || reader.finalizedCalls.Load() != 1 || reader.destinationCalls.Load() != 1 || reader.estimateCalls.Load() != 0 || reader.unexpectedCalls.Load() != 0 {
		t.Fatalf("emergency RPC calls = chain:%d finalized:%d destination:%d estimate:%d unexpected:%d",
			reader.chainCalls.Load(), reader.finalizedCalls.Load(), reader.destinationCalls.Load(), reader.estimateCalls.Load(), reader.unexpectedCalls.Load())
	}
	incidents, err := handoff.RescueIncidents(context.Background(), network.ChainID)
	if err != nil || len(incidents) != 0 {
		t.Fatalf("emergency stop incidents = (%v, %v), want none", incidents, err)
	}
	if !strings.Contains(logs.String(), `"error_code":"paid_actions_stopped"`) {
		t.Fatalf("operator log did not preserve safe stop classification: %q", logs.String())
	}
}

func TestDryRunPlanUsesOnlyReadCapabilitiesAndPreservesCandidate(t *testing.T) {
	runtimeConfig := loadRuntime(t, testEnvironment(filepath.Join(t.TempDir(), "dry-run-state")))
	rescuer := testAddress(4)
	handoff := openStore(t, runtimeConfig, rescuer)
	t.Cleanup(func() { closeStoreCleanup(t, handoff) })
	candidate := domain.NewBlockCandidate(runtimeConfig.Networks[0].ChainID, domain.CandidateNative, runtimeConfig.SourceAddress, testHash(5), 5)
	if _, err := handoff.Put(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	queued, err := handoff.Next(context.Background(), candidate.Network)
	if err != nil {
		t.Fatal(err)
	}
	reader := &readOnlyRPC{chainID: big.NewInt(int64(candidate.Network))}
	session, err := dryrun.NewSession(context.Background(), 1, reader, dryrun.Config{
		Network:     candidate.Network,
		Source:      runtimeConfig.SourceAddress,
		Sponsor:     runtimeConfig.SponsorAddress,
		Destination: runtimeConfig.Destination,
		Rescuer:     rescuer,
		ReadTimeout: runtimeConfig.ReadTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Handle(context.Background(), queued); err != nil {
		t.Fatal(err)
	}
	if reader.chainCalls.Load() != 1 || reader.estimateCalls.Load() != 1 || reader.finalizedCalls.Load() != 0 || reader.destinationCalls.Load() != 0 || reader.unexpectedCalls.Load() != 0 {
		t.Fatalf("dry-run RPC calls = chain:%d estimate:%d finalized:%d destination:%d unexpected:%d",
			reader.chainCalls.Load(), reader.estimateCalls.Load(), reader.finalizedCalls.Load(), reader.destinationCalls.Load(), reader.unexpectedCalls.Load())
	}
	replayed, err := handoff.Replay(context.Background(), candidate.Network)
	if err != nil || len(replayed) != 1 || replayed[0].ID != candidate.ID {
		t.Fatalf("dry-run changed durable candidate state: (%v, %v)", replayed, err)
	}
}

func TestTypedRedactionCanariesDoNotEscapeComposedOperatorPath(t *testing.T) {
	const (
		rpcCanary          = "rpc-credential-canary"
		stateCanary        = "state-path-canary"
		sourceKeyCanary    = "source-key-canary"
		sponsorKeyCanary   = "sponsor-key-canary"
		tokenCanary        = "token-metadata-canary"
		amountCanary       = "amount-canary-987654321"
		signatureCanary    = "signature-canary"
		signedTxCanary     = "signed-transaction-canary"
		operatorCodeCanary = "operator-code-canary"
	)
	values := testEnvironment(filepath.Join(t.TempDir(), stateCanary))
	values["RPC_READ_1_HTTP_BASE"] = "https://user:" + rpcCanary + "@read-one.invalid/rpc"
	values["SOURCE_PRIVATE_KEY"] = sourceKeyCanary
	values["SPONSOR_PRIVATE_KEY"] = sponsorKeyCanary
	runtimeConfig := loadRuntime(t, values)
	rescuer := testAddress(4)
	handoff := openStore(t, runtimeConfig, rescuer)
	t.Cleanup(func() { closeStoreCleanup(t, handoff) })
	token := domain.Token{Address: testAddress(8), Symbol: tokenCanary, Decimals: 18}
	candidate := domain.NewLogCandidate(runtimeConfig.Networks[0].ChainID, runtimeConfig.SourceAddress, token, testHash(8), testHash(18), 8, 0)
	if _, err := handoff.Put(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	queued, err := handoff.Next(context.Background(), candidate.Network)
	if err != nil {
		t.Fatal(err)
	}
	reader := &readOnlyRPC{chainID: big.NewInt(int64(candidate.Network))}
	session, err := dryrun.NewSession(context.Background(), 2, reader, dryrun.Config{
		Network:     candidate.Network,
		Source:      runtimeConfig.SourceAddress,
		Sponsor:     runtimeConfig.SponsorAddress,
		Destination: runtimeConfig.Destination,
		Rescuer:     rescuer,
		ReadTimeout: runtimeConfig.ReadTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Handle(context.Background(), queued); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	logger := observability.NewLogger(&output)
	logger.Record(observability.Event{
		Level:       observability.LevelError,
		Code:        observability.EventCode(operatorCodeCanary),
		ChainID:     candidate.Network,
		NetworkName: runtimeConfig.Networks[0].ReadProviders[0].HTTPURL,
		Candidate:   queued.ID,
		TokenSymbol: queued.Token.Symbol,
		Amount:      amountCanary + signatureCanary + signedTxCanary,
		TxHash:      queued.TxHash,
		ErrorCode:   domain.ErrorCode(signatureCanary),
	})
	safeError, err := observability.NewSafeError(observability.ErrorSigning, observability.ErrorSigningFailed, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := logger.Write(observability.SafeEvent{
		Level:    observability.LevelError,
		Code:     observability.LogStateTransition,
		ChainID:  candidate.Network,
		Incident: domain.NewIncidentID(candidate.ID),
		State:    observability.DurableFailed,
		Result:   observability.ResultFailed,
		Error:    &safeError,
	}); err != nil {
		t.Fatal(err)
	}

	surface := output.String() + fmt.Sprintf("%v|%+v|%#v|%+v|%#v",
		runtimeConfig, runtimeConfig.Watch, runtimeConfig.LiveSecrets, runtimeConfig.Networks[0], runtimeConfig.Networks[0].ReadProviders[0])
	for _, canary := range []string{rpcCanary, stateCanary, sourceKeyCanary, sponsorKeyCanary, tokenCanary, amountCanary, signatureCanary, signedTxCanary, operatorCodeCanary} {
		if strings.Contains(surface, canary) {
			t.Fatalf("composed operator surface exposed canary %q: %q", canary, surface)
		}
	}
	for _, safeValue := range []string{`"code":"event_redacted"`, `"error_code":"error_redacted"`, `"error_code":"signing_failed"`} {
		if !strings.Contains(output.String(), safeValue) {
			t.Fatalf("operator log lost safe classification %s: %q", safeValue, output.String())
		}
	}
}

func testEnvironment(stateDirectory string) map[string]string {
	return map[string]string{
		"DRY_RUN":                                     "true",
		"SOURCE_ADDRESS":                              testAddress(1).Hex(),
		"SPONSOR_ADDRESS":                             testAddress(2).Hex(),
		"DESTINATION_ADDRESS":                         testAddress(3).Hex(),
		"ENABLED_NETWORKS":                            "base",
		"RPC_READ_1_HTTP_BASE":                        "https://read-one.invalid/rpc",
		"RPC_READ_1_WS_BASE":                          "wss://read-one.invalid/ws",
		"RPC_READ_1_TRUST_DOMAIN_BASE":                "provider-one",
		"RPC_READ_2_HTTP_BASE":                        "https://read-two.invalid/rpc",
		"RPC_READ_2_WS_BASE":                          "wss://read-two.invalid/ws",
		"RPC_READ_2_TRUST_DOMAIN_BASE":                "provider-two",
		"RESCUER_MANIFEST_BASE":                       "test-only-manifest.json",
		"STATE_DIRECTORY":                             stateDirectory,
		"MAX_TRANSACTION_COST_WEI":                    "100",
		"HOURLY_BUDGET_WEI":                           "100",
		"DAILY_BUDGET_WEI":                            "100",
		"CUMULATIVE_BUDGET_WEI":                       "100",
		"SPONSOR_MIN_BALANCE_WEI":                     "10",
		"NETWORK_MAX_TRANSACTION_COST_WEI_BASE":       "100",
		"NETWORK_HOURLY_BUDGET_WEI_BASE":              "100",
		"NETWORK_DAILY_BUDGET_WEI_BASE":               "100",
		"NETWORK_CUMULATIVE_BUDGET_WEI_BASE":          "100",
		"NETWORK_SPONSOR_MIN_BALANCE_WEI_BASE":        "10",
		"MAX_FEE_PER_GAS_WEI_BASE":                    "100",
		"MAX_PRIORITY_FEE_PER_GAS_WEI_BASE":           "1",
		"TOKEN_GAS_LIMIT_BASE":                        "1",
		"NATIVE_GAS_LIMIT_BASE":                       "1",
		"CHAIN_OVERHEAD_MAX_WEI_BASE":                 "0",
		"NATIVE_MIN_NET_VALUE_WEI_BASE":               "1",
		"UNKNOWN_TOKEN_MAX_TRANSACTION_COST_WEI_BASE": "100",
	}
}

func loadRuntime(t *testing.T, values map[string]string) config.Runtime {
	t.Helper()
	runtimeConfig, err := config.LoadFromMap(values)
	if err != nil {
		t.Fatalf("config.LoadFromMap() error = %v", err)
	}
	return runtimeConfig
}

func openStore(t *testing.T, runtimeConfig config.Runtime, rescuer common.Address) *store.BoltStore {
	t.Helper()
	handoff, err := store.Open(filepath.Join(runtimeConfig.Watch.StateDirectory, "handoff.db"), store.OpenOptions{
		Network:             runtimeConfig.Networks[0].ChainID,
		Source:              runtimeConfig.SourceAddress,
		Sponsor:             runtimeConfig.SponsorAddress,
		Destination:         runtimeConfig.Destination,
		Rescuer:             rescuer,
		PolicyFingerprint:   policyFingerprint,
		MaxPending:          16,
		MaxDiscoveredTokens: 16,
	})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	return handoff
}

func runtimeBudgetPolicy(t *testing.T, runtimeConfig config.Runtime) budget.Policy {
	t.Helper()
	global := runtimeConfig.Policy
	policy := budget.Policy{Global: budget.Limits{
		PerTransaction: toUint256(t, global.MaxTransactionCostWei),
		PerHour:        toUint256(t, global.HourlyBudgetWei),
		PerDay:         toUint256(t, global.DailyBudgetWei),
		Cumulative:     toUint256(t, global.CumulativeBudgetWei),
	}}
	for _, network := range runtimeConfig.Networks {
		economic := network.EconomicPolicy
		policy.Networks = append(policy.Networks, budget.NetworkPolicy{
			Network: network.ChainID,
			Sponsor: runtimeConfig.SponsorAddress,
			Limits: budget.Limits{
				PerTransaction: toUint256(t, economic.MaxTransactionCostWei),
				PerHour:        toUint256(t, economic.HourlyBudgetWei),
				PerDay:         toUint256(t, economic.DailyBudgetWei),
				Cumulative:     toUint256(t, economic.CumulativeBudgetWei),
			},
			TransactionOverhead:     toUint256(t, economic.TransactionOverheadWei),
			EmergencySponsorReserve: toUint256(t, economic.SponsorMinimumBalanceWei),
		})
	}
	return policy
}

func runtimeFeePolicy(t *testing.T, network config.Network) rescue.FeePolicy {
	t.Helper()
	economic := network.EconomicPolicy
	return rescue.FeePolicy{
		Network:                 network.ChainID,
		MaxFeePerGas:            toUint256(t, economic.MaxFeePerGasWei),
		MaxPriorityFeePerGas:    toUint256(t, economic.MaxPriorityFeePerGasWei),
		TokenGasLimit:           economic.TokenGasLimit,
		NativeGasLimit:          economic.NativeGasLimit,
		Overhead:                toUint256(t, economic.TransactionOverheadWei),
		UnknownTokenCostCap:     toUint256(t, economic.UnknownTokenMaxTransactionCostWei),
		TransactionCostCap:      toUint256(t, economic.MaxTransactionCostWei),
		UnboundedAdditionalFees: economic.UnboundedAdditionalFees,
	}
}

func admissionConfig(runtimeConfig config.Runtime) rescue.AdmissionConfig {
	return rescue.AdmissionConfig{
		Window:                    runtimeConfig.Policy.AbuseWindow,
		RateWindow:                time.Minute,
		RateLimit:                 runtimeConfig.Policy.RateLimitPerMinute,
		MaxAttemptsPerToken:       runtimeConfig.Policy.MaxAttemptsPerTokenWindow,
		MaxAttemptsPerSourceEvent: runtimeConfig.Policy.MaxAttemptsPerSourceEvent,
		MaxNewUnknownTokens:       runtimeConfig.Policy.MaxNewUnknownTokensPerWindow,
		Capacity:                  16,
	}
}

func reservationRequest(runtimeConfig config.Runtime, candidate domain.RescueCandidate, attempt uint32) budget.ReservationRequest {
	incident := domain.NewAssetIncidentID(candidate.ID, candidate.Kind, common.Address{})
	return budget.ReservationRequest{
		Network:        candidate.Network,
		Sponsor:        runtimeConfig.SponsorAddress,
		Candidate:      candidate.ID,
		Attempt:        budget.Attempt{Incident: incident, Number: attempt},
		Quote:          budget.CostQuote{GasLimit: 1, MaxFeePerGas: *uint256.NewInt(100)},
		SponsorBalance: *uint256.NewInt(1_000),
	}
}

func toUint256(t *testing.T, value *big.Int) uint256.Int {
	t.Helper()
	converted, overflow := uint256.FromBig(value)
	if overflow || converted == nil {
		t.Fatalf("value %v does not fit uint256", value)
	}
	return *converted
}

func assertNoPaidAttempts(t *testing.T, attempts *dryrun.Attempts) {
	t.Helper()
	if attempts.AuthorizationSignatures() != 0 || attempts.TransactionSignatures() != 0 || attempts.Broadcasts() != 0 {
		t.Fatalf("paid attempts = authorization:%d transaction:%d broadcast:%d, want all zero",
			attempts.AuthorizationSignatures(), attempts.TransactionSignatures(), attempts.Broadcasts())
	}
}

func closeStore(t *testing.T, handoff *store.BoltStore) {
	t.Helper()
	if err := handoff.Close(); err != nil {
		t.Fatalf("store Close() error = %v", err)
	}
}

func closeLedger(t *testing.T, ledger *budget.BudgetLedger) {
	t.Helper()
	if err := ledger.Close(); err != nil {
		t.Fatalf("budget Close() error = %v", err)
	}
}

func closeStoreCleanup(t *testing.T, handoff *store.BoltStore) {
	t.Helper()
	if err := handoff.Close(); err != nil {
		t.Errorf("store Close() error = %v", err)
	}
}

func closeLedgerCleanup(t *testing.T, ledger *budget.BudgetLedger) {
	t.Helper()
	if err := ledger.Close(); err != nil {
		t.Errorf("budget Close() error = %v", err)
	}
}

func testAddress(value byte) common.Address {
	var address common.Address
	address[len(address)-1] = value
	return address
}

func testHash(value byte) common.Hash {
	var hash common.Hash
	hash[len(hash)-1] = value
	return hash
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

type validFence struct{}

func (validFence) Validate() error { return nil }

func (validFence) Release() error { return nil }

type readOnlyRPC struct {
	chainID     *big.Int
	destination common.Address
	rescuer     common.Address

	chainCalls       atomic.Uint64
	estimateCalls    atomic.Uint64
	finalizedCalls   atomic.Uint64
	destinationCalls atomic.Uint64
	unexpectedCalls  atomic.Uint64
}

func (reader *readOnlyRPC) ChainID(context.Context) (*big.Int, error) {
	reader.chainCalls.Add(1)
	return new(big.Int).Set(reader.chainID), nil
}

func (reader *readOnlyRPC) EstimateGas(context.Context, ethereum.CallMsg) (uint64, error) {
	reader.estimateCalls.Add(1)
	return 21_000, nil
}

func (reader *readOnlyRPC) PendingNonceAt(context.Context, common.Address) (uint64, error) {
	reader.unexpectedCalls.Add(1)
	return 0, errors.New("unexpected pending nonce read")
}

func (reader *readOnlyRPC) HeaderByNumber(context.Context, *big.Int) (*types.Header, error) {
	reader.unexpectedCalls.Add(1)
	return nil, errors.New("unexpected fee header read")
}

func (reader *readOnlyRPC) SuggestGasPrice(context.Context) (*big.Int, error) {
	reader.unexpectedCalls.Add(1)
	return nil, errors.New("unexpected gas price read")
}

func (reader *readOnlyRPC) Finalized(context.Context) (guardrpc.BlockRef, error) {
	reader.finalizedCalls.Add(1)
	return guardrpc.BlockRef{Number: 1, Hash: testHash(1)}, nil
}

func (reader *readOnlyRPC) Header(context.Context, uint64) (guardrpc.BlockRef, error) {
	reader.unexpectedCalls.Add(1)
	return guardrpc.BlockRef{}, errors.New("unexpected finalized header read")
}

func (reader *readOnlyRPC) BalanceAt(context.Context, guardrpc.BlockRef, common.Address) (*big.Int, error) {
	reader.unexpectedCalls.Add(1)
	return nil, errors.New("unexpected balance read")
}

func (reader *readOnlyRPC) CodeAt(context.Context, guardrpc.BlockRef, common.Address) ([]byte, error) {
	reader.unexpectedCalls.Add(1)
	return nil, errors.New("unexpected code read")
}

func (reader *readOnlyRPC) CallContract(_ context.Context, _ guardrpc.BlockRef, call ethereum.CallMsg) ([]byte, error) {
	if call.To == nil || *call.To != reader.rescuer {
		reader.unexpectedCalls.Add(1)
		return nil, errors.New("unexpected contract read")
	}
	reader.destinationCalls.Add(1)
	return common.LeftPadBytes(reader.destination.Bytes(), 32), nil
}

func (reader *readOnlyRPC) Receipt(context.Context, common.Hash) (*types.Receipt, error) {
	reader.unexpectedCalls.Add(1)
	return nil, errors.New("unexpected receipt read")
}

var (
	_ rescue.RPCReader      = (*readOnlyRPC)(nil)
	_ rescue.FinalityReader = (*readOnlyRPC)(nil)
	_ dryrun.Reader         = (*readOnlyRPC)(nil)
	_ store.ProcessFence    = validFence{}
	_ rescue.AdmissionClock = wallClock{}
)
