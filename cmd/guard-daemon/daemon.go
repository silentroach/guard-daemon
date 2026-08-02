package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	"guard-daemon/internal/watcher"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

const (
	reconnectDelay       = 5 * time.Second
	candidateRetryDelay  = 5 * time.Second
	maxPendingCandidates = uint32(1024)
	maxDiscoveredTokens  = uint32(1024)
	maxWatcherLookback   = uint64(10_000)
)

const (
	errorStartupInvalid      domain.ErrorCode = "daemon_startup_invalid"
	errorDialFailed          domain.ErrorCode = "daemon_rpc_dial_failed"
	errorClientInvalid       domain.ErrorCode = "daemon_rpc_client_invalid"
	errorSessionFailed       domain.ErrorCode = "daemon_session_failed"
	errorWatcherFailed       domain.ErrorCode = "daemon_watcher_failed"
	errorWorkerStopped       domain.ErrorCode = "daemon_worker_stopped"
	errorQueueNextFailed     domain.ErrorCode = "daemon_queue_next_failed"
	errorIncidentPutFailed   domain.ErrorCode = "daemon_incident_put_failed"
	errorCandidateNackFailed domain.ErrorCode = "daemon_candidate_nack_failed"
	errorCandidateAckFailed  domain.ErrorCode = "daemon_candidate_ack_failed"
	errorTokenForbidden      domain.ErrorCode = "daemon_token_forbidden"
	errorStoreFailed         domain.ErrorCode = "daemon_store_failed"
	errorQuorumFailed        domain.ErrorCode = "daemon_runtime_quorum_failed"
)

const (
	eventHTTPFallback    observability.EventCode = "daemon_rpc_http_fallback"
	eventGenerationError observability.EventCode = "daemon_generation_failed"
)

type generationClient interface {
	Generation() uint64
	Reader() rpc.Reader
	LogSubscriber() rpc.LogSubscriber
	HeadSubscriber() rpc.HeadSubscriber
	Close()
}

type submissionClient interface {
	rpc.Broadcaster
	Close()
}

type attestationClient interface {
	contracts.AttestationReader
	Close()
}

type generationRunner interface {
	Run(context.Context) error
}

type runtimeQuorum interface {
	rpc.FinalizedReader
	Close()
}

type candidateHandler interface {
	Generation() uint64
	Handle(context.Context, domain.RescueCandidate) error
}

type candidateSession interface {
	candidateHandler
	Close()
}

type preparedNetwork struct {
	configured config.Network
	manifest   contracts.DeploymentManifest
	handoff    store.HandoffStore
}

type daemonDependencies struct {
	serviceClock   clock.Clock
	observer       observability.Observer
	dial           func(context.Context, string, uint64) (generationClient, error)
	dialSubmission func(context.Context, string) (submissionClient, error)
	loadManifest   func(config.Runtime, config.Network) (contracts.DeploymentManifest, error)
	attestNetwork  func(context.Context, config.Network, contracts.DeploymentManifest, time.Duration) error
	newSigners     func(config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error)
	newSession     func(context.Context, *networkProcess, generationClient) (candidateSession, error)
	newWatcher     func(watcher.Dependencies) (generationRunner, error)
	openStore      func(config.Runtime, config.Network, domain.Network) (store.HandoffStore, error)
	openQuorum     func(context.Context, config.Network, time.Duration) (runtimeQuorum, error)
}

type daemon struct {
	dependencies daemonDependencies
	networks     []*networkProcess
}

type networkProcess struct {
	configured       config.Network
	network          domain.Network
	mode             config.Mode
	source           common.Address
	sponsor          common.Address
	destination      common.Address
	readTimeout      time.Duration
	watchLookback    uint64
	coordinator      *rescue.Coordinator
	dryAuthorizer    rescue.AuthorizationSigner
	dryTransactioner rescue.TransactionSigner
	dryBroadcaster   rpc.Broadcaster
	dryAttempts      *dryrun.Attempts
	handoff          store.HandoffStore
	codec            *contracts.ERC20Codec
	allowedTokens    map[common.Address]struct{}
}

