package observability

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestAlertCooldownSurvivesRestart(t *testing.T) {
	clock := &fakeAlertClock{now: time.Unix(50, 0)}
	path := filepath.Join(t.TempDir(), "alerts.json")
	var firstNotifications, restartedNotifications int
	manager, err := NewAlertManager(AlertManagerConfig{Cooldown: time.Minute, Capacity: 8, StatePath: path}, clock, AlertSinkFunc(func(Alert) error { firstNotifications++; return nil }))
	if err != nil {
		t.Fatal(err)
	}
	if emitted, err := manager.Raise(1, AlertBudgetBlocked); err != nil || !emitted || firstNotifications != 1 {
		t.Fatalf("first Raise = %t, %v, notifications=%d", emitted, err, firstNotifications)
	}
	restarted, err := NewAlertManager(AlertManagerConfig{Cooldown: time.Minute, Capacity: 8, StatePath: path}, clock, AlertSinkFunc(func(Alert) error { restartedNotifications++; return nil }))
	if err != nil {
		t.Fatal(err)
	}
	if emitted, err := restarted.Raise(1, AlertBudgetBlocked); err != nil || emitted || restartedNotifications != 0 {
		t.Fatalf("restarted Raise = %t, %v, notifications=%d", emitted, err, restartedNotifications)
	}
}

