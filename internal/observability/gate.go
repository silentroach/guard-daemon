package observability

import (
	"errors"
	"sync"
)

var (
	ErrPaidActionsStopped = errors.New("paid actions are stopped")
	ErrNilPaidAction      = errors.New("paid action is nil")
)

// PaidActionGate синхронизирует остановку подписанта и модуля отправки. Каждый вызов
// этих компонентов должен полностью выполняться внутри Do.
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

// Do регистрирует начало одного действия подписания или отправки. Переданная функция
// не должна вызывать Stop для того же PaidActionGate, поскольку Stop ожидает её завершения.
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

// Stop сначала запрещает запуск новых переданных функций, а затем ждёт завершения уже
// начатых. После возврата Stop все начатые функции завершены, а новые запустить нельзя.
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