type generationClientView struct {
	generationClient
	reader rpc.Reader
	logs   rpc.LogSubscriber
	heads  rpc.HeadSubscriber
}

func (client *generationClientView) Reader() rpc.Reader                 { return client.reader }
func (client *generationClientView) LogSubscriber() rpc.LogSubscriber   { return client.logs }
func (client *generationClientView) HeadSubscriber() rpc.HeadSubscriber { return client.heads }

type liveCandidateSession struct {
	*rescue.Session
	submission submissionClient
}

type tokenPolicySession struct {
	candidateSession
	allowed      map[common.Address]struct{}
	allowUnknown bool
}

func (session *tokenPolicySession) Handle(ctx context.Context, candidate domain.RescueCandidate) error {
	if candidate.Kind == domain.CandidateToken {
		if candidate.Token.Address == (common.Address{}) {
			return processError("daemon.token_policy", domain.ErrorConfiguration, errorTokenForbidden, false, nil)
		}
		if _, allowed := session.allowed[candidate.Token.Address]; !session.allowUnknown && !allowed {
			return processError("daemon.token_policy", domain.ErrorConfiguration, errorTokenForbidden, false, nil)
		}
	}
	return session.candidateSession.Handle(ctx, candidate)
}

func (session *liveCandidateSession) Close() {
	session.submission.Close()
}

func newProductionDependencies(observer observability.Observer, mode config.Mode) daemonDependencies {
	dialer := rpc.EthClientDialer{}
	dependencies := daemonDependencies{
		serviceClock: clock.Real{},
		observer:     observer,
		dial: func(ctx context.Context, endpoint string, generation uint64) (generationClient, error) {
			return dialer.DialContext(ctx, endpoint, generation)
		},
		loadManifest: loadConfiguredManifest,
		openStore: func(runtimeConfig config.Runtime, _ config.Network, network domain.Network) (store.HandoffStore, error) {
			path := filepath.Join(runtimeConfig.Watch.StateDirectory, "watcher-"+strconv.FormatInt(int64(network.ChainID), 10)+".db")
			return store.Open(path, store.OpenOptions{
				Network:             network.ChainID,
				Source:              runtimeConfig.SourceAddress,
				PolicyFingerprint:   watcher.PolicyFingerprint(network, runtimeConfig.Watch.LookbackBlocks),
				MaxPending:          maxPendingCandidates,
				MaxDiscoveredTokens: maxDiscoveredTokens,
				Clock:               clock.Real{},
			})
		},
		openQuorum: openRuntimeQuorum,
	}
	if mode.IsLive() {
		dependencies.dialSubmission = dialSubmissionClient
		dependencies.attestNetwork = attestConfiguredNetwork
		dependencies.newSigners = newPrivateKeySigners
	}
	return dependencies
}

func openRuntimeQuorum(ctx context.Context, network config.Network, timeout time.Duration) (runtimeQuorum, error) {
	endpoints := make([]rpc.ProviderEndpoint, 0, len(network.ReadProviders))
	for _, provider := range network.ReadProviders {
		endpoints = append(endpoints, rpc.ProviderEndpoint{
			Identity: rpc.ProviderIdentity{
				ID: provider.ID, Fingerprint: provider.EndpointFingerprint, TrustDomain: provider.TrustDomain,
			},
			Endpoint: provider.HTTPURL,
		})
	}
	return rpc.DialQuorum(ctx, endpoints, timeout)
}

func loadConfiguredManifest(runtimeConfig config.Runtime, network config.Network) (contracts.DeploymentManifest, error) {
	manifestFile, err := os.Open(network.ManifestPath)
	if err != nil {
		return contracts.DeploymentManifest{}, errors.New("не удалось открыть deployment manifest")
	}
	defer manifestFile.Close()

	artifactFile, err := os.Open(runtimeConfig.Artifact.Path)
	if err != nil {
		return contracts.DeploymentManifest{}, errors.New("не удалось открыть canonical artifact")
	}
	defer artifactFile.Close()

	return contracts.LoadTrustedManifest(manifestFile, artifactFile, contracts.ManifestExpectations{
		ChainID:        strconv.FormatInt(int64(network.ChainID), 10),
		ContractRole:   "rescuer",
		Destination:    runtimeConfig.Destination,
		Sponsor:        runtimeConfig.SponsorAddress,
		ArtifactSHA256: runtimeConfig.Artifact.SHA256,
		SourceProvenance: contracts.SourceProvenance{
			Kind:  runtimeConfig.Artifact.SourceKind,
			Value: runtimeConfig.Artifact.SourceValue,
		},
		CompilerVersion: runtimeConfig.Artifact.CompilerVersion,
	})
}

