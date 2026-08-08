package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"guard-daemon/internal/budget"
	"guard-daemon/internal/config"
	"guard-daemon/internal/contracts"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"
	"guard-daemon/internal/rescue"
	"guard-daemon/internal/rescue/dryrun"
	"guard-daemon/internal/rpc"
	"guard-daemon/internal/store"

	"github.com/ethereum/go-ethereum/common"
)

func newDaemon(ctx context.Context, runtimeConfig config.Runtime, dependencies daemonDependencies) (_ *daemon, resultErr error) {
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
	if err := rejectRestoredSigningState(runtimeConfig, dependencies.lstatRestoreMarker); err != nil {
		return nil, processError("daemon.restore_marker", domain.ErrorConfiguration, errorStartupInvalid, false, err)
	}
	if _, err := runtimeBudgetPolicy(runtimeConfig); err != nil {
		return nil, processError("daemon.budget_policy", domain.ErrorConfiguration, errorStartupInvalid, false, err)
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
	chainIDs := make([]domain.NetworkID, 0, len(prepared))
	for _, network := range prepared {
		chainIDs = append(chainIDs, network.configured.ChainID)
	}
	metrics, err := observability.NewMetrics(chainIDs)
	if err != nil {
		return nil, processError("daemon.metrics", domain.ErrorConfiguration, errorStartupInvalid, false, err)
	}
	health, err := observability.NewHealth(chainIDs)
	if err != nil {
		return nil, processError("daemon.health", domain.ErrorConfiguration, errorStartupInvalid, false, err)
	}
	alertPath, err := dependencies.alertStatePath(runtimeConfig)
	if err != nil {
		return nil, processError("daemon.alerts", domain.ErrorConfiguration, errorStartupInvalid, false, err)
	}
	alerts, err := observability.NewAlertManager(observability.AlertManagerConfig{
		Cooldown:  runtimeConfig.Policy.AlertCooldown,
		Capacity:  len(chainIDs) * 8,
		StatePath: alertPath,
	}, dependencies.serviceClock, structuredAlertSink{observer: dependencies.observer})
	if err != nil {
		return nil, processError("daemon.alerts", domain.ErrorConfiguration, errorStartupInvalid, false, err)
	}
	gate := observability.NewPaidActionGate()
	if runtimeConfig.Policy.EmergencyStop {
		gate.Stop()
		health.Stop()
		for _, chainID := range chainIDs {
			if _, alertErr := alerts.Raise(chainID, observability.AlertPaidActionsStopped); alertErr != nil {
				return nil, processError("daemon.alert", domain.ErrorInternal, errorStartupInvalid, false, alertErr)
			}
		}
	} else {
		for _, chainID := range chainIDs {
			if _, alertErr := alerts.Resolve(chainID, observability.AlertPaidActionsStopped); alertErr != nil {
				return nil, processError("daemon.alert", domain.ErrorInternal, errorStartupInvalid, false, alertErr)
			}
		}
	}

	if runtimeConfig.Mode.IsLive() {
		for _, network := range prepared {
			if err := dependencies.attestNetwork(ctx, network.configured, network.manifest, runtimeConfig.ReadTimeout); err != nil {
				return nil, processError("daemon.attestation", domain.ErrorConfiguration, errorStartupInvalid, false, err)
			}
		}
	}

	startupComplete := false
	var budgetLedger budget.Ledger
	var budgetFence store.ProcessFence
	var admission *rescue.AdmissionController
	defer func() {
		if startupComplete {
			return
		}
		var cleanupError error
		for _, network := range prepared {
			if network.handoff != nil && network.lease != (store.Lease{}) {
				cleanupContext, cancel := context.WithTimeout(context.Background(), leaseReleaseTimeout)
				if err := network.handoff.Release(cleanupContext, network.lease); err != nil {
					cleanupError = errors.Join(cleanupError, processError("daemon.lease_release", domain.ErrorInternal, errorLeaseReleaseFailed, false, err))
				}
				cancel()
			}
		}
		for _, network := range prepared {
			if network.fence != nil {
				if err := network.fence.Release(); err != nil {
					cleanupError = errors.Join(cleanupError, processError("daemon.fence_release", domain.ErrorInternal, errorFenceReleaseFailed, false, err))
				}
			}
		}
		for _, network := range prepared {
			if network.handoff != nil {
				if err := network.handoff.Close(); err != nil {
					cleanupError = errors.Join(cleanupError, processError("daemon.store.close", domain.ErrorInternal, errorStoreFailed, false, err))
				}
			}
		}
		if budgetLedger != nil {
			if err := budgetLedger.Close(); err != nil {
				cleanupError = errors.Join(cleanupError, processError("daemon.budget.close", domain.ErrorInternal, errorBudgetFailed, false, err))
			}
		}
		if admission != nil {
			if err := admission.Close(); err != nil {
				cleanupError = errors.Join(cleanupError, processError("daemon.admission.close", domain.ErrorInternal, errorBudgetFailed, false, err))
			}
		}
		if budgetFence != nil {
			if err := budgetFence.Release(); err != nil {
				cleanupError = errors.Join(cleanupError, processError("daemon.budget_fence_release", domain.ErrorInternal, errorFenceReleaseFailed, false, err))
			}
		}
		resultErr = errors.Join(resultErr, cleanupError)
	}()
	if runtimeConfig.Mode.IsLive() {
		budgetFence, err = dependencies.acquireBudgetFence(runtimeConfig.SponsorAddress)
		if err != nil || budgetFence == nil {
			return nil, processError("daemon.budget_fence_acquire", domain.ErrorInternal, errorFenceAcquireFailed, false, err)
		}
		for index := range prepared {
			network := prepared[index].configured.Domain(prepared[index].manifest.Address)
			key := store.LeaseKey{Network: network.ChainID, Sponsor: runtimeConfig.SponsorAddress}
			fence, acquireErr := dependencies.acquireFence(key)
			if fence != nil {
				prepared[index].fence = fence
			}
			if acquireErr != nil || fence == nil {
				return nil, processError("daemon.fence_acquire", domain.ErrorInternal, errorFenceAcquireFailed, false, acquireErr)
			}
		}
	}
	if runtimeConfig.Mode.IsLive() {
		admissionConfig := rescue.AdmissionConfig{
			Window: runtimeConfig.Policy.AbuseWindow, RateWindow: time.Minute, RateLimit: runtimeConfig.Policy.RateLimitPerMinute,
			MaxAttemptsPerToken:       runtimeConfig.Policy.MaxAttemptsPerTokenWindow,
			MaxAttemptsPerSourceEvent: runtimeConfig.Policy.MaxAttemptsPerSourceEvent,
			MaxNewUnknownTokens:       runtimeConfig.Policy.MaxNewUnknownTokensPerWindow,
			Capacity:                  int(maxPendingCandidates) * len(prepared),
		}
		admission, err = dependencies.openAdmission(runtimeConfig, admissionConfig)
		if err != nil || admission == nil {
			return nil, processError("daemon.admission", domain.ErrorInternal, errorBudgetFailed, false, err)
		}
	}
	for index := range prepared {
		network := prepared[index].configured.Domain(prepared[index].manifest.Address)
		handoff, openErr := dependencies.openStore(runtimeConfig, prepared[index].configured, network)
		if openErr != nil || handoff == nil {
			return nil, processError("daemon.store", domain.ErrorInternal, errorStoreFailed, false, openErr)
		}
		prepared[index].handoff = handoff
	}
	if runtimeConfig.Mode.IsLive() {
		openedBudget, openErr := dependencies.openBudget(runtimeConfig, prepared)
		if openErr != nil || openedBudget == nil {
			return nil, processError("daemon.budget", domain.ErrorInternal, errorBudgetFailed, false, openErr)
		}
		budgetLedger = openedBudget
		snapshot, snapshotErr := budgetLedger.Snapshot(ctx)
		if snapshotErr != nil {
			return nil, processError("daemon.budget_snapshot", domain.ErrorInternal, errorBudgetFailed, false, snapshotErr)
		}
		metrics.SetGlobalBudget(snapshot.Global.Spent.Cumulative, snapshot.Global.Reserved.Cumulative, snapshot.Global.Remaining.Cumulative)
		for chainID, network := range snapshot.Networks {
			if metricsErr := metrics.SetBudget(chainID, network.Spent.Cumulative, network.Reserved.Cumulative, network.Remaining.Cumulative); metricsErr != nil {
				return nil, processError("daemon.budget_metrics", domain.ErrorInternal, errorBudgetFailed, false, metricsErr)
			}
			blocked := snapshot.Global.Blocked() || network.Blocked()
			if healthErr := health.SetCondition(chainID, observability.ConditionBudgetBlocked, blocked); healthErr != nil {
				return nil, processError("daemon.budget_health", domain.ErrorInternal, errorBudgetFailed, false, healthErr)
			}
			if blocked {
				if _, alertErr := alerts.Raise(chainID, observability.AlertBudgetBlocked); alertErr != nil {
					return nil, processError("daemon.budget_alert", domain.ErrorInternal, errorBudgetFailed, false, alertErr)
				}
			} else if _, alertErr := alerts.Resolve(chainID, observability.AlertBudgetBlocked); alertErr != nil {
				return nil, processError("daemon.budget_alert", domain.ErrorInternal, errorBudgetFailed, false, alertErr)
			}
		}
	}
	if runtimeConfig.Mode.IsLive() {
		owner, ownerErr := newProcessLeaseOwner()
		if ownerErr != nil {
			return nil, processError("daemon.lease_owner", domain.ErrorInternal, errorLeaseAcquireFailed, false, ownerErr)
		}
		for index := range prepared {
			network := prepared[index].configured.Domain(prepared[index].manifest.Address)
			key := store.LeaseKey{
				Network: network.ChainID,
				Sponsor: runtimeConfig.SponsorAddress,
			}
			lease, acquireErr := prepared[index].handoff.Acquire(ctx, key, owner, processLeaseTTL)
			if lease != (store.Lease{}) {
				prepared[index].lease = lease
			}
			if acquireErr != nil || lease.Key != key || lease.Owner != owner || !lease.ExpiresAt.After(dependencies.serviceClock.Now()) {
				return nil, processError("daemon.lease_acquire", domain.ErrorInternal, errorLeaseAcquireFailed, false, acquireErr)
			}
		}
	}

	var authorizer rescue.AuthorizationSigner
	var transactioner rescue.TransactionSigner
	var stoppedBroadcaster rpc.Broadcaster
	if runtimeConfig.Mode.IsLive() && !runtimeConfig.Policy.EmergencyStop {
		authorizer, transactioner, err = dependencies.newSigners(runtimeConfig.LiveSecrets)
		if err != nil || authorizer == nil || transactioner == nil {
			return nil, processError("daemon.signers", domain.ErrorConfiguration, errorStartupInvalid, false, err)
		}
		if authorizer.Address() != runtimeConfig.SourceAddress || transactioner.Address() != runtimeConfig.SponsorAddress {
			return nil, processError("daemon.signer_address", domain.ErrorConfiguration, errorStartupInvalid, false, nil)
		}
	} else if runtimeConfig.Mode.IsLive() {
		authorizer, transactioner, stoppedBroadcaster, _ = dryrun.NewGuards(runtimeConfig.SourceAddress, runtimeConfig.SponsorAddress)
	}

	codec, err := contracts.NewERC20Codec()
	if err != nil {
		return nil, processError("daemon.erc20_codec", domain.ErrorInternal, errorStartupInvalid, false, err)
	}

	process := &daemon{
		dependencies: dependencies, networks: make([]*networkProcess, 0, len(prepared)),
		budget: budgetLedger, budgetFence: budgetFence, metrics: metrics, health: health, alerts: alerts, gate: gate,
		stateDirectory: runtimeConfig.Watch.StateDirectory, admission: admission,
	}
	for _, preparedNetwork := range prepared {
		network := preparedNetwork.configured.Domain(preparedNetwork.manifest.Address)
		var coordinator *rescue.Coordinator
		if runtimeConfig.Mode.IsLive() {
			feePolicy, nativeMinimum, tokenValues, policyErr := rescuePolicy(preparedNetwork.configured)
			if policyErr != nil {
				return nil, processError("daemon.rescue_policy", domain.ErrorConfiguration, errorStartupInvalid, false, policyErr)
			}
			coordinator, err = rescue.NewCoordinator(rescue.Config{
				Network:            network,
				Source:             runtimeConfig.SourceAddress,
				Sponsor:            runtimeConfig.SponsorAddress,
				Destination:        runtimeConfig.Destination,
				StartupTimeout:     runtimeConfig.ReadTimeout,
				FeeReadTimeout:     runtimeConfig.ReadTimeout,
				State:              preparedNetwork.handoff,
				LeaseManager:       preparedNetwork.handoff,
				Lease:              preparedNetwork.lease,
				ProcessFence:       preparedNetwork.fence,
				LeaseTTL:           processLeaseTTL,
				MaxAttempts:        rescueMaxAttempts,
				RetryDelay:         candidateRetryDelay,
				ReceiptTimeout:     rescueReceiptTimeout,
				Budget:             budgetLedger,
				Gate:               gate,
				Admission:          admission,
				FeePolicy:          feePolicy,
				NativeMinimum:      nativeMinimum,
				TrustedTokens:      append([]common.Address(nil), preparedNetwork.configured.TrustedTokens...),
				TrustedTokenValues: tokenValues,
				Metrics:            metrics,
				Alerts:             alerts,
				Health:             health,
			}, authorizer, transactioner, dependencies.serviceClock, dependencies.observer)
			if err != nil {
				return nil, processError("daemon.coordinator", domain.ErrorConfiguration, errorStartupInvalid, false, err)
			}
		}

		networkState := &networkProcess{
			configured:       preparedNetwork.configured,
			network:          network,
			mode:             runtimeConfig.Mode,
			source:           runtimeConfig.SourceAddress,
			sponsor:          runtimeConfig.SponsorAddress,
			destination:      runtimeConfig.Destination,
			readTimeout:      runtimeConfig.ReadTimeout,
			watchLookback:    runtimeConfig.Watch.LookbackBlocks,
			coordinator:      coordinator,
			handoff:          preparedNetwork.handoff,
			fence:            preparedNetwork.fence,
			codec:            codec,
			allowedTokens:    make(map[common.Address]struct{}, len(network.Tokens)),
			emergencyStopped: runtimeConfig.Policy.EmergencyStop,
			dryBroadcaster:   stoppedBroadcaster,
			metrics:          metrics,
			health:           health,
			alerts:           alerts,
		}
		for _, token := range network.Tokens {
			networkState.allowedTokens[token.Address] = struct{}{}
		}
		if network.AllowUnknownTokens {
			dependencies.observer.Record(observability.Event{
				Level: observability.LevelWarning, Code: eventUnknownTokenOptIn, ChainID: network.ChainID, NetworkName: network.Name,
			})
		}
		if runtimeConfig.Mode.IsDryRun() {
			networkState.dryAuthorizer, networkState.dryTransactioner, networkState.dryBroadcaster, networkState.dryAttempts = dryrun.NewGuards(runtimeConfig.SourceAddress, runtimeConfig.SponsorAddress)
		}
		process.networks = append(process.networks, networkState)
	}
	startupComplete = true
	return process, nil
}

func rejectRestoredSigningState(runtimeConfig config.Runtime, lstat func(string) (os.FileInfo, error)) error {
	if !runtimeConfig.Mode.IsLive() || runtimeConfig.Policy.EmergencyStop {
		return nil
	}
	_, err := lstat(restoreMarkerPath)
	if err == nil {
		return errors.New("состояние было восстановлено из резервной копии")
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return errors.New("не удалось безопасно проверить marker восстановленного состояния")
}
