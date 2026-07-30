package rescue

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"guard-daemon/internal/clock"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestSessionHandlesTokenWithFakeSignersAndBroadcaster(t *testing.T) {
	source := localAddress(1)
	sponsor := localAddress(2)
	destination := localAddress(3)
	rescuer := localAddress(4)
	token := localAddress(5)
	chainID := domain.NetworkID(901)

	reader := &fakeReader{
		chainID:     big.NewInt(int64(chainID)),
		source:      source,
		destination: destination,
		rescuer:     rescuer,
		balances:    []*big.Int{big.NewInt(7), new(big.Int)},
	}
	authorizer := &fakeAuthorizationSigner{address: source}
	transactioner := &fakeTransactionSigner{address: sponsor}
	broadcaster := &fakeBroadcaster{}
	coordinator, err := NewCoordinator(Config{
		Network: domain.Network{
			Name:       "local-test",
			ChainID:    chainID,
			Rescuer:    rescuer,
			HasRescuer: true,
		},
		Source:      source,
		Sponsor:     sponsor,
		Destination: destination,
	}, authorizer, transactioner, clock.Real{}, observability.Discard{})
	if err != nil {
		t.Fatalf("NewCoordinator() error = %v", err)
	}
	session, err := coordinator.NewSession(context.Background(), 8, reader, broadcaster)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	candidate := domain.RescueCandidate{
		Network:    chainID,
		Kind:       domain.CandidateToken,
		Source:     source,
		Token:      domain.Token{Address: token, Symbol: "LOCAL", Decimals: 18},
		Generation: 8,
	}
	if err := session.Handle(context.Background(), candidate); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	if len(authorizer.calls) != 1 || len(transactioner.calls) != 1 || len(broadcaster.transactions) != 1 {
		t.Fatalf("attempt counts = authorization:%d transaction:%d broadcast:%d, want 1 each", len(authorizer.calls), len(transactioner.calls), len(broadcaster.transactions))
	}
	authorization := authorizer.calls[0]
	transaction := broadcaster.transactions[0]
	if authorization.Address != rescuer || authorization.Nonce != 9 || transaction.Nonce() != 7 || transaction.Gas() != tokenSweepGas || transaction.To() == nil || *transaction.To() != source {
		t.Fatalf("unexpected attempted authorization or transaction content")
	}
	authorizations := transaction.SetCodeAuthorizations()
	if len(authorizations) != 1 || authorizations[0].Address != rescuer || authorizations[0].Nonce != 9 {
		t.Fatalf("transaction authorization list = %#v", authorizations)
	}
	if len(transaction.Data()) != 4+32+32+32 || common.BytesToAddress(transaction.Data()[len(transaction.Data())-20:]) != token {
		t.Fatalf("transaction does not contain one-token sweepAll call")
	}
}

func TestTokenPostconditionReadFailureIsClassified(t *testing.T) {
	source := localAddress(11)
	sponsor := localAddress(12)
	destination := localAddress(13)
	rescuer := localAddress(14)
	token := localAddress(15)
	chainID := domain.NetworkID(902)
	rawFailure := errors.New("backend detail must remain private")
	reader := &fakeReader{
		chainID:      big.NewInt(int64(chainID)),
		source:       source,
		destination:  destination,
		rescuer:      rescuer,
		balances:     []*big.Int{big.NewInt(1)},
		balanceError: rawFailure,
	}
	observer := &fakeObserver{}
	coordinator, err := NewCoordinator(Config{
		Network: domain.Network{Name: "local-test", ChainID: chainID, Rescuer: rescuer, HasRescuer: true},
		Source:  source, Sponsor: sponsor, Destination: destination,
	}, &fakeAuthorizationSigner{address: source}, &fakeTransactionSigner{address: sponsor}, clock.Real{}, observer)
	if err != nil {
		t.Fatalf("NewCoordinator() error = %v", err)
	}
	session, err := coordinator.NewSession(context.Background(), 1, reader, &fakeBroadcaster{})
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	err = session.Handle(context.Background(), domain.RescueCandidate{
		Network: chainID, Kind: domain.CandidateToken, Source: source, Token: domain.Token{Address: token}, Generation: 1,
	})
	var classified *domain.ClassifiedError
	if !errors.As(err, &classified) || classified.Code != codePostcondition || !classified.Ambiguous {
		t.Fatalf("Handle() error = %v, want ambiguous %s", err, codePostcondition)
	}
	if strings.Contains(err.Error(), rawFailure.Error()) || len(coordinator.retrySnapshot()) != 0 {
		t.Fatalf("postcondition failure exposed raw detail or changed retry policy")
	}
	lastEvent := observer.events[len(observer.events)-1]
	if lastEvent.ErrorCode != codePostcondition || lastEvent.Candidate != (domain.CandidateID{}) {
		t.Fatalf("failure event = %#v", lastEvent)
	}
}

