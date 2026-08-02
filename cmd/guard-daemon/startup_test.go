package main

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"guard-daemon/internal/clock"
	"guard-daemon/internal/config"
	"guard-daemon/internal/contracts"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"
	"guard-daemon/internal/rescue"
	"guard-daemon/internal/rescue/dryrun"
	"guard-daemon/internal/rpc"
	"guard-daemon/internal/store"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestDryRunStartupHasNoProductionSigningOrBroadcastGraph(t *testing.T) {
	production := newProductionDependencies(observability.Discard{}, config.ModeDryRun)
	if production.newSigners != nil || production.dialSubmission != nil || production.attestNetwork != nil {
		t.Fatal("dry-run production dependencies содержат live capability")
	}

	runtimeConfig := testRuntime(t, []domain.Network{testNetwork("dry-network", 401)})
	runtimeConfig.Mode = config.ModeDryRun
	var networkCalls atomic.Int32
	dependencies := daemonDependencies{
		serviceClock: clock.Real{},
		observer:     observability.Discard{},
		dial: func(context.Context, string, uint64) (generationClient, error) {
			networkCalls.Add(1)
			return nil, errors.New("неожиданный network access")
		},
		loadManifest: func(config.Runtime, config.Network) (contracts.DeploymentManifest, error) {
			return contracts.DeploymentManifest{Address: testProcessAddress(4)}, nil
		},
	}
	dependencies = completeTestRuntimeDependencies(dependencies)

	process, err := newDaemon(context.Background(), runtimeConfig, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if networkCalls.Load() != 0 {
		t.Fatalf("network calls during dry startup = %d, нужно 0", networkCalls.Load())
	}
	if len(process.networks) != 1 {
		t.Fatalf("network process count = %d, нужно 1", len(process.networks))
	}
	network := process.networks[0]
	if network.coordinator != nil || network.dryAuthorizer == nil || network.dryTransactioner == nil || network.dryBroadcaster == nil || network.dryAttempts == nil {
		t.Fatal("dry-run graph содержит coordinator или не содержит fail-closed guards")
	}
	if network.dryAttempts.AuthorizationSignatures() != 0 || network.dryAttempts.TransactionSignatures() != 0 || network.dryAttempts.Broadcasts() != 0 {
		t.Fatal("dry-run startup вызвал signing или broadcast guard")
	}
}

func TestLiveStartupAttestsEveryNetworkBeforeConstructingSigners(t *testing.T) {
	runtimeConfig := testRuntime(t, []domain.Network{
		testNetwork("first-network", 501),
		testNetwork("second-network", 502),
	})
	var events []string
	dependencies := startupDependencies(t)
	dependencies.loadManifest = func(_ config.Runtime, network config.Network) (contracts.DeploymentManifest, error) {
		events = append(events, "manifest:"+network.Name)
		return contracts.DeploymentManifest{Address: testProcessAddress(byte(network.ChainID))}, nil
	}
	dependencies.attestNetwork = func(_ context.Context, network config.Network, _ contracts.DeploymentManifest, timeout time.Duration) error {
		if timeout != runtimeConfig.ReadTimeout {
			t.Fatalf("attestation timeout = %s, нужен %s", timeout, runtimeConfig.ReadTimeout)
		}
		events = append(events, "attest:"+network.Name)
		return nil
	}
	dependencies.openStore = func(_ config.Runtime, configured config.Network, network domain.Network) (store.HandoffStore, error) {
		events = append(events, "store:"+configured.Name)
		return newLegacyMemoryHandoff(network.ChainID, dependencies.serviceClock), nil
	}
	dependencies.newSigners = func(config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
		events = append(events, "signers")
		return testPrivateKeySigners(t)
	}

	if _, err := newDaemon(context.Background(), runtimeConfig, dependencies); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"manifest:first-network",
		"manifest:second-network",
		"attest:first-network",
		"attest:second-network",
		"store:first-network",
		"store:second-network",
		"signers",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("startup order = %v, нужен %v", events, want)
	}
}