func TestAlertPendingDeliveryReplaysAfterRestart(t *testing.T) {
	clock := &fakeAlertClock{now: time.Unix(50, 0)}
	path := filepath.Join(t.TempDir(), "alerts.json")
	manager, err := NewAlertManager(
		AlertManagerConfig{Cooldown: time.Minute, Capacity: 8, StatePath: path}, clock,
		AlertSinkFunc(func(Alert) error { return errors.New("sink unavailable") }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Raise(1, AlertBudgetBlocked); !errors.Is(err, ErrAlertDelivery) {
		t.Fatalf("Raise() error = %v", err)
	}
	var replayed []Alert
	restarted, err := NewAlertManager(
		AlertManagerConfig{Cooldown: time.Minute, Capacity: 8, StatePath: path}, clock,
		AlertSinkFunc(func(alert Alert) error { replayed = append(replayed, alert); return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 1 || replayed[0].State != AlertFiring {
		t.Fatalf("replayed alerts = %#v", replayed)
	}
	if emitted, err := restarted.Raise(1, AlertBudgetBlocked); err != nil || emitted {
		t.Fatalf("Raise() after replay = %t, %v", emitted, err)
	}
}

func TestAlertPersistenceFailureRollsBackInMemoryTransition(t *testing.T) {
	clock := &fakeAlertClock{now: time.Unix(50, 0)}
	parent := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "alerts.json")
	notifications := 0
	manager, err := NewAlertManager(
		AlertManagerConfig{Cooldown: time.Minute, Capacity: 8, StatePath: path}, clock,
		AlertSinkFunc(func(Alert) error { notifications++; return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parent, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Raise(1, AlertBudgetBlocked); !errors.Is(err, ErrAlertPersistence) {
		t.Fatalf("Raise() error = %v", err)
	}
	if manager.Active(1, AlertBudgetBlocked) || notifications != 0 {
		t.Fatalf("failed persistence escaped: active=%t notifications=%d", manager.Active(1, AlertBudgetBlocked), notifications)
	}
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if emitted, err := manager.Raise(1, AlertBudgetBlocked); err != nil || !emitted || notifications != 1 {
		t.Fatalf("Raise() after recovery = %t, %v, notifications=%d", emitted, err, notifications)
	}
}

func TestAlertManagerDeduplicatesWithFakeClockAndResolve(t *testing.T) {
	t.Parallel()

	clock := &fakeAlertClock{now: time.Date(2026, time.August, 3, 10, 0, 0, 0, time.UTC)}
	var notifications []Alert
	manager, err := NewAlertManager(
		AlertManagerConfig{Cooldown: time.Minute, Capacity: 4},
		clock,
		AlertSinkFunc(func(alert Alert) error { notifications = append(notifications, alert); return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}

	if emitted, err := manager.Raise(1, AlertRPCDegraded); err != nil || !emitted {
		t.Fatalf("initial Raise() = %v, %v", emitted, err)
	}
	if emitted, err := manager.Raise(1, AlertRPCDegraded); err != nil || emitted {
		t.Fatalf("deduplicated Raise() = %v, %v", emitted, err)
	}
	clock.Advance(time.Minute - time.Nanosecond)
	if emitted, err := manager.Raise(1, AlertRPCDegraded); err != nil || emitted {
		t.Fatalf("pre-cooldown Raise() = %v, %v", emitted, err)
	}
	clock.Advance(time.Nanosecond)
	if emitted, err := manager.Raise(1, AlertRPCDegraded); err != nil || !emitted {
		t.Fatalf("cooldown Raise() = %v, %v", emitted, err)
	}
	if resolved, err := manager.Resolve(1, AlertRPCDegraded); err != nil || !resolved {
		t.Fatalf("Resolve() = %v, %v", resolved, err)
	}
	if resolved, err := manager.Resolve(1, AlertRPCDegraded); err != nil || resolved {
		t.Fatalf("duplicate Resolve() = %v, %v", resolved, err)
	}
	if manager.Active(1, AlertRPCDegraded) {
		t.Fatal("resolved alert остался активным")
	}

	wantStates := []AlertState{AlertFiring, AlertFiring, AlertResolved}
	if len(notifications) != len(wantStates) {
		t.Fatalf("notifications = %#v, want states %#v", notifications, wantStates)
	}
	for index, want := range wantStates {
		if notifications[index].State != want {
			t.Fatalf("notification %d state = %v, want %v", index, notifications[index].State, want)
		}
	}
}

func TestAlertReactivationNotifiesInsideCooldown(t *testing.T) {
	clock := &fakeAlertClock{now: time.Unix(50, 0)}
	var states []AlertState
	manager, err := NewAlertManager(
		AlertManagerConfig{Cooldown: time.Hour, Capacity: 8}, clock,
		AlertSinkFunc(func(alert Alert) error { states = append(states, alert.State); return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Raise(1, AlertBudgetBlocked); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Resolve(1, AlertBudgetBlocked); err != nil {
		t.Fatal(err)
	}
	if emitted, err := manager.Raise(1, AlertBudgetBlocked); err != nil || !emitted {
		t.Fatalf("reactivation = %t, %v", emitted, err)
	}
	want := []AlertState{AlertFiring, AlertResolved, AlertFiring}
	if len(states) != len(want) {
		t.Fatalf("states = %v", states)
	}
	for index := range want {
		if states[index] != want[index] {
			t.Fatalf("states = %v, want %v", states, want)
		}
	}
}

func TestAlertManagerConcurrentRaiseEmitsOnce(t *testing.T) {
	t.Parallel()

	const workers = 64
	clock := &fakeAlertClock{now: time.Date(2026, time.August, 3, 10, 0, 0, 0, time.UTC)}
	var sinkMu sync.Mutex
	notifications := 0
	manager, err := NewAlertManager(
		AlertManagerConfig{Cooldown: time.Hour, Capacity: 2},
		clock,
		AlertSinkFunc(func(Alert) error {
			sinkMu.Lock()
			notifications++
			sinkMu.Unlock()
			return nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	errorsChannel := make(chan error, workers)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := manager.Raise(1, AlertBudgetBlocked)
			errorsChannel <- err
		}()
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatal(err)
		}
	}
	sinkMu.Lock()
	got := notifications
	sinkMu.Unlock()
	if got != 1 {
		t.Fatalf("notifications = %d, want 1", got)
	}
}

func TestAlertManagerCallsSinkOutsideLock(t *testing.T) {
	t.Parallel()

	clock := &fakeAlertClock{now: time.Date(2026, time.August, 3, 10, 0, 0, 0, time.UTC)}
	var manager *AlertManager
	var states []AlertState
	sink := AlertSinkFunc(func(alert Alert) error {
		states = append(states, alert.State)
		if alert.State == AlertFiring {
			if resolved, err := manager.Resolve(alert.ChainID, alert.Code); err != nil || !resolved {
				t.Fatalf("reentrant Resolve() = %v, %v", resolved, err)
			}
		}
		return nil
	})
	var err error
	manager, err = NewAlertManager(AlertManagerConfig{Cooldown: time.Minute, Capacity: 2}, clock, sink)
	if err != nil {
		t.Fatal(err)
	}
	if emitted, err := manager.Raise(1, AlertAmbiguousRescue); err != nil || !emitted {
		t.Fatalf("Raise() = %v, %v", emitted, err)
	}
	if len(states) != 2 || states[0] != AlertFiring || states[1] != AlertResolved {
		t.Fatalf("reentrant sink states = %#v", states)
	}
}

func TestAlertManagerCapacityNeverEvictsActiveAlert(t *testing.T) {
	t.Parallel()

	clock := &fakeAlertClock{now: time.Date(2026, time.August, 3, 10, 0, 0, 0, time.UTC)}
	manager, err := NewAlertManager(AlertManagerConfig{Cooldown: time.Minute, Capacity: 1}, clock, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Raise(1, AlertBudgetBlocked); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Raise(2, AlertRPCDegraded); !errors.Is(err, ErrAlertCapacity) {
		t.Fatalf("capacity error = %v, want ErrAlertCapacity", err)
	}
	if !manager.Active(1, AlertBudgetBlocked) || len(manager.Snapshot()) != 1 {
		t.Fatal("active alert был вытеснен при исчерпании capacity")
	}
	if _, err := manager.Resolve(1, AlertBudgetBlocked); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Raise(2, AlertRPCDegraded); err != nil {
		t.Fatalf("resolved entry не была переиспользована: %v", err)
	}
	status := manager.Snapshot()
	if len(status) != 1 || status[0].ChainID != 2 || !status[0].Active {
		t.Fatalf("snapshot after eviction = %#v", status)
	}
}

type fakeAlertClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *fakeAlertClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeAlertClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	clock.mu.Unlock()
}
