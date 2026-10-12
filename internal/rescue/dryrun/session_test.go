package dryrun

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"reflect"
	"testing"
	"time"

	"guard-daemon/internal/contracts"
	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
)

func TestSessionPlansAndSimulatesOnly(t *testing.T) {
	t.Parallel()

	config := validConfig()
	codec, err := contracts.NewRescuerCodec()
	if err != nil {
		t.Fatal(err)
	}
	token := testAddress(5)
	tokenData, err := codec.PackSweepAll([]common.Address{token})
	if err != nil {
		t.Fatal(err)
	}
	ethData, err := codec.PackSweepEth()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		kind  domain.CandidateKind
		token common.Address
		data  []byte
	}{
		{name: "token", kind: domain.CandidateToken, token: token, data: tokenData},
		{name: "native", kind: domain.CandidateNative, data: ethData},
		{name: "periodic", kind: domain.CandidatePeriodic, data: ethData},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reader := &fakeReader{chainID: big.NewInt(int64(config.Network))}
			session, err := NewSession(context.Background(), 7, reader, config)
			if err != nil {
				t.Fatalf("NewSession() returned an error: %v", err)
			}
			_, _, _, attempts := NewGuards(config.Source, config.Sponsor)
			candidate := domain.RescueCandidate{
				Network:    config.Network,
				Source:     config.Source,
				Generation: 0,
				Kind:       test.kind,
				Token:      domain.Token{Address: test.token},
			}
			if err := session.Handle(context.Background(), candidate); err != nil {
				t.Fatalf("Handle() returned an error: %v", err)
			}

			wantCall := ethereum.CallMsg{From: config.Sponsor, To: &config.Source, Data: test.data}
			if len(reader.calls) != 1 || !reflect.DeepEqual(reader.calls[0], wantCall) {
				t.Fatalf("EstimateGas calls = %#v, want exactly %#v", reader.calls, wantCall)
			}
			if attempts.AuthorizationSignatures() != 0 || attempts.TransactionSignatures() != 0 || attempts.Broadcasts() != 0 {
				t.Fatalf("guard attempts = (%d, %d, %d), want all zeros", attempts.AuthorizationSignatures(), attempts.TransactionSignatures(), attempts.Broadcasts())
			}
			if session.Generation() != 7 {
				t.Fatalf("Generation() returned %d, want 7", session.Generation())
			}
			session.Close()
		})
	}
}

func TestNewSessionFailsClosed(t *testing.T) {
	t.Parallel()

	config := validConfig()
	rawRPCError := errors.New("private RPC detail")
	tests := []struct {
		name   string
		reader *fakeReader
		code   domain.ErrorCode
	}{
		{name: "chain read", reader: &fakeReader{chainErr: rawRPCError}, code: codeChainIDReadFailed},
		{name: "chain mismatch", reader: &fakeReader{chainID: big.NewInt(int64(config.Network + 1))}, code: codeChainIDMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewSession(context.Background(), 1, test.reader, config)
			assertErrorCode(t, err, test.code)
			if bytes.Contains([]byte(err.Error()), []byte(rawRPCError.Error())) {
				t.Fatal("classified error exposed source RPC details")
			}
		})
	}
}

func TestNewSessionRejectsInvalidConfigBeforeRPC(t *testing.T) {
	t.Parallel()

	valid := validConfig()
	tests := map[string]Config{
		"network":             withConfig(valid, func(config *Config) { config.Network = 0 }),
		"source":              withConfig(valid, func(config *Config) { config.Source = common.Address{} }),
		"sponsor":             withConfig(valid, func(config *Config) { config.Sponsor = common.Address{} }),
		"destination":         withConfig(valid, func(config *Config) { config.Destination = common.Address{} }),
		"rescuer":             withConfig(valid, func(config *Config) { config.Rescuer = common.Address{} }),
		"read timeout":        withConfig(valid, func(config *Config) { config.ReadTimeout = 0 }),
		"source sponsor":      withConfig(valid, func(config *Config) { config.Sponsor = config.Source }),
		"source destination":  withConfig(valid, func(config *Config) { config.Destination = config.Source }),
		"sponsor destination": withConfig(valid, func(config *Config) { config.Destination = config.Sponsor }),
	}
	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reader := &fakeReader{chainID: big.NewInt(901)}
			_, err := NewSession(context.Background(), 1, reader, config)
			assertErrorCode(t, err, codeInvalidConfig)
			if reader.chainCalls != 0 {
				t.Fatalf("ChainID calls = %d, want 0", reader.chainCalls)
			}
		})
	}
}