func TestStartupReadDeadlineCancelsHungRPC(t *testing.T) {
	source := localAddress(21)
	sponsor := localAddress(22)
	coordinator, err := NewCoordinator(Config{
		Network:        domain.Network{Name: "local-test", ChainID: 903},
		Source:         source,
		Sponsor:        sponsor,
		Destination:    localAddress(23),
		StartupTimeout: 20 * time.Millisecond,
	}, &fakeAuthorizationSigner{address: source}, &fakeTransactionSigner{address: sponsor}, clock.Real{}, observability.Discard{})
	if err != nil {
		t.Fatal(err)
	}
	reader := &fakeReader{chainIDCall: func(ctx context.Context) (*big.Int, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}

	_, err = coordinator.NewSession(context.Background(), 1, reader, &fakeBroadcaster{})
	var classified *domain.ClassifiedError
	if !errors.As(err, &classified) || classified.Code != codeContextCanceled {
		t.Fatalf("NewSession() error = %v, want deadline classification", err)
	}
}

func TestFeeReadDeadlineReleasesOperationLock(t *testing.T) {
	source := localAddress(31)
	sponsor := localAddress(32)
	coordinator, err := NewCoordinator(Config{
		Network:        domain.Network{Name: "local-test", ChainID: 904, Rescuer: localAddress(34), HasRescuer: true},
		Source:         source,
		Sponsor:        sponsor,
		Destination:    localAddress(33),
		FeeReadTimeout: 20 * time.Millisecond,
	}, &fakeAuthorizationSigner{address: source}, &fakeTransactionSigner{address: sponsor}, clock.Real{}, observability.Discard{})
	if err != nil {
		t.Fatal(err)
	}
	reader := &fakeReader{headerCall: func(ctx context.Context) (*types.Header, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	session := &Session{coordinator: coordinator, generation: 1, reader: reader, broadcaster: &fakeBroadcaster{}}

	err = session.renewAndSweep(context.Background(), domain.Token{Address: localAddress(35)})
	var classified *domain.ClassifiedError
	if !errors.As(err, &classified) || classified.Code != codeContextCanceled {
		t.Fatalf("renewAndSweep() error = %v, want fee deadline classification", err)
	}
	if !coordinator.beginOperation() {
		t.Fatal("operation lock remained held after fee deadline")
	}
	coordinator.endOperation()
}

type fakeReader struct {
	mu           sync.Mutex
	chainID      *big.Int
	source       common.Address
	destination  common.Address
	rescuer      common.Address
	balances     []*big.Int
	balanceError error
	chainIDCall  func(context.Context) (*big.Int, error)
	headerCall   func(context.Context) (*types.Header, error)
}

func (reader *fakeReader) ChainID(ctx context.Context) (*big.Int, error) {
	if reader.chainIDCall != nil {
		return reader.chainIDCall(ctx)
	}
	return new(big.Int).Set(reader.chainID), nil
}

func (reader *fakeReader) BalanceAt(context.Context, common.Address, *big.Int) (*big.Int, error) {
	return new(big.Int), nil
}

func (reader *fakeReader) CodeAt(context.Context, common.Address, *big.Int) ([]byte, error) {
	return append([]byte{0xef, 0x01, 0x00}, reader.rescuer.Bytes()...), nil
}

func (reader *fakeReader) PendingNonceAt(_ context.Context, address common.Address) (uint64, error) {
	if address == reader.source {
		return 9, nil
	}
	return 7, nil
}

func (reader *fakeReader) HeaderByNumber(ctx context.Context, _ *big.Int) (*types.Header, error) {
	if reader.headerCall != nil {
		return reader.headerCall(ctx)
	}
	return &types.Header{BaseFee: big.NewInt(1_000_000)}, nil
}

func (reader *fakeReader) SuggestGasPrice(context.Context) (*big.Int, error) {
	return big.NewInt(2_000_000), nil
}

func (reader *fakeReader) CallContract(_ context.Context, call ethereum.CallMsg, _ *big.Int) ([]byte, error) {
	if call.To != nil && *call.To == reader.rescuer {
		return common.LeftPadBytes(reader.destination.Bytes(), 32), nil
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if len(reader.balances) == 0 {
		return nil, reader.balanceError
	}
	balance := reader.balances[0]
	reader.balances = reader.balances[1:]
	return common.LeftPadBytes(balance.Bytes(), 32), nil
}

func (reader *fakeReader) TransactionReceipt(context.Context, common.Hash) (*types.Receipt, error) {
	return &types.Receipt{Status: types.ReceiptStatusSuccessful}, nil
}

type fakeAuthorizationSigner struct {
	address common.Address
	calls   []types.SetCodeAuthorization
}

func (signer *fakeAuthorizationSigner) Address() common.Address {
	return signer.address
}

func (signer *fakeAuthorizationSigner) SignAuthorization(_ context.Context, authorization types.SetCodeAuthorization) (types.SetCodeAuthorization, error) {
	signer.calls = append(signer.calls, authorization)
	return authorization, nil
}

type fakeTransactionSigner struct {
	address common.Address
	calls   []*types.Transaction
}

func (signer *fakeTransactionSigner) Address() common.Address {
	return signer.address
}

func (signer *fakeTransactionSigner) SignTransaction(_ context.Context, transaction *types.Transaction, _ *big.Int) (*types.Transaction, error) {
	signer.calls = append(signer.calls, transaction)
	return transaction, nil
}

type fakeBroadcaster struct {
	transactions []*types.Transaction
}

type fakeObserver struct {
	events []observability.Event
}

func (observer *fakeObserver) Record(event observability.Event) {
	observer.events = append(observer.events, event)
}

func (broadcaster *fakeBroadcaster) SendTransaction(_ context.Context, transaction *types.Transaction) error {
	broadcaster.transactions = append(broadcaster.transactions, transaction)
	return nil
}

func localAddress(value byte) common.Address {
	var address common.Address
	address[len(address)-1] = value
	return address
}
