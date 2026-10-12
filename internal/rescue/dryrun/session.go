package dryrun

import (
	"context"
	"math/big"
	"time"

	"guard-daemon/internal/contracts"
	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
)

const (
	codeInvalidConfig        domain.ErrorCode = "dry_run_invalid_config"
	codeChainIDReadFailed    domain.ErrorCode = "dry_run_chain_id_read_failed"
	codeChainIDMismatch      domain.ErrorCode = "dry_run_chain_id_mismatch"
	codeCandidateMismatch    domain.ErrorCode = "dry_run_candidate_mismatch"
	codeCandidateUnsupported domain.ErrorCode = "dry_run_candidate_unsupported"
	codePlanFailed           domain.ErrorCode = "dry_run_plan_failed"
	codeSimulationFailed     domain.ErrorCode = "dry_run_simulation_failed"
)

type Config struct {
	Network     domain.NetworkID
	Source      common.Address
	Sponsor     common.Address
	Destination common.Address
	Rescuer     common.Address
	ReadTimeout time.Duration
}

type Reader interface {
	ChainID(context.Context) (*big.Int, error)
	EstimateGas(context.Context, ethereum.CallMsg) (uint64, error)
}

type Session struct {
	generation uint64
	reader     Reader
	config     Config
	codec      *contracts.RescuerCodec
}

func NewSession(ctx context.Context, generation uint64, reader Reader, config Config) (*Session, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	if ctx == nil || reader == nil {
		return nil, newError("dry_run.session", domain.ErrorConfiguration, codeInvalidConfig, false)
	}

	codec, err := contracts.NewRescuerCodec()
	if err != nil {
		return nil, newError("dry_run.plan", domain.ErrorInternal, codePlanFailed, false)
	}
	readContext, cancel := context.WithTimeout(ctx, config.ReadTimeout)
	defer cancel()
	chainID, err := reader.ChainID(readContext)
	if err != nil {
		return nil, newError("dry_run.chain_id", domain.ErrorRPCTransient, codeChainIDReadFailed, true)
	}
	if chainID == nil || chainID.Cmp(big.NewInt(int64(config.Network))) != 0 {
		return nil, newError("dry_run.chain_id", domain.ErrorConfiguration, codeChainIDMismatch, false)
	}

	return &Session{
		generation: generation,
		reader:     reader,
		config:     config,
		codec:      codec,
	}, nil
}

func (session *Session) Generation() uint64 {
	return session.generation
}

func (session *Session) Handle(ctx context.Context, candidate domain.RescueCandidate) error {
	if ctx == nil {
		return newError("dry_run.candidate", domain.ErrorConfiguration, codeInvalidConfig, false)
	}
	if candidate.Network != session.config.Network || candidate.Source != session.config.Source || (candidate.Generation != 0 && candidate.Generation != session.generation) {
		return newError("dry_run.candidate", domain.ErrorConfiguration, codeCandidateMismatch, false)
	}

	var (
		data []byte
		err  error
	)
	switch candidate.Kind {
	case domain.CandidateToken:
		if candidate.Token.Address == (common.Address{}) {
			return newError("dry_run.plan", domain.ErrorConfiguration, codePlanFailed, false)
		}
		data, err = session.codec.PackSweepAll([]common.Address{candidate.Token.Address})
	case domain.CandidateNative, domain.CandidatePeriodic:
		data, err = session.codec.PackSweepEth()
	default:
		return newError("dry_run.candidate", domain.ErrorConfiguration, codeCandidateUnsupported, false)
	}
	if err != nil {
		return newError("dry_run.plan", domain.ErrorInternal, codePlanFailed, false)
	}

	target := session.config.Source
	readContext, cancel := context.WithTimeout(ctx, session.config.ReadTimeout)
	defer cancel()
	if _, err := session.reader.EstimateGas(readContext, ethereum.CallMsg{
		From: session.config.Sponsor,
		To:   &target,
		Data: data,
	}); err != nil {
		return newError("dry_run.simulation", domain.ErrorRPCTransient, codeSimulationFailed, true)
	}
	return nil
}

func (*Session) Close() {}

func validateConfig(config Config) error {
	zero := common.Address{}
	if config.Network <= 0 || config.Source == zero || config.Sponsor == zero || config.Destination == zero || config.Rescuer == zero || config.ReadTimeout <= 0 ||
		config.Source == config.Sponsor || config.Source == config.Destination || config.Sponsor == config.Destination {
		return newError("dry_run.config", domain.ErrorConfiguration, codeInvalidConfig, false)
	}
	return nil
}

func newError(operation string, class domain.ErrorClass, code domain.ErrorCode, retryable bool) *domain.ClassifiedError {
	return domain.NewError(operation, class, code, retryable, false, nil)
}
