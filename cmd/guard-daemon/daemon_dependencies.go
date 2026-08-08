package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"guard-daemon/internal/budget"
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
	"github.com/holiman/uint256"
)

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
				Sponsor:             runtimeConfig.SponsorAddress,
				Destination:         runtimeConfig.Destination,
				Rescuer:             network.Rescuer,
				PolicyFingerprint:   watcher.PolicyFingerprint(network, runtimeConfig.Watch.LookbackBlocks),
				MaxPending:          maxPendingCandidates,
				MaxDiscoveredTokens: maxDiscoveredTokens,
				Clock:               clock.Real{},
			})
		},
		openQuorum: openRuntimeQuorum,
		openBudget: func(runtimeConfig config.Runtime, prepared []preparedNetwork) (budget.Ledger, error) {
			policy, err := runtimeBudgetPolicy(runtimeConfig)
			if err != nil {
				return nil, err
			}
			path, err := store.CanonicalBudgetPath(runtimeConfig.SponsorAddress)
			if err != nil {
				return nil, err
			}
			return budget.Open(path, budget.OpenOptions{
				Policy: policy, PolicyFingerprint: budgetBindingFingerprint(runtimeConfig, prepared), Now: time.Now,
			})
		},
		openAdmission: func(runtimeConfig config.Runtime, admissionConfig rescue.AdmissionConfig) (*rescue.AdmissionController, error) {
			path, err := store.CanonicalAdmissionPath(runtimeConfig.SponsorAddress)
			if err != nil {
				return nil, err
			}
			return rescue.OpenAdmissionController(path, admissionConfig, clock.Real{})
		},
		alertStatePath: func(runtimeConfig config.Runtime) (string, error) {
			return store.CanonicalAlertPath(runtimeConfig.SponsorAddress)
		},
		lstatRestoreMarker: os.Lstat,
	}
	if mode.IsLive() {
		dependencies.dialSubmission = dialSubmissionClient
		dependencies.attestNetwork = attestConfiguredNetwork
		dependencies.newSigners = newPrivateKeySigners
		dependencies.acquireFence = store.AcquireProcessFence
		dependencies.acquireBudgetFence = store.AcquireBudgetFence
	}
	return dependencies
}

func runtimeBudgetPolicy(runtimeConfig config.Runtime) (budget.Policy, error) {
	global, err := budgetLimits(runtimeConfig.Policy.MaxTransactionCostWei, runtimeConfig.Policy.HourlyBudgetWei, runtimeConfig.Policy.DailyBudgetWei, runtimeConfig.Policy.CumulativeBudgetWei)
	if err != nil {
		return budget.Policy{}, err
	}
	policy := budget.Policy{Global: global, Networks: make([]budget.NetworkPolicy, 0, len(runtimeConfig.Networks))}
	for _, network := range runtimeConfig.Networks {
		economic := network.EconomicPolicy
		limits, err := budgetLimits(economic.MaxTransactionCostWei, economic.HourlyBudgetWei, economic.DailyBudgetWei, economic.CumulativeBudgetWei)
		if err != nil {
			return budget.Policy{}, err
		}
		overhead, overheadOverflow := uint256.FromBig(economic.TransactionOverheadWei)
		reserve, reserveOverflow := uint256.FromBig(economic.SponsorMinimumBalanceWei)
		if overheadOverflow || reserveOverflow || reserve.IsZero() {
			return budget.Policy{}, errors.New("некорректная budget policy сети")
		}
		policy.Networks = append(policy.Networks, budget.NetworkPolicy{
			Network: network.ChainID, Sponsor: runtimeConfig.SponsorAddress, Limits: limits,
			TransactionOverhead: *overhead, EmergencySponsorReserve: *reserve,
		})
	}
	return policy, nil
}

func budgetLimits(perTransaction, perHour, perDay, cumulative *big.Int) (budget.Limits, error) {
	values := []*big.Int{perTransaction, perHour, perDay, cumulative}
	converted := make([]*uint256.Int, len(values))
	for index, value := range values {
		if value == nil || value.Sign() <= 0 {
			return budget.Limits{}, errors.New("некорректные global budget limits")
		}
		var overflow bool
		converted[index], overflow = uint256.FromBig(value)
		if overflow {
			return budget.Limits{}, errors.New("budget limit не помещается в uint256")
		}
	}
	return budget.Limits{PerTransaction: *converted[0], PerHour: *converted[1], PerDay: *converted[2], Cumulative: *converted[3]}, nil
}

func budgetBindingFingerprint(runtimeConfig config.Runtime, prepared []preparedNetwork) [sha256.Size]byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte("guard-daemon/budget-binding/v1\x00"))
	_, _ = hash.Write(runtimeConfig.SourceAddress[:])
	_, _ = hash.Write(runtimeConfig.SponsorAddress[:])
	_, _ = hash.Write(runtimeConfig.Destination[:])
	for _, network := range prepared {
		var chain [8]byte
		binary.BigEndian.PutUint64(chain[:], uint64(network.configured.ChainID))
		_, _ = hash.Write(chain[:])
		_, _ = hash.Write(network.manifest.Address[:])
		if network.configured.AllowUnknownTokens {
			_, _ = hash.Write([]byte{1})
		} else {
			_, _ = hash.Write([]byte{0})
		}
		for _, token := range network.configured.TrustedTokens {
			_, _ = hash.Write(token[:])
		}
	}
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result
}

