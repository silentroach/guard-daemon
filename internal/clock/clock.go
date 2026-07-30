package clock

import (
	"context"
	"time"
)

type Ticker interface {
	C() <-chan time.Time
	Stop()
}

type Timer interface {
	C() <-chan time.Time
	Stop()
}

type Clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
	NewTicker(time.Duration) Ticker
	NewTimer(time.Duration) Timer
}

type Real struct{}

func (Real) Now() time.Time {
	return time.Now()
}

func (Real) Sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (Real) NewTicker(duration time.Duration) Ticker {
	return realTicker{Ticker: time.NewTicker(duration)}
}

func (Real) NewTimer(duration time.Duration) Timer {
	return realTimer{Timer: time.NewTimer(duration)}
}

type realTicker struct {
	*time.Ticker
}

func (ticker realTicker) C() <-chan time.Time {
	return ticker.Ticker.C
}

type realTimer struct {
	*time.Timer
}

func (timer realTimer) C() <-chan time.Time {
	return timer.Timer.C
}

func (timer realTimer) Stop() {
	timer.Timer.Stop()
}