func attestConfiguredNetwork(ctx context.Context, network config.Network, manifest contracts.DeploymentManifest, readTimeout time.Duration) error {
	return attestConfiguredNetworkWith(
		ctx,
		network,
		manifest,
		readTimeout,
		func(dialContext context.Context, endpoint string) (attestationClient, error) {
			return ethclient.DialContext(dialContext, endpoint)
		},
		contracts.AttestDeployment,
	)
}

func attestConfiguredNetworkWith(
	ctx context.Context,
	network config.Network,
	manifest contracts.DeploymentManifest,
	readTimeout time.Duration,
	dial func(context.Context, string) (attestationClient, error),
	attest func(context.Context, contracts.DeploymentManifest, []contracts.ReadProvider, time.Duration) error,
) error {
	if dial == nil || attest == nil {
		return errors.New("не заданы зависимости аттестации")
	}
	providers := make([]contracts.ReadProvider, 0, len(network.ReadProviders))
	clients := make([]attestationClient, 0, len(network.ReadProviders))
	defer func() {
		for _, client := range clients {
			client.Close()
		}
	}()

	for _, configuredProvider := range network.ReadProviders {
		dialContext, cancel := context.WithTimeout(ctx, readTimeout)
		client, err := dial(dialContext, configuredProvider.HTTPURL)
		cancel()
		if err != nil || client == nil {
			return domain.NewError("daemon.attestation_dial", domain.ErrorRPCTransient, errorDialFailed, true, false, err)
		}
		clients = append(clients, client)
		providers = append(providers, contracts.ReadProvider{
			ID:                  configuredProvider.ID,
			EndpointFingerprint: configuredProvider.EndpointFingerprint,
			TrustDomain:         configuredProvider.TrustDomain,
			Reader:              client,
		})
	}
	return attest(ctx, manifest, providers, readTimeout)
}

func newPrivateKeySigners(secrets config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
	sourceKey, sponsorKey, ok := secrets.PrivateKeys()
	if !ok {
		return nil, nil, errors.New("live private keys не заданы")
	}
	authorizer, err := rescue.NewPrivateKeyAuthorizationSigner(sourceKey)
	if err != nil {
		return nil, nil, err
	}
	transactioner, err := rescue.NewPrivateKeyTransactionSigner(sponsorKey)
	if err != nil {
		return nil, nil, err
	}
	return authorizer, transactioner, nil
}

func dialSubmissionClient(ctx context.Context, endpoint string) (submissionClient, error) {
	client, err := ethclient.DialContext(ctx, endpoint)
	if err != nil {
		return nil, domain.NewError("daemon.broadcast_dial", domain.ErrorRPCTransient, errorDialFailed, true, false, err)
	}
	return client, nil
}