func TestStateFailureBlocksSignerConstruction(t *testing.T) {
	runtimeConfig := testRuntime(t, []domain.Network{testNetwork("state-network", 650)})
	dependencies := startupDependencies(t)
	var signerConstructions atomic.Int32
	dependencies.openStore = func(config.Runtime, config.Network, domain.Network) (store.HandoffStore, error) {
		return nil, errors.New("private state path detail")
	}
	dependencies.newSigners = func(config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
		signerConstructions.Add(1)
		return testPrivateKeySigners(t)
	}

	err := func() error {
		_, err := newDaemon(context.Background(), runtimeConfig, dependencies)
		return err
	}()
	if publicErrorCode(err) != errorStoreFailed || signerConstructions.Load() != 0 {
		t.Fatalf("state failure: error=%v signer constructions=%d", err, signerConstructions.Load())
	}
}

func TestAttestationFailureBlocksSignerConstruction(t *testing.T) {
	runtimeConfig := testRuntime(t, []domain.Network{
		testNetwork("first-network", 601),
		testNetwork("second-network", 602),
	})
	dependencies := startupDependencies(t)
	var attestations atomic.Int32
	var signerConstructions atomic.Int32
	dependencies.attestNetwork = func(context.Context, config.Network, contracts.DeploymentManifest, time.Duration) error {
		if attestations.Add(1) == 2 {
			return errors.New("test-only quorum disagreement")
		}
		return nil
	}
	dependencies.newSigners = func(config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
		signerConstructions.Add(1)
		return testPrivateKeySigners(t)
	}

	if _, err := newDaemon(context.Background(), runtimeConfig, dependencies); err == nil {
		t.Fatal("newDaemon() не отклонил расхождение quorum")
	}
	if attestations.Load() != 2 || signerConstructions.Load() != 0 {
		t.Fatalf("attestations=%d signer constructions=%d", attestations.Load(), signerConstructions.Load())
	}
}

func TestSignerAddressMismatchFailsBeforeSigningOrBroadcast(t *testing.T) {
	tests := []struct {
		name          string
		authorizer    commonAddressSelector
		transactioner commonAddressSelector
	}{
		{name: "source", authorizer: wrongAddress, transactioner: sponsorAddress},
		{name: "sponsor", authorizer: sourceAddress, transactioner: wrongAddress},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtimeConfig := testRuntime(t, []domain.Network{testNetwork("mismatch-network", 701)})
			dependencies := startupDependencies(t)
			var broadcastDials atomic.Int32
			dependencies.dialSubmission = func(context.Context, string) (submissionClient, error) {
				broadcastDials.Add(1)
				return &stubSubmissionClient{}, nil
			}
			var attempts *dryrun.Attempts
			dependencies.newSigners = func(config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
				authorizer, transactioner, _, guardAttempts := dryrun.NewGuards(
					test.authorizer(runtimeConfig),
					test.transactioner(runtimeConfig),
				)
				attempts = guardAttempts
				return authorizer, transactioner, nil
			}

			if _, err := newDaemon(context.Background(), runtimeConfig, dependencies); err == nil {
				t.Fatal("newDaemon() не отклонил адрес signer")
			}
			if attempts == nil || attempts.AuthorizationSignatures() != 0 || attempts.TransactionSignatures() != 0 || attempts.Broadcasts() != 0 || broadcastDials.Load() != 0 {
				t.Fatalf("mismatch вызвал side effect: attempts=%v broadcast dials=%d", attempts, broadcastDials.Load())
			}
		})
	}
}

