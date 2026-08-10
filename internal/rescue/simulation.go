package rescue

import (
	"context"
	"errors"
	"math/big"

	"guard-daemon/internal/rpc"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

var (
	ErrInvalidSimulationRequest = errors.New("rescue simulation: invalid request")
	ErrEstimateGasFailed        = errors.New("rescue simulation: EstimateGas failed")
	ErrInvalidGasEstimate       = errors.New("rescue simulation: gas estimate is zero or excessive")
	ErrQuorumSimulationFailed   = errors.New("rescue simulation: quorum simulation failed")
)

type GasEstimator interface {
	EstimateGas(context.Context, ethereum.CallMsg) (uint64, error)
}

type PinnedCallSimulator interface {
	CallContract(context.Context, rpc.BlockRef, ethereum.CallMsg) ([]byte, error)
}

type EIP7702SimulationRequest struct {
	Sponsor           common.Address
	Source            common.Address
	GasLimit          uint64
	GasTipCap         *big.Int
	GasFeeCap         *big.Int
	Data              []byte
	AuthorizationList []types.SetCodeAuthorization
}

// EstimateEIP7702Gas выполняет ровно один вызов оценки без изменения состояния.
// Кортежи авторизации передаются заранее, поэтому функция не зависит от подписанта.
func EstimateEIP7702Gas(ctx context.Context, estimator GasEstimator, request EIP7702SimulationRequest) (uint64, error) {
	if ctx == nil || estimator == nil || !validSimulationRequest(request) {
		return 0, ErrInvalidSimulationRequest
	}
	estimate, err := estimator.EstimateGas(ctx, simulationCall(request))
	if err != nil {
		return 0, errors.Join(ErrEstimateGasFailed, err)
	}
	if estimate == 0 || estimate > request.GasLimit {
		return 0, ErrInvalidGasEstimate
	}
	return estimate, nil
}

// SimulateEIP7702At повторяет вызов с теми же параметрами через кворум чтения,
// привязанный к хешу блока. Предварительная оценка проверяет только требуемый объём gas.
func SimulateEIP7702At(ctx context.Context, simulator PinnedCallSimulator, block rpc.BlockRef, request EIP7702SimulationRequest) error {
	if ctx == nil || simulator == nil || block.Hash == (common.Hash{}) || !validSimulationRequest(request) {
		return ErrInvalidSimulationRequest
	}
	if _, err := simulator.CallContract(ctx, block, simulationCall(request)); err != nil {
		return errors.Join(ErrQuorumSimulationFailed, err)
	}
	return nil
}

func validSimulationRequest(request EIP7702SimulationRequest) bool {
	return request.Sponsor != (common.Address{}) && request.Source != (common.Address{}) && request.Sponsor != request.Source &&
		request.GasLimit > 0 && len(request.Data) > 0 && len(request.AuthorizationList) > 0 &&
		validSimulationFee(request.GasTipCap) && validSimulationFee(request.GasFeeCap) && request.GasFeeCap.Cmp(request.GasTipCap) >= 0
}

func simulationCall(request EIP7702SimulationRequest) ethereum.CallMsg {
	source := request.Source
	return ethereum.CallMsg{
		From:              request.Sponsor,
		To:                &source,
		Gas:               request.GasLimit,
		GasFeeCap:         copyBig(request.GasFeeCap),
		GasTipCap:         copyBig(request.GasTipCap),
		Data:              append([]byte(nil), request.Data...),
		AuthorizationList: append([]types.SetCodeAuthorization(nil), request.AuthorizationList...),
	}
}

func validSimulationFee(value *big.Int) bool {
	return value != nil && value.Sign() > 0 && value.BitLen() <= 256
}