func newDaemon(ctx context.Context, runtimeConfig config.Runtime, dependencies daemonDependencies) (*daemon, error) {
	if ctx == nil {
		return nil, processError("daemon.context", domain.ErrorConfiguration, errorStartupInvalid, false, nil)
	}
	if (!runtimeConfig.Mode.IsDryRun() && !runtimeConfig.Mode.IsLive()) || runtimeConfig.SourceAddress == (common.Address{}) || runtimeConfig.SponsorAddress == (common.Address{}) || runtimeConfig.Destination == (common.Address{}) || runtimeConfig.SourceAddress == runtimeConfig.SponsorAddress || runtimeConfig.SourceAddress == runtimeConfig.Destination || runtimeConfig.SponsorAddress == runtimeConfig.Destination || runtimeConfig.ReadTimeout <= 0 || runtimeConfig.Watch.StateDirectory == "" || strings.IndexByte(runtimeConfig.Watch.StateDirectory, 0) >= 0 || filepath.Clean(runtimeConfig.Watch.StateDirectory) != runtimeConfig.Watch.StateDirectory || runtimeConfig.Watch.LookbackBlocks == 0 || runtimeConfig.Watch.LookbackBlocks > maxWatcherLookback {
		return nil, processError("daemon.config", domain.ErrorConfiguration, errorStartupInvalid, false, nil)
	}
	dependencies, err := dependencies.withDefaults(runtimeConfig.Mode)
	if err != nil {
		return nil, processError("daemon.dependencies", domain.ErrorConfiguration, errorStartupInvalid, false, err)
	}

	prepared := make([]preparedNetwork, 0, len(runtimeConfig.Networks))
	for _, configuredNetwork := range runtimeConfig.Networks {
		manifest, err := dependencies.loadManifest(runtimeConfig, configuredNetwork)
		if err != nil || manifest.Address == (common.Address{}) {
			return nil, processError("daemon.manifest", domain.ErrorConfiguration, errorStartupInvalid, false, err)
		}
		prepared = append(prepared, preparedNetwork{configured: configuredNetwork, manifest: manifest})
	}
	if len(prepared) == 0 {
		return nil, processError("daemon.networks", domain.ErrorConfiguration, errorStartupInvalid, false, nil)
	}

	if runtimeConfig.Mode.IsLive() {
		for _, network := range prepared {
			if err := dependencies.attestNetwork(ctx, network.configured, network.manifest, runtimeConfig.ReadTimeout); err != nil {
				return nil, processError("daemon.attestation", domain.ErrorConfiguration, errorStartupInvalid, false, err)
			}
		}
	}

	storesOpen := true
	defer func() {
		if !storesOpen {
			return
		}
		for _, network := range prepared {
			if network.handoff != nil {
				_ = network.handoff.Close()
			}
		}
	}()
	for index := range prepared {
		network := prepared[index].configured.Domain(prepared[index].manifest.Address)
		handoff, openErr := dependencies.openStore(runtimeConfig, prepared[index].configured, network)
		if openErr != nil || handoff == nil {
			return nil, processError("daemon.store", domain.ErrorInternal, errorStoreFailed, false, openErr)
		}
		prepared[index].handoff = handoff
	}

	var authorizer rescue.AuthorizationSigner
	var transactioner rescue.TransactionSigner
	if runtimeConfig.Mode.IsLive() {
		authorizer, transactioner, err = dependencies.newSigners(runtimeConfig.LiveSecrets)
		if err != nil || authorizer == nil || transactioner == nil {
			return nil, processError("daemon.signers", domain.ErrorConfiguration, errorStartupInvalid, false, err)
		}
		if authorizer.Address() != runtimeConfig.SourceAddress || transactioner.Address() != runtimeConfig.SponsorAddress {
			return nil, processError("daemon.signer_address", domain.ErrorConfiguration, errorStartupInvalid, false, nil)
		}
	}

	codec, err := contracts.NewERC20Codec()
	if err != nil {
		return nil, processError("daemon.erc20_codec", domain.ErrorInternal, errorStartupInvalid, false, err)
	}

	process := &daemon{dependencies: dependencies, networks: make([]*networkProcess, 0, len(prepared))}
	for _, preparedNetwork := range prepared {
		network := preparedNetwork.configured.Domain(preparedNetwork.manifest.Address)
		var coordinator *rescue.Coordinator
		if runtimeConfig.Mode.IsLive() {
			coordinator, err = rescue.NewCoordinator(rescue.Config{
				Network:     network,
				Source:      runtimeConfig.SourceAddress,
				Sponsor:     runtimeConfig.SponsorAddress,
				Destination: runtimeConfig.Destination,
			}, authorizer, transactioner, dependencies.serviceClock, dependencies.observer)
			if err != nil {
				return nil, processError("daemon.coordinator", domain.ErrorConfiguration, errorStartupInvalid, false, err)
			}
		}

		networkState := &networkProcess{
			configured:    preparedNetwork.configured,
			network:       network,
			mode:          runtimeConfig.Mode,
			source:        runtimeConfig.SourceAddress,
			sponsor:       runtimeConfig.SponsorAddress,
			destination:   runtimeConfig.Destination,
			readTimeout:   runtimeConfig.ReadTimeout,
			watchLookback: runtimeConfig.Watch.LookbackBlocks,
			coordinator:   coordinator,
			handoff:       preparedNetwork.handoff,
			codec:         codec,
			allowedTokens: make(map[common.Address]struct{}, len(network.Tokens)),
		}
		for _, token := range network.Tokens {
			networkState.allowedTokens[token.Address] = struct{}{}
		}
		if runtimeConfig.Mode.IsDryRun() {
			networkState.dryAuthorizer, networkState.dryTransactioner, networkState.dryBroadcaster, networkState.dryAttempts = dryrun.NewGuards(runtimeConfig.SourceAddress, runtimeConfig.SponsorAddress)
		}
		process.networks = append(process.networks, networkState)
	}
	storesOpen = false
	return process, nil
}