func TestLiveSessionUsesSeparateConfiguredBroadcastRPC(t *testing.T) {
	runtimeConfig := testRuntime(t, []domain.Network{testNetwork("separate-broadcast", 801)})
	dependencies := startupDependencies(t)
	var attempts *dryrun.Attempts
	dependencies.newSigners = func(config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
		authorizer, transactioner, _, guardAttempts := dryrun.NewGuards(runtimeConfig.SourceAddress, runtimeConfig.SponsorAddress)
		attempts = guardAttempts
		return authorizer, transactioner, nil
	}
	submission := &countingSubmissionClient{}
	var dialedEndpoint string
	dependencies.dialSubmission = func(_ context.Context, endpoint string) (submissionClient, error) {
		dialedEndpoint = endpoint
		return submission, nil
	}
	process, err := newDaemon(context.Background(), runtimeConfig, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	network := process.networks[0]
	reader := &startupGenerationReader{
		chainID:     big.NewInt(int64(network.network.ChainID)),
		destination: runtimeConfig.Destination,
		delegatedTo: network.network.Rescuer,
	}
	client := &readGenerationClient{generation: 1, reader: reader}
	session, err := network.openSession(context.Background(), client, process.dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if dialedEndpoint != network.configured.BroadcastHTTP {
		t.Fatalf("broadcast endpoint = %q, нужен configured override", dialedEndpoint)
	}
	if attempts == nil || attempts.AuthorizationSignatures() != 0 || attempts.TransactionSignatures() != 0 || submission.broadcasts.Load() != 0 {
		t.Fatal("создание live session вызвало подпись или отправку при активной делегации")
	}
	session.Close()
	if submission.closed.Load() != 1 {
		t.Fatalf("broadcast client close count = %d, нужен 1", submission.closed.Load())
	}
}

func TestConfiguredAttestationUsesBothHTTPOverridesAndProviderIdentity(t *testing.T) {
	runtimeConfig := testRuntime(t, []domain.Network{testNetwork("attested-network", 901)})
	network := runtimeConfig.Networks[0]
	clients := []*fakeAttestationClient{{}, {}}
	var endpoints []string
	dial := func(ctx context.Context, endpoint string) (attestationClient, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("attestation dial не получил deadline")
		}
		endpoints = append(endpoints, endpoint)
		return clients[len(endpoints)-1], nil
	}
	attestCalls := 0
	err := attestConfiguredNetworkWith(
		context.Background(),
		network,
		contracts.DeploymentManifest{Address: testProcessAddress(4)},
		runtimeConfig.ReadTimeout,
		dial,
		func(_ context.Context, _ contracts.DeploymentManifest, providers []contracts.ReadProvider, timeout time.Duration) error {
			attestCalls++
			if timeout != runtimeConfig.ReadTimeout || len(providers) != 2 {
				t.Fatalf("attestation input: timeout=%s providers=%d", timeout, len(providers))
			}
			for index, provider := range providers {
				configured := network.ReadProviders[index]
				if provider.ID != configured.ID || provider.EndpointFingerprint != configured.EndpointFingerprint || provider.TrustDomain != configured.TrustDomain || provider.Reader != clients[index] {
					t.Fatalf("provider %d не совпал с config override", index)
				}
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	wantEndpoints := []string{network.ReadProviders[0].HTTPURL, network.ReadProviders[1].HTTPURL}
	if !reflect.DeepEqual(endpoints, wantEndpoints) || attestCalls != 1 {
		t.Fatalf("attestation endpoints=%v calls=%d", endpoints, attestCalls)
	}
	for index, client := range clients {
		if client.closed.Load() != 1 {
			t.Fatalf("attestation client %d close count = %d", index, client.closed.Load())
		}
	}
}

func TestDryRunSessionPlansWithoutCallingGuards(t *testing.T) {
	runtimeConfig := testRuntime(t, []domain.Network{testNetwork("dry-plan", 902)})
	runtimeConfig.Mode = config.ModeDryRun
	allowedToken := testProcessAddress(9)
	runtimeConfig.Networks[0].Tokens = []domain.Token{{Address: allowedToken}}
	dependencies := daemonDependencies{
		serviceClock: clock.Real{},
		observer:     observability.Discard{},
		dial: func(context.Context, string, uint64) (generationClient, error) {
			return nil, errors.New("unexpected watcher dial")
		},
		loadManifest: func(config.Runtime, config.Network) (contracts.DeploymentManifest, error) {
			return contracts.DeploymentManifest{Address: testProcessAddress(4)}, nil
		},
		newSession: func(context.Context, *networkProcess, generationClient) (candidateSession, error) {
			t.Fatal("dry run вызвал injected session factory")
			return nil, nil
		},
	}
	dependencies = completeTestRuntimeDependencies(dependencies)
	process, err := newDaemon(context.Background(), runtimeConfig, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	network := process.networks[0]
	reader := &startupGenerationReader{chainID: big.NewInt(int64(network.network.ChainID))}
	session, err := network.openSession(context.Background(), &readGenerationClient{generation: 3, reader: reader}, process.dependencies)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	candidate := domain.RescueCandidate{
		Network: network.network.ChainID,
		Source:  runtimeConfig.SourceAddress,
		Kind:    domain.CandidateToken,
		Token:   domain.Token{Address: allowedToken},
	}
	if err := session.Handle(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	if len(reader.estimates) != 1 {
		t.Fatalf("EstimateGas calls = %d, нужен 1", len(reader.estimates))
	}
	if network.dryAttempts.AuthorizationSignatures() != 0 || network.dryAttempts.TransactionSignatures() != 0 || network.dryAttempts.Broadcasts() != 0 {
		t.Fatal("обычный dry-run путь вызвал signing или broadcast guard")
	}
}

func TestSessionTokenPolicyRejectsInjectedCandidateBeforeHandler(t *testing.T) {
	allowed := testProcessAddress(10)
	network := &networkProcess{
		network:       domain.Network{AllowUnknownTokens: false},
		allowedTokens: map[common.Address]struct{}{allowed: {}},
	}
	base := &countingCandidateSession{generation: 1}
	session := network.bindTokenPolicy(base)
	err := session.Handle(context.Background(), domain.RescueCandidate{
		Kind:  domain.CandidateToken,
		Token: domain.Token{Address: testProcessAddress(11)},
	})
	if publicErrorCode(err) != errorTokenForbidden || base.handles.Load() != 0 {
		t.Fatalf("forbidden candidate: error=%v handler calls=%d", err, base.handles.Load())
	}

	network.network.AllowUnknownTokens = true
	base = &countingCandidateSession{generation: 1}
	session = network.bindTokenPolicy(base)
	if err := session.Handle(context.Background(), domain.RescueCandidate{Kind: domain.CandidateToken, Token: domain.Token{Address: testProcessAddress(11)}}); err != nil {
		t.Fatal(err)
	}
	if base.handles.Load() != 1 {
		t.Fatalf("all-token handler calls = %d, нужен 1", base.handles.Load())
	}
}

type commonAddressSelector func(config.Runtime) common.Address

func sourceAddress(runtimeConfig config.Runtime) common.Address  { return runtimeConfig.SourceAddress }
func sponsorAddress(runtimeConfig config.Runtime) common.Address { return runtimeConfig.SponsorAddress }
func wrongAddress(config.Runtime) common.Address                 { return testProcessAddress(0xfe) }

func startupDependencies(t *testing.T) daemonDependencies {
	t.Helper()
	dependencies := daemonDependencies{
		serviceClock: clock.Real{},
		observer:     observability.Discard{},
		dial: func(context.Context, string, uint64) (generationClient, error) {
			return nil, errors.New("неожиданный watcher dial")
		},
		loadManifest: func(config.Runtime, config.Network) (contracts.DeploymentManifest, error) {
			return contracts.DeploymentManifest{Address: testProcessAddress(4)}, nil
		},
		attestNetwork: func(context.Context, config.Network, contracts.DeploymentManifest, time.Duration) error {
			return nil
		},
		dialSubmission: func(context.Context, string) (submissionClient, error) {
			return &stubSubmissionClient{}, nil
		},
	}
	dependencies.newSigners = func(config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
		return testPrivateKeySigners(t)
	}
	return completeTestRuntimeDependencies(dependencies)
}

func testPrivateKeySigners(t *testing.T) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
	t.Helper()
	authorizer, err := rescue.NewPrivateKeyAuthorizationSigner(deterministicKey(t, 1))
	if err != nil {
		return nil, nil, err
	}
	transactioner, err := rescue.NewPrivateKeyTransactionSigner(deterministicKey(t, 2))
	return authorizer, transactioner, err
}

type countingSubmissionClient struct {
	broadcasts atomic.Int32
	closed     atomic.Int32
}

func (client *countingSubmissionClient) SendTransaction(context.Context, *types.Transaction) error {
	client.broadcasts.Add(1)
	return nil
}

func (client *countingSubmissionClient) Close() { client.closed.Add(1) }

type countingCandidateSession struct {
	generation uint64
	handles    atomic.Int32
}

func (session *countingCandidateSession) Generation() uint64 { return session.generation }
func (session *countingCandidateSession) Handle(context.Context, domain.RescueCandidate) error {
	session.handles.Add(1)
	return nil
}
func (*countingCandidateSession) Close() {}

type readGenerationClient struct {
	generation uint64
	reader     rpc.Reader
}

func (client *readGenerationClient) Generation() uint64          { return client.generation }
func (client *readGenerationClient) Reader() rpc.Reader          { return client.reader }
func (*readGenerationClient) LogSubscriber() rpc.LogSubscriber   { return nil }
func (*readGenerationClient) HeadSubscriber() rpc.HeadSubscriber { return nil }
func (*readGenerationClient) Close()                             {}

type startupGenerationReader struct {
	chainID     *big.Int
	destination common.Address
	delegatedTo common.Address
	estimates   []ethereum.CallMsg
}

func (reader *startupGenerationReader) ChainID(context.Context) (*big.Int, error) {
	return new(big.Int).Set(reader.chainID), nil
}

func (*startupGenerationReader) BlockNumber(context.Context) (uint64, error) {
	return 0, errors.New("unexpected call")
}

func (*startupGenerationReader) HeaderByNumber(context.Context, *big.Int) (*types.Header, error) {
	return nil, errors.New("unexpected call")
}

func (*startupGenerationReader) BalanceAt(context.Context, common.Address, *big.Int) (*big.Int, error) {
	return nil, errors.New("unexpected call")
}

func (reader *startupGenerationReader) CodeAt(context.Context, common.Address, *big.Int) ([]byte, error) {
	return append([]byte{0xef, 0x01, 0x00}, reader.delegatedTo.Bytes()...), nil
}

func (*startupGenerationReader) NonceAt(context.Context, common.Address, *big.Int) (uint64, error) {
	return 0, errors.New("unexpected call")
}

func (*startupGenerationReader) PendingNonceAt(context.Context, common.Address) (uint64, error) {
	return 0, errors.New("unexpected call")
}

func (reader *startupGenerationReader) CallContract(context.Context, ethereum.CallMsg, *big.Int) ([]byte, error) {
	result := make([]byte, 32)
	copy(result[12:], reader.destination[:])
	return result, nil
}

func (*startupGenerationReader) SuggestGasPrice(context.Context) (*big.Int, error) {
	return nil, errors.New("unexpected call")
}

func (*startupGenerationReader) SuggestGasTipCap(context.Context) (*big.Int, error) {
	return nil, errors.New("unexpected call")
}

func (reader *startupGenerationReader) EstimateGas(_ context.Context, call ethereum.CallMsg) (uint64, error) {
	reader.estimates = append(reader.estimates, call)
	return 100_000, nil
}

func (*startupGenerationReader) TransactionReceipt(context.Context, common.Hash) (*types.Receipt, error) {
	return nil, errors.New("unexpected call")
}

func (*startupGenerationReader) FilterLogs(context.Context, ethereum.FilterQuery) ([]types.Log, error) {
	return nil, errors.New("unexpected call")
}

var _ rpc.Reader = (*startupGenerationReader)(nil)

type fakeAttestationClient struct {
	closed atomic.Int32
}

func (*fakeAttestationClient) ChainID(context.Context) (*big.Int, error) {
	return nil, errors.New("unexpected call")
}

func (*fakeAttestationClient) HeaderByNumber(context.Context, *big.Int) (*types.Header, error) {
	return nil, errors.New("unexpected call")
}

func (*fakeAttestationClient) TransactionByHash(context.Context, common.Hash) (*types.Transaction, bool, error) {
	return nil, false, errors.New("unexpected call")
}

func (*fakeAttestationClient) TransactionReceipt(context.Context, common.Hash) (*types.Receipt, error) {
	return nil, errors.New("unexpected call")
}

func (*fakeAttestationClient) CodeAtHash(context.Context, common.Address, common.Hash) ([]byte, error) {
	return nil, errors.New("unexpected call")
}

func (*fakeAttestationClient) CallContractAtHash(context.Context, ethereum.CallMsg, common.Hash) ([]byte, error) {
	return nil, errors.New("unexpected call")
}

func (client *fakeAttestationClient) Close() { client.closed.Add(1) }

var _ attestationClient = (*fakeAttestationClient)(nil)
