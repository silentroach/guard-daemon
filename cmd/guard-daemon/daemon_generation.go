package main

import (
	"context"
	"errors"

	"guard-daemon/internal/config"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"
	"guard-daemon/internal/rescue/dryrun"
	"guard-daemon/internal/rpc"
	"guard-daemon/internal/store"
	"guard-daemon/internal/watcher"
)

func (network *networkProcess) supervise(ctx context.Context, dependencies daemonDependencies) error {
	for generation := uint64(1); ; generation++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := network.runGeneration(ctx, generation, dependencies)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		dependencies.observer.Record(observability.Event{
			Level:       observability.LevelWarning,
			Code:        eventGenerationError,
			ChainID:     network.network.ChainID,
			NetworkName: network.network.Name,
			ErrorCode:   publicErrorCode(err),
		})
		if network.metrics != nil {
			_ = network.metrics.RecordReconnect(network.network.ChainID)
			_ = network.metrics.RecordRPCError(network.network.ChainID)
		}
		if network.health != nil {
			_ = network.health.SetCondition(network.network.ChainID, observability.ConditionRPCDegraded, true)
		}
		if network.alerts != nil {
			if _, alertErr := network.alerts.Raise(network.network.ChainID, observability.AlertRPCDegraded); alertErr != nil {
				return alertErr
			}
		}
		if err := dependencies.serviceClock.Sleep(ctx, reconnectDelay); err != nil {
			return err
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

	session, err := network.openSession(generationContext, boundedClient, quorum, dependencies)
	if err != nil {
		return processError("daemon.session", domain.ErrorRPCTransient, errorSessionFailed, true, err)
	}
	defer session.Close()
	if err := updateQueueDepth(generationContext, network.network.ChainID, network.handoff, network.metrics); err != nil {
		return err
	}
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
		Ready: func() error {
			if network.alerts != nil {
				if _, alertErr := network.alerts.Resolve(network.network.ChainID, observability.AlertRPCDegraded); alertErr != nil {
					return processError("daemon.rpc_alert", domain.ErrorInternal, errorSessionFailed, true, alertErr)
				}
			}
			if network.health != nil {
				if healthErr := network.health.SetCondition(network.network.ChainID, observability.ConditionRPCDegraded, false); healthErr != nil {
					return processError("daemon.rpc_health", domain.ErrorInternal, errorSessionFailed, true, healthErr)
				}
			}
			return nil
		},
	})
	if err != nil {
		return processError("daemon.watcher", domain.ErrorInternal, errorWatcherFailed, false, err)
	}

	workerCount := 2
	reconciliation := reconciliationWorker(network.mode, session)
	if reconciliation != nil {
		workerCount++
	}
	results := make(chan error, workerCount)
	go func() {
		results <- watcherService.Run(generationContext)
	}()
	go func() {
		results <- consumeCandidates(generationContext, network.network.ChainID, network.handoff, network.handoff, session, dependencies.serviceClock, network.metrics)
	}()
	if reconciliation != nil {
		go func() {
			results <- reconciliation.RunReconciliation(generationContext)
		}()
	}

	first := <-results
	cancelGeneration()
	workerErrors := []error{first}
	for range workerCount - 1 {
		workerErrors = append(workerErrors, <-results)
	}
	return generationError(workerErrors...)
}

func reconciliationWorker(mode config.Mode, session candidateSession) reconciliationSession {
	if !mode.IsLive() {
		return nil
	}
	reconciliation, _ := session.(reconciliationSession)
	return reconciliation
}

func (network *networkProcess) openSession(ctx context.Context, client generationClient, quorum runtimeQuorum, dependencies daemonDependencies) (candidateSession, error) {
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
		if err := network.validateProcessFence(); err != nil {
			return nil, err
		}
		session, err := dependencies.newSession(ctx, network, client, quorum)
		if err != nil {
			return nil, err
		}
		if session == nil {
			return nil, errors.New("session factory вернула пустой результат")
		}
		return network.bindTokenPolicy(session), nil
	}
	if network.emergencyStopped {
		session, err := network.coordinator.NewSession(ctx, client.Generation(), client.Reader(), quorum, network.dryBroadcaster)
		if err != nil {
			return nil, err
		}
		return network.bindTokenPolicy(&guardedCandidateSession{Session: session}), nil
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
	boundedBroadcaster, err := rpc.NewDeadlineBroadcaster(submission, network.readTimeout)
	if err != nil {
		submission.Close()
		return nil, err
	}
	if err := network.validateProcessFence(); err != nil {
		submission.Close()
		return nil, err
	}
	session, err := network.coordinator.NewSession(ctx, client.Generation(), client.Reader(), quorum, boundedBroadcaster)
	if err != nil {
		submission.Close()
		return nil, err
	}
	return network.bindTokenPolicy(&liveCandidateSession{Session: session, submission: submission}), nil
}

func (network *networkProcess) validateProcessFence() error {
	if network.fence == nil {
		return processError("daemon.fence_validate", domain.ErrorInternal, errorFenceValidateFailed, false, store.ErrFenceLost)
	}
	if err := network.fence.Validate(); err != nil {
		return processError("daemon.fence_validate", domain.ErrorInternal, errorFenceValidateFailed, false, err)
	}
	return nil
}

func (network *networkProcess) bindTokenPolicy(session candidateSession) candidateSession {
	policySession := &tokenPolicySession{
		candidateSession: session,
		allowed:          network.allowedTokens,
		allowUnknown:     network.network.AllowUnknownTokens,
	}
	if reconciliation, ok := session.(reconciliationSession); ok {
		return &reconcilingTokenPolicySession{tokenPolicySession: policySession, reconciliationSession: reconciliation}
	}
	return policySession
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
				ChainID:     network.network.ChainID,
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