func (dependencies daemonDependencies) withDefaults(mode config.Mode) (daemonDependencies, error) {
	if dependencies.serviceClock == nil || dependencies.observer == nil || dependencies.dial == nil || dependencies.loadManifest == nil || dependencies.openStore == nil || dependencies.openQuorum == nil {
		return daemonDependencies{}, errors.New("не заданы обязательные зависимости процесса")
	}
	if mode.IsLive() && (dependencies.dialSubmission == nil || dependencies.attestNetwork == nil || dependencies.newSigners == nil) {
		return daemonDependencies{}, errors.New("не заданы обязательные live-зависимости процесса")
	}
	if dependencies.newWatcher == nil {
		dependencies.newWatcher = func(dependencies watcher.Dependencies) (generationRunner, error) {
			return watcher.NewService(dependencies)
		}
	}
	return dependencies, nil
}

func (process *daemon) Run(ctx context.Context) error {
	if ctx == nil {
		_ = process.closeStores()
		return processError("daemon.run", domain.ErrorConfiguration, errorStartupInvalid, false, nil)
	}

	var workers sync.WaitGroup
	workers.Add(len(process.networks))
	for _, network := range process.networks {
		go func() {
			defer workers.Done()
			network.supervise(ctx, process.dependencies)
		}()
	}
	workers.Wait()
	if err := process.closeStores(); err != nil {
		return processError("daemon.store.close", domain.ErrorInternal, errorStoreFailed, false, err)
	}
	return nil
}

func (process *daemon) closeStores() error {
	var firstError error
	for _, network := range process.networks {
		if err := network.handoff.Close(); err != nil && firstError == nil {
			firstError = err
		}
	}
	return firstError
}

func (network *networkProcess) supervise(ctx context.Context, dependencies daemonDependencies) {
	for generation := uint64(1); ; generation++ {
		if ctx.Err() != nil {
			return
		}

		err := network.runGeneration(ctx, generation, dependencies)
		if ctx.Err() != nil {
			return
		}
		dependencies.observer.Record(observability.Event{
			Level:       observability.LevelWarning,
			Code:        eventGenerationError,
			NetworkName: network.network.Name,
			ErrorCode:   publicErrorCode(err),
		})
		if err := dependencies.serviceClock.Sleep(ctx, reconnectDelay); err != nil {
			return
		}
	}
}