func rescuePolicy(network config.Network) (rescue.FeePolicy, uint256.Int, map[common.Address]rescue.TrustedTokenValuePolicy, error) {
	economic := network.EconomicPolicy
	values := []*big.Int{
		economic.MaxFeePerGasWei,
		economic.MaxPriorityFeePerGasWei,
		economic.TransactionOverheadWei,
		economic.UnknownTokenMaxTransactionCostWei,
		economic.NativeMinimumNetValueWei,
		economic.MaxTransactionCostWei,
	}
	converted := make([]*uint256.Int, len(values))
	for index, value := range values {
		if value == nil || value.Sign() < 0 {
			return rescue.FeePolicy{}, uint256.Int{}, nil, errors.New("некорректная rescue policy сети")
		}
		var overflow bool
		converted[index], overflow = uint256.FromBig(value)
		if overflow {
			return rescue.FeePolicy{}, uint256.Int{}, nil, errors.New("rescue policy не помещается в uint256")
		}
	}
	policy := rescue.FeePolicy{
		Network:                 network.ChainID,
		MaxFeePerGas:            *converted[0],
		MaxPriorityFeePerGas:    *converted[1],
		TokenGasLimit:           economic.TokenGasLimit,
		NativeGasLimit:          economic.NativeGasLimit,
		Overhead:                *converted[2],
		UnknownTokenCostCap:     *converted[3],
		TransactionCostCap:      *converted[5],
		UnboundedAdditionalFees: economic.UnboundedAdditionalFees,
	}
	tokenValues := make(map[common.Address]rescue.TrustedTokenValuePolicy, len(economic.TokenValueRules))
	for _, rule := range economic.TokenValueRules {
		minimum, minimumOverflow := uint256.FromBig(rule.MinimumBalance)
		maximum, maximumOverflow := uint256.FromBig(rule.MaxTransactionCostWei)
		if minimumOverflow || maximumOverflow || minimum.IsZero() || maximum.IsZero() {
			return rescue.FeePolicy{}, uint256.Int{}, nil, errors.New("некорректное правило ценности trusted token")
		}
		tokenValues[rule.Address] = rescue.TrustedTokenValuePolicy{MinimumBalance: *minimum, MaximumCost: *maximum}
	}
	return policy, *converted[4], tokenValues, nil
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

	return contracts.LoadTrustedManifest(manifestFile, artifactFile, configuredManifestExpectations(runtimeConfig, network))
}

func configuredManifestExpectations(runtimeConfig config.Runtime, network config.Network) contracts.ManifestExpectations {
	return contracts.ManifestExpectations{
		ChainID:        strconv.FormatInt(int64(network.ChainID), 10),
		ContractRole:   "rescuer",
		Destination:    runtimeConfig.Destination,
		Sponsor:        runtimeConfig.SponsorAddress,
		ArtifactSHA256: runtimeConfig.Artifact.SHA256,
		SourceProvenance: contracts.SourceProvenance{
			Kind:  network.ManifestSourceKind,
			Value: network.ManifestSourceValue,
		},
		CompilerVersion: runtimeConfig.Artifact.CompilerVersion,
	}
}

func attestConfiguredNetwork(ctx context.Context, network config.Network, manifest contracts.DeploymentManifest, readTimeout time.Duration) error {
	return attestConfiguredNetworkWith(
		ctx,
		network,
		manifest,
		readTimeout,
		func(dialContext context.Context, endpoint string) (attestationClient, error) {
			return rpc.DialEthClient(dialContext, endpoint)
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
	client, err := rpc.DialEthClient(ctx, endpoint)
	if err != nil {
		return nil, domain.NewError("daemon.broadcast_dial", domain.ErrorRPCTransient, errorDialFailed, true, false, err)
	}
	return client, nil
}

func newProcessLeaseOwner() (string, error) {
	var opaque [32]byte
	if _, err := rand.Read(opaque[:]); err != nil {
		return "", errors.New("не удалось создать идентификатор владельца lease")
	}
	return hex.EncodeToString(opaque[:]), nil
}

func (dependencies daemonDependencies) withDefaults(mode config.Mode) (daemonDependencies, error) {
	if dependencies.lstatRestoreMarker == nil {
		dependencies.lstatRestoreMarker = os.Lstat
	}
	if dependencies.serviceClock == nil || dependencies.observer == nil || dependencies.dial == nil || dependencies.loadManifest == nil || dependencies.openStore == nil || dependencies.openQuorum == nil || dependencies.alertStatePath == nil {
		return daemonDependencies{}, errors.New("не заданы обязательные зависимости процесса")
	}
	if mode.IsLive() && (dependencies.dialSubmission == nil || dependencies.attestNetwork == nil || dependencies.newSigners == nil || dependencies.acquireFence == nil || dependencies.acquireBudgetFence == nil || dependencies.openBudget == nil || dependencies.openAdmission == nil) {
		return daemonDependencies{}, errors.New("не заданы обязательные live-зависимости процесса")
	}
	if dependencies.newWatcher == nil {
		dependencies.newWatcher = func(dependencies watcher.Dependencies) (generationRunner, error) {
			return watcher.NewService(dependencies)
		}
	}
	return dependencies, nil
}