func TestHandleRejectsInvalidCandidateWithoutSimulation(t *testing.T) {
	t.Parallel()

	config := validConfig()
	tests := []struct {
		name      string
		candidate domain.RescueCandidate
		code      domain.ErrorCode
	}{
		{name: "network", candidate: candidateFor(config, domain.CandidateNative), code: codeCandidateMismatch},
		{name: "source", candidate: candidateFor(config, domain.CandidateNative), code: codeCandidateMismatch},
		{name: "generation", candidate: candidateFor(config, domain.CandidateNative), code: codeCandidateMismatch},
		{name: "zero token", candidate: candidateFor(config, domain.CandidateToken), code: codePlanFailed},
		{name: "unsupported", candidate: candidateFor(config, domain.CandidateKind(255)), code: codeCandidateUnsupported},
	}
	tests[0].candidate.Network++
	tests[1].candidate.Source = testAddress(9)
	tests[2].candidate.Generation++
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reader := &fakeReader{chainID: big.NewInt(int64(config.Network))}
			session, err := NewSession(context.Background(), 7, reader, config)
			if err != nil {
				t.Fatal(err)
			}
			err = session.Handle(context.Background(), test.candidate)
			assertErrorCode(t, err, test.code)
			if len(reader.calls) != 0 {
				t.Fatalf("EstimateGas calls = %d, want 0", len(reader.calls))
			}
		})
	}
}

func TestHandleClassifiesSimulationFailure(t *testing.T) {
	t.Parallel()

	config := validConfig()
	reader := &fakeReader{chainID: big.NewInt(int64(config.Network)), estimateErr: errors.New("private simulation detail")}
	session, err := NewSession(context.Background(), 7, reader, config)
	if err != nil {
		t.Fatal(err)
	}
	err = session.Handle(context.Background(), candidateFor(config, domain.CandidateNative))
	assertErrorCode(t, err, codeSimulationFailed)
}

type fakeReader struct {
	chainID     *big.Int
	chainErr    error
	estimateErr error
	chainCalls  int
	calls       []ethereum.CallMsg
}

func (reader *fakeReader) ChainID(context.Context) (*big.Int, error) {
	reader.chainCalls++
	if reader.chainID == nil {
		return nil, reader.chainErr
	}
	return new(big.Int).Set(reader.chainID), reader.chainErr
}

func (reader *fakeReader) EstimateGas(_ context.Context, call ethereum.CallMsg) (uint64, error) {
	reader.calls = append(reader.calls, call)
	return 100_000, reader.estimateErr
}

func validConfig() Config {
	return Config{
		Network:     901,
		Source:      testAddress(1),
		Sponsor:     testAddress(2),
		Destination: testAddress(3),
		Rescuer:     testAddress(4),
		ReadTimeout: time.Second,
	}
}

func candidateFor(config Config, kind domain.CandidateKind) domain.RescueCandidate {
	return domain.RescueCandidate{Network: config.Network, Source: config.Source, Generation: 7, Kind: kind}
}

func withConfig(config Config, change func(*Config)) Config {
	change(&config)
	return config
}

func testAddress(value byte) common.Address {
	var address common.Address
	address[len(address)-1] = value
	return address
}

func assertErrorCode(t *testing.T, err error, code domain.ErrorCode) {
	t.Helper()
	var classified *domain.ClassifiedError
	if !errors.As(err, &classified) || classified.Code != code {
		t.Fatalf("error = %v, want classified code %s", err, code)
	}
}

var _ Reader = (*fakeReader)(nil)