func (network *networkProcess) runGeneration(ctx context.Context, generation uint64, dependencies daemonDependencies) error {
	generationContext, cancelGeneration := context.WithCancel(ctx)
	defer cancelGeneration()

	client, err := network.dialClient(generationContext, generation, dependencies)
	if err != nil {
		return err
	}
	defer client.Close()
	if client.Generation() != generation {
		return processError("daemon.client_generation", domain.ErrorInternal, errorClientInvalid, false, nil)
	}
	reader, err := rpc.NewDeadlineReader(client.Reader(), network.readTimeout)
	if err != nil {
		return processError("daemon.reader", domain.ErrorInternal, errorClientInvalid, false, err)
	}
	logSubscriber, err := rpc.NewDeadlineLogSubscriber(client.LogSubscriber(), network.readTimeout)
	if err != nil {
		return processError("daemon.logs", domain.ErrorInternal, errorClientInvalid, false, err)
	}
	headSubscriber, err := rpc.NewDeadlineHeadSubscriber(client.HeadSubscriber(), network.readTimeout)
	if err != nil {
		return processError("daemon.heads", domain.ErrorInternal, errorClientInvalid, false, err)
	}
	boundedClient := &generationClientView{
		generationClient: client,
		reader:           reader,
		logs:             logSubscriber,
		heads:            headSubscriber,
	}
	quorum, err := dependencies.openQuorum(generationContext, network.configured, network.readTimeout)
	if err != nil || quorum == nil {
		return processError("daemon.quorum", domain.ErrorRPCTransient, errorQuorumFailed, true, err)
	}
	defer quorum.Close()

	session, err := network.openSession(generationContext, boundedClient, dependencies)
	if err != nil {
		return processError("daemon.session", domain.ErrorRPCTransient, errorSessionFailed, true, err)
	}
	defer session.Close()
	watcherService, err := dependencies.newWatcher(watcher.Dependencies{
		Contracts:      reader,
		Finalized:      quorum,
		LogSubscriber:  logSubscriber,
		HeadSubscriber: headSubscriber,
		Codec:          network.codec,
		Clock:          dependencies.serviceClock,
		Observer:       dependencies.observer,
		Store:          network.handoff,
		Source:         network.source,
		Network:        network.network,
		Generation:     generation,
		LookbackBlocks: network.watchLookback,
		ReadTimeout:    network.readTimeout,
	})
	if err != nil {
		return processError("daemon.watcher", domain.ErrorInternal, errorWatcherFailed, false, err)
	}

	results := make(chan error, 2)
	go func() {
		results <- watcherService.Run(generationContext)
	}()
	go func() {
		results <- consumeCandidates(generationContext, network.network.ChainID, network.handoff, network.handoff, session, dependencies.serviceClock)
	}()

	first := <-results
	cancelGeneration()
	second := <-results
	return generationError(first, second)
}

func (network *networkProcess) openSession(ctx context.Context, client generationClient, dependencies daemonDependencies) (candidateSession, error) {
	if network.mode.IsDryRun() {
		session, err := dryrun.NewSession(ctx, client.Generation(), client.Reader(), dryrun.Config{
			Network:     network.network.ChainID,
			Source:      network.source,
			Sponsor:     network.sponsor,
			Destination: network.destination,
			Rescuer:     network.network.Rescuer,
			ReadTimeout: network.readTimeout,
		})
		if err != nil {
			return nil, err
		}
		return network.bindTokenPolicy(session), nil
	}
	if dependencies.newSession != nil {
		session, err := dependencies.newSession(ctx, network, client)
		if err != nil {
			return nil, err
		}
		if session == nil {
			return nil, errors.New("session factory вернула пустой результат")
		}
		return network.bindTokenPolicy(session), nil
	}

	dialContext, cancel := context.WithTimeout(ctx, network.readTimeout)
	submission, err := dependencies.dialSubmission(dialContext, network.configured.BroadcastHTTP)
	cancel()
	if err != nil || submission == nil {
		if submission != nil {
			submission.Close()
		}
		return nil, err
	}
	session, err := network.coordinator.NewSession(ctx, client.Generation(), client.Reader(), submission)
	if err != nil {
		submission.Close()
		return nil, err
	}
	return network.bindTokenPolicy(&liveCandidateSession{Session: session, submission: submission}), nil
}

func (network *networkProcess) bindTokenPolicy(session candidateSession) candidateSession {
	return &tokenPolicySession{
		candidateSession: session,
		allowed:          network.allowedTokens,
		allowUnknown:     network.network.AllowUnknownTokens,
	}
}

