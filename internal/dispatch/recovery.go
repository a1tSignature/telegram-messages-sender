package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"telegram-messages-sender/internal/domain"
	"telegram-messages-sender/internal/telegram"
)

const recoveryPoll = time.Second
const shutdownGrace = 4 * time.Second

type RecoveryTransport interface {
	Transport
	Status() telegram.Status
	Recover(context.Context, domain.UpdateCheckpoint, []domain.RecoveryReference) (domain.RecoveryBatch, error)
}

type recoveryCycle struct {
	initialized bool
	needed      bool
	offlineAt   time.Time
	wake        uint64
	outage      uint64
	retryAt     time.Time
}

func (r *recoveryCycle) needsRecovery(status telegram.Status, wake uint64, now time.Time) bool {
	if !status.TransportAvailable {
		if r.offlineAt.IsZero() {
			r.offlineAt = now
		}
		return false
	}
	if !r.offlineAt.IsZero() {
		r.needed = r.needed || now.Sub(r.offlineAt) >= telegram.SustainedInterruption
		r.offlineAt = time.Time{}
	}
	r.needed = r.needed || !r.initialized || wake != r.wake || status.Outage != r.outage
	return r.needed && !now.Before(r.retryAt)
}

func (e *Engine) NotifyWake() {
	e.wakeGeneration.Add(1)
	e.changed()
}

func (e *Engine) RunRecovering(ctx context.Context, transport RecoveryTransport) error {
	return e.run(ctx, transport, func(ctx context.Context) error {
		return e.recoveryLoop(ctx, transport)
	})
}

func (e *Engine) recoveryLoop(ctx context.Context, transport RecoveryTransport) error {
	var cycle recoveryCycle
	var due time.Time
	dirty := true
	account := transport.Status().AccountID
	for ctx.Err() == nil {
		status, now, wake := transport.Status(), e.now(), e.wakeGeneration.Load()
		if cycle.needsRecovery(status, wake, now) {
			retry, err := e.reconcile(ctx, transport, account)
			if err != nil {
				return err
			}
			if !retry.IsZero() {
				cycle.retryAt = retry
			} else {
				cycle = recoveryCycle{initialized: true, wake: wake, outage: status.Outage}
				e.gate.Lock()
				clear(e.queue)
				e.recoveredWake = wake
				e.recoveredOutage = status.Outage
				e.dispatchError = ""
				e.gate.Unlock()
				dirty = true
				continue
			}
		}
		if status.TransportAvailable && cycle.initialized && !cycle.needed && (dirty || (!due.IsZero() && !due.After(now))) {
			next, worked, err := e.step(ctx, transport)
			if err != nil {
				return err
			}
			if worked {
				dirty = true
				continue
			}
			due, dirty = next, false
		}
		wakeAt := e.now().Add(recoveryPoll)
		if status.TransportAvailable && !cycle.needed && !due.IsZero() && due.Before(wakeAt) {
			wakeAt = due
		}
		dirty = e.wait(ctx, wakeAt) || dirty
	}
	return nil
}

func (e *Engine) reconcile(ctx context.Context, transport RecoveryTransport, account domain.AccountID) (time.Time, error) {
	recoveryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		current, refs, err := e.state.RecoveryState(recoveryCtx, account)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return e.now().Add(5 * time.Second), nil
			}
			return time.Time{}, err
		}
		until, err := e.state.AccountWaitUntil(recoveryCtx)
		if errors.Is(err, context.DeadlineExceeded) {
			return e.now().Add(5 * time.Second), nil
		}
		if err != nil {
			return time.Time{}, err
		}
		if until.After(e.now()) {
			return until, nil
		}
		batch, err := transport.Recover(recoveryCtx, current, refs)
		if err != nil {
			var wait *telegram.ServerWait
			if errors.As(err, &wait) {
				e.gate.Lock()
				storeErr := e.persistWait(ctx, "", wait)
				e.gate.Unlock()
				return e.now().Add(wait.Duration), storeErr
			}
			if ctx.Err() != nil {
				return time.Time{}, ctx.Err()
			}
			e.SetUnavailable(fmt.Errorf("Telegram recovery: %w", err))
			return e.now().Add(5 * time.Second), nil
		}
		if batch.More && current == batch.Checkpoint {
			return time.Time{}, errors.New("Telegram recovery checkpoint did not advance")
		}
		if err := e.state.ApplyRecovery(ctx, account, current, batch, e.now()); err != nil {
			return time.Time{}, fmt.Errorf("persist delivery recovery: %w", err)
		}
		if !batch.More {
			return time.Time{}, nil
		}
	}
}

func drainingSubmission(ctx context.Context) (context.Context, func()) {
	live, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		timer := time.NewTimer(shutdownGrace)
		defer timer.Stop()
		select {
		case <-timer.C:
			cancel()
		case <-done:
		}
	})
	return live, func() {
		close(done)
		stop()
		cancel()
	}
}
