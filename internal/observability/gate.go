package observability

import (
	"errors"
	"sync"
)

var (
	ErrPaidActionsStopped = errors.New("платные действия остановлены")
	ErrNilPaidAction      = errors.New("платное действие не задано")
)

// PaidActionGate является общей границей для signer и broadcaster. Каждый
// такой вызов должен полностью находиться внутри Do.
type PaidActionGate struct {
	mu      sync.Mutex
	drained *sync.Cond
	stopped bool
	active  uint64
}

func NewPaidActionGate() *PaidActionGate {
	gate := &PaidActionGate{}
	gate.drained = sync.NewCond(&gate.mu)
	return gate
}

// Do резервирует право на одно signing/broadcast действие. Callback не должен
// вызывать Stop на том же gate, поскольку Stop ожидает завершения callback.
func (gate *PaidActionGate) Do(action func() error) error {
	if action == nil {
		return ErrNilPaidAction
	}
	gate.mu.Lock()
	gate.initLocked()
	if gate.stopped {
		gate.mu.Unlock()
		return ErrPaidActionsStopped
	}
	gate.active++
	gate.mu.Unlock()

	defer func() {
		gate.mu.Lock()
		gate.active--
		if gate.active == 0 {
			gate.drained.Broadcast()
		}
		gate.mu.Unlock()
	}()
	return action()
}

// Stop сначала запрещает новые callbacks, затем ждёт уже начатые. После
// возврата Stop signer/broadcaster не выполняется ни в одном callback gate.
func (gate *PaidActionGate) Stop() {
	gate.mu.Lock()
	gate.initLocked()
	gate.stopped = true
	for gate.active != 0 {
		gate.drained.Wait()
	}
	gate.mu.Unlock()
}

func (gate *PaidActionGate) Stopped() bool {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.stopped
}

func (gate *PaidActionGate) initLocked() {
	if gate.drained == nil {
		gate.drained = sync.NewCond(&gate.mu)
	}
}