func (network *networkProcess) dialClient(ctx context.Context, generation uint64, dependencies daemonDependencies) (generationClient, error) {
	endpoints := make([]string, 0, len(network.configured.ReadProviders)*2)
	for _, provider := range network.configured.ReadProviders {
		endpoints = append(endpoints, provider.WSURL)
	}
	webSocketCount := len(endpoints)
	for _, provider := range network.configured.ReadProviders {
		endpoints = append(endpoints, provider.HTTPURL)
	}
	if len(endpoints) == 0 {
		return nil, processError("daemon.rpc_dial", domain.ErrorConfiguration, errorDialFailed, false, nil)
	}
	start := int((generation - 1) % uint64(len(endpoints)))
	var lastError error
	httpFallbackRecorded := false
	for attempt := range endpoints {
		index := (start + attempt) % len(endpoints)
		endpoint := endpoints[index]
		if index >= webSocketCount && !httpFallbackRecorded {
			dependencies.observer.Record(observability.Event{
				Level:       observability.LevelWarning,
				Code:        eventHTTPFallback,
				NetworkName: network.network.Name,
				ErrorCode:   publicErrorCode(lastError),
			})
			httpFallbackRecorded = true
		}
		dialContext, cancelDial := context.WithTimeout(ctx, network.readTimeout)
		client, err := dependencies.dial(dialContext, endpoint, generation)
		cancelDial()
		if err == nil && client != nil {
			return client, nil
		}
		if client != nil {
			client.Close()
		}
		lastError = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, processError("daemon.rpc_dial", domain.ErrorRPCTransient, errorDialFailed, true, lastError)
}

func consumeCandidates(ctx context.Context, network domain.NetworkID, queue store.CandidateQueue, incidents store.IncidentStore, session candidateHandler, serviceClock clock.Clock) error {
	for {
		candidate, err := queue.Next(ctx, network)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return processError("daemon.queue_next", domain.ErrorInternal, errorQueueNextFailed, true, err)
		}

		incidentID := domain.NewIncidentID(candidate.ID)
		if err := incidents.PutIncident(ctx, store.Incident{
			ID:        incidentID,
			Candidate: candidate.ID,
			Network:   candidate.Network,
			CreatedAt: serviceClock.Now(),
		}); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return processError("daemon.incident_put", domain.ErrorInternal, errorIncidentPutFailed, true, err)
		}

		handlingCandidate := candidate
		if handlingCandidate.Generation != 0 && handlingCandidate.Generation != session.Generation() {
			// The stable queued ID remains unchanged while replay is bound to the
			// current immutable RPC generation.
			handlingCandidate.Generation = session.Generation()
		}
		handleError := session.Handle(ctx, handlingCandidate)
		if err := ctx.Err(); err != nil {
			return err
		}
		if shouldReplay(handleError) {
			retryAt := serviceClock.Now().Add(candidateRetryDelay)
			if err := queue.Nack(ctx, candidate.ID, incidentID, retryAt); err != nil {
				return processError("daemon.candidate_nack", domain.ErrorInternal, errorCandidateNackFailed, true, err)
			}
			continue
		}
		if err := queue.Ack(ctx, candidate.ID, incidentID); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return processError("daemon.candidate_ack", domain.ErrorInternal, errorCandidateAckFailed, true, err)
		}
	}
}

func shouldReplay(err error) bool {
	if err == nil {
		return false
	}
	var classified *domain.ClassifiedError
	if !errors.As(err, &classified) {
		return true
	}
	return classified.Retryable || classified.Ambiguous
}

func generationError(first, second error) error {
	for _, err := range []error{first, second} {
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	for _, err := range []error{first, second} {
		if err != nil {
			return err
		}
	}
	return processError("daemon.worker", domain.ErrorInternal, errorWorkerStopped, true, nil)
}

func publicErrorCode(err error) domain.ErrorCode {
	var classified *domain.ClassifiedError
	if errors.As(err, &classified) && classified.Code != "" {
		return classified.Code
	}
	return errorWorkerStopped
}

func processError(operation string, class domain.ErrorClass, code domain.ErrorCode, retryable bool, cause error) error {
	return domain.NewError(operation, class, code, retryable, false, cause)
}
