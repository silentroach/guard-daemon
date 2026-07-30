package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"guard-daemon/internal/clock"
	"guard-daemon/internal/config"
	"guard-daemon/internal/contracts"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"
	"guard-daemon/internal/rescue"
	"guard-daemon/internal/rpc"
	"guard-daemon/internal/store"
	"guard-daemon/internal/watcher"

	"github.com/ethereum/go-ethereum/common"
)

const (
	reconnectDelay      = 5 * time.Second
	candidateRetryDelay = 5 * time.Second
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
	Broadcaster() rpc.Broadcaster
	Close()
}

type generationRunner interface {
	Run(context.Context) error
}

type candidateHandler interface {
	Generation() uint64
	Handle(context.Context, domain.RescueCandidate) error
}

type daemonDependencies struct {
	serviceClock clock.Clock
	observer     observability.Observer
	dial         func(context.Context, string, uint64) (generationClient, error)
	newSession   func(context.Context, *rescue.Coordinator, generationClient) (candidateHandler, error)
	newWatcher   func(watcher.Dependencies) (generationRunner, error)
}

type daemon struct {
	dependencies daemonDependencies
	networks     []*networkProcess
}

type networkProcess struct {
	network     domain.Network
	source      common.Address
	coordinator *rescue.Coordinator
	handoff     *legacyMemoryHandoff
	codec       *contracts.ERC20Codec
}

func newProductionDependencies(observer observability.Observer) daemonDependencies {
	dialer := rpc.EthClientDialer{}
	return daemonDependencies{
		serviceClock: clock.Real{},
		observer:     observer,
		dial: func(ctx context.Context, endpoint string, generation uint64) (generationClient, error) {
			return dialer.DialContext(ctx, endpoint, generation)
		},
	}
}

func newDaemon(runtimeConfig config.Runtime, dependencies daemonDependencies) (*daemon, error) {
	dependencies, err := dependencies.withDefaults()
	if err != nil {
		return nil, processError("daemon.dependencies", domain.ErrorConfiguration, errorStartupInvalid, false, err)
	}

	authorizer, err := rescue.NewPrivateKeyAuthorizationSigner(runtimeConfig.SourcePrivateKey)
	if err != nil {
		return nil, processError("daemon.source_signer", domain.ErrorConfiguration, errorStartupInvalid, false, err)
	}
	transactioner, err := rescue.NewPrivateKeyTransactionSigner(runtimeConfig.SponsorPrivateKey)
	if err != nil {
		return nil, processError("daemon.sponsor_signer", domain.ErrorConfiguration, errorStartupInvalid, false, err)
	}
	if authorizer.Address() != runtimeConfig.SourceAddress || transactioner.Address() != runtimeConfig.SponsorAddress {
		return nil, processError("daemon.signer_address", domain.ErrorConfiguration, errorStartupInvalid, false, nil)
	}

	codec, err := contracts.NewERC20Codec()
	if err != nil {
		return nil, processError("daemon.erc20_codec", domain.ErrorInternal, errorStartupInvalid, false, err)
	}

	process := &daemon{dependencies: dependencies, networks: make([]*networkProcess, 0, len(runtimeConfig.Networks))}
	for _, configuredNetwork := range runtimeConfig.Networks {
		network := configuredNetwork
		network.Tokens = append([]domain.Token(nil), configuredNetwork.Tokens...)
		coordinator, err := rescue.NewCoordinator(rescue.Config{
			Network:     network,
			Source:      runtimeConfig.SourceAddress,
			Sponsor:     runtimeConfig.SponsorAddress,
			Destination: runtimeConfig.Destination,
		}, authorizer, transactioner, dependencies.serviceClock, dependencies.observer)
		if err != nil {
			return nil, processError("daemon.coordinator", domain.ErrorConfiguration, errorStartupInvalid, false, err)
		}

		process.networks = append(process.networks, &networkProcess{
			network:     network,
			source:      runtimeConfig.SourceAddress,
			coordinator: coordinator,
			handoff:     newLegacyMemoryHandoff(network.ChainID, dependencies.serviceClock),
			codec:       codec,
		})
	}
	return process, nil
}

func (dependencies daemonDependencies) withDefaults() (daemonDependencies, error) {
	if dependencies.serviceClock == nil || dependencies.observer == nil || dependencies.dial == nil {
		return daemonDependencies{}, errors.New("не заданы обязательные зависимости процесса")
	}
	if dependencies.newSession == nil {
		dependencies.newSession = func(ctx context.Context, coordinator *rescue.Coordinator, client generationClient) (candidateHandler, error) {
			return coordinator.NewSession(ctx, client.Generation(), client.Reader(), client.Broadcaster())
		}
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
	return nil
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

	session, err := dependencies.newSession(generationContext, network.coordinator, client)
	if err != nil {
		return processError("daemon.session", domain.ErrorRPCTransient, errorSessionFailed, true, err)
	}
	watcherService, err := dependencies.newWatcher(watcher.Dependencies{
		Contracts:      client.Reader(),
		Logs:           client.Reader(),
		Blocks:         client.Reader(),
		LogSubscriber:  client.LogSubscriber(),
		HeadSubscriber: client.HeadSubscriber(),
		Codec:          network.codec,
		Clock:          dependencies.serviceClock,
		Observer:       dependencies.observer,
		Queue:          network.handoff,
		Source:         network.source,
		Network:        network.network,
		Generation:     generation,
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

func (network *networkProcess) dialClient(ctx context.Context, generation uint64, dependencies daemonDependencies) (generationClient, error) {
	client, err := dependencies.dial(ctx, network.network.WSURL, generation)
	if err == nil && client != nil {
		return client, nil
	}
	if client != nil {
		client.Close()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	dependencies.observer.Record(observability.Event{
		Level:       observability.LevelWarning,
		Code:        eventHTTPFallback,
		NetworkName: network.network.Name,
		ErrorCode:   publicErrorCode(err),
	})
	client, err = dependencies.dial(ctx, network.network.HTTPURL, generation)
	if err != nil || client == nil {
		if client != nil {
			client.Close()
		}
		return nil, processError("daemon.rpc_dial", domain.ErrorRPCTransient, errorDialFailed, true, err)
	}
	return client, nil
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
