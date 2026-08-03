package rescue

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
)

func TestEstimateEIP7702GasUsesExactCallShapeAndContext(t *testing.T) {
	type contextKey string
	ctx := context.WithValue(context.Background(), contextKey("request"), "simulation")
	sponsor := common.HexToAddress("0x1001")
	source := common.HexToAddress("0x2002")
	authorizations := []types.SetCodeAuthorization{{
		ChainID: *uint256.NewInt(8453), Address: common.HexToAddress("0x3003"), Nonce: 7,
	}}
	request := EIP7702SimulationRequest{
		Sponsor: sponsor, Source: source, GasLimit: 220_000,
		GasTipCap: big.NewInt(10), GasFeeCap: big.NewInt(30), Data: []byte{0xaa, 0xbb},
		AuthorizationList: authorizations,
	}
	estimator := &capturingGasEstimator{estimate: 180_000, wantContext: ctx}

	estimate, err := EstimateEIP7702Gas(ctx, estimator, request)
	if err != nil || estimate != 180_000 {
		t.Fatalf("EstimateEIP7702Gas() = %d, %v", estimate, err)
	}
	want := ethereum.CallMsg{
		From: sponsor, To: &source, Gas: 220_000, GasTipCap: big.NewInt(10), GasFeeCap: big.NewInt(30),
		Data: []byte{0xaa, 0xbb}, AuthorizationList: []types.SetCodeAuthorization{{
			ChainID: *uint256.NewInt(8453), Address: common.HexToAddress("0x3003"), Nonce: 7,
		}},
	}
	if estimator.calls != 1 || !reflect.DeepEqual(estimator.call, want) {
		t.Fatalf("EstimateGas calls = %d, call = %#v, want %#v", estimator.calls, estimator.call, want)
	}

	request.Data[0] = 0
	request.GasTipCap.SetInt64(99)
	request.AuthorizationList[0].Nonce = 99
	if !reflect.DeepEqual(estimator.call, want) {
		t.Fatalf("captured CallMsg changed with request mutation: %#v", estimator.call)
	}
}

func TestEstimateEIP7702GasPropagatesEstimateFailure(t *testing.T) {
	cause := errors.New("rpc unavailable")
	estimator := &capturingGasEstimator{err: cause}
	_, err := EstimateEIP7702Gas(context.Background(), estimator, validSimulationRequestForTest())
	if !errors.Is(err, ErrEstimateGasFailed) || !errors.Is(err, cause) || estimator.calls != 1 {
		t.Fatalf("EstimateEIP7702Gas() error = %v, calls = %d", err, estimator.calls)
	}
}

func TestEstimateEIP7702GasRejectsZeroAndExcessiveEstimate(t *testing.T) {
	for _, estimate := range []uint64{0, 100_001} {
		t.Run(new(big.Int).SetUint64(estimate).String(), func(t *testing.T) {
			estimator := &capturingGasEstimator{estimate: estimate}
			_, err := EstimateEIP7702Gas(context.Background(), estimator, validSimulationRequestForTest())
			if !errors.Is(err, ErrInvalidGasEstimate) || estimator.calls != 1 {
				t.Fatalf("EstimateEIP7702Gas() error = %v, calls = %d", err, estimator.calls)
			}
		})
	}
}

func TestEstimateEIP7702GasRejectsMalformedRequestWithoutRPC(t *testing.T) {
	estimator := &capturingGasEstimator{estimate: 1}
	request := validSimulationRequestForTest()
	request.AuthorizationList = nil
	_, err := EstimateEIP7702Gas(context.Background(), estimator, request)
	if !errors.Is(err, ErrInvalidSimulationRequest) || estimator.calls != 0 {
		t.Fatalf("EstimateEIP7702Gas() error = %v, calls = %d", err, estimator.calls)
	}
}

func validSimulationRequestForTest() EIP7702SimulationRequest {
	return EIP7702SimulationRequest{
		Sponsor: common.HexToAddress("0x1001"), Source: common.HexToAddress("0x2002"), GasLimit: 100_000,
		GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(2), Data: []byte{1},
		AuthorizationList: []types.SetCodeAuthorization{{ChainID: *uint256.NewInt(1), Address: common.HexToAddress("0x3003")}},
	}
}

type capturingGasEstimator struct {
	estimate    uint64
	err         error
	wantContext context.Context
	call        ethereum.CallMsg
	calls       int
}

func (estimator *capturingGasEstimator) EstimateGas(ctx context.Context, call ethereum.CallMsg) (uint64, error) {
	if estimator.wantContext != nil && ctx != estimator.wantContext {
		return 0, errors.New("unexpected context")
	}
	estimator.calls++
	estimator.call = call
	return estimator.estimate, estimator.err
}
