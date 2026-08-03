package observability

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

func TestPaidActionGateStopClosesEntryAndWaitsForActiveAction(t *testing.T) {
	t.Parallel()

	gate := NewPaidActionGate()
	entered := make(chan struct{})
	release := make(chan struct{})
	actionDone := make(chan error, 1)
	go func() {
		actionDone <- gate.Do(func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	stopDone := make(chan struct{})
	go func() {
		gate.Stop()
		close(stopDone)
	}()
	for !gate.Stopped() {
		runtime.Gosched()
	}
	select {
	case <-stopDone:
		t.Fatal("Stop вернулся до завершения активного платного действия")
	default:
	}

	var forbiddenCalls atomic.Uint64
	if err := gate.Do(func() error {
		forbiddenCalls.Add(1)
		return nil
	}); !errors.Is(err, ErrPaidActionsStopped) {
		t.Fatalf("Do while stopping error = %v, want ErrPaidActionsStopped", err)
	}
	close(release)
	if err := <-actionDone; err != nil {
		t.Fatal(err)
	}
	<-stopDone
	if err := gate.Do(func() error {
		forbiddenCalls.Add(1)
		return nil
	}); !errors.Is(err, ErrPaidActionsStopped) {
		t.Fatalf("Do after Stop error = %v, want ErrPaidActionsStopped", err)
	}
	if forbiddenCalls.Load() != 0 {
		t.Fatalf("после stop выполнено callbacks: %d", forbiddenCalls.Load())
	}
}

func TestPaidActionGateConcurrentStopIsIdempotent(t *testing.T) {
	t.Parallel()

	const stoppers = 16
	var gate PaidActionGate
	entered := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = gate.Do(func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	var wait sync.WaitGroup
	for index := 0; index < stoppers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			gate.Stop()
		}()
	}
	for !gate.Stopped() {
		runtime.Gosched()
	}
	close(release)
	wait.Wait()
	if !gate.Stopped() {
		t.Fatal("gate не сохранил stopped state")
	}
}

func TestPaidActionGateReturnsActionErrorAndRejectsNil(t *testing.T) {
	t.Parallel()

	gate := NewPaidActionGate()
	want := errors.New("синтетическая безопасная ошибка")
	if err := gate.Do(func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("Do error = %v, want action error", err)
	}
	if err := gate.Do(nil); !errors.Is(err, ErrNilPaidAction) {
		t.Fatalf("Do(nil) error = %v, want ErrNilPaidAction", err)
	}
}
