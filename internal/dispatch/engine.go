package dispatch

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"telegram-messages-sender/internal/app"
	"telegram-messages-sender/internal/domain"
	"telegram-messages-sender/internal/store"
)

type Engine struct {
	state *store.Store
	now   func() time.Time
	draw  func(int) int
	wake  chan struct{}

	gate          sync.Mutex
	queue         schedule
	transport     Transport
	dispatchError string
}

func New(state *store.Store) *Engine {
	return newEngine(state, time.Now, rand.IntN)
}

func newEngine(state *store.Store, now func() time.Time, draw func(int) int) *Engine {
	return &Engine{state: state, now: now, draw: draw, queue: make(schedule), wake: make(chan struct{}, 1)}
}

func (e *Engine) Run(ctx context.Context, transport Transport) (err error) {
	e.gate.Lock()
	if e.transport != nil {
		e.gate.Unlock()
		return errors.New("dispatch is already running")
	}
	e.transport = transport
	e.dispatchError = ""
	e.gate.Unlock()
	defer func() {
		e.gate.Lock()
		defer e.gate.Unlock()
		e.transport = nil
		if ctx.Err() == nil && err != nil {
			e.dispatchError = err.Error()
		}
	}()

	for ctx.Err() == nil {
		wake, worked, err := e.step(ctx, transport)
		if err != nil {
			return err
		}
		if !worked {
			e.wait(ctx, wake)
		}
	}
	return nil
}

func (e *Engine) wait(ctx context.Context, at time.Time) {
	var elapsed <-chan time.Time
	if !at.IsZero() {
		timer := time.NewTimer(at.Sub(e.now()))
		defer timer.Stop()
		elapsed = timer.C
	}
	select {
	case <-ctx.Done():
	case <-elapsed:
	case <-e.wake:
	}
}

func (e *Engine) step(ctx context.Context, transport Transport) (time.Time, bool, error) {
	e.gate.Lock()
	snapshot, delivery, err := e.refresh(ctx)
	if err != nil {
		e.gate.Unlock()
		return time.Time{}, false, err
	}
	item, wake := e.queue.next(e.now(), delivery.Waits)
	if item == nil {
		e.gate.Unlock()
		return wake, false, nil
	}
	work := *item
	e.gate.Unlock()

	return time.Time{}, true, e.deliver(ctx, transport, snapshot.Revision, work)
}

func (e *Engine) refresh(ctx context.Context) (store.Snapshot, store.DeliveryState, error) {
	snapshot, err := e.state.Snapshot(ctx)
	if err != nil {
		return store.Snapshot{}, store.DeliveryState{}, err
	}
	delivery, err := e.state.DeliveryState(ctx)
	if err == nil {
		e.queue.refresh(snapshot, delivery, e.now(), e.draw)
	}
	return snapshot, delivery, err
}

func (e *Engine) SetUnavailable(err error) {
	e.gate.Lock()
	defer e.gate.Unlock()
	e.dispatchError = err.Error()
}

func (e *Engine) Handler() app.Handler {
	handle := app.NewHandler(e)
	return func(ctx context.Context, command app.Command) (app.Result, error) {
		if command.Operation == app.ReenableChat && command.ChatID != "" && command.PublicationID == "" && command.Interval == 0 {
			return e.reenable(ctx, command.ChatID)
		}
		return handle(ctx, command)
	}
}

func (e *Engine) Status(ctx context.Context) (app.Result, error) {
	e.gate.Lock()
	defer e.gate.Unlock()
	result, err := e.state.Status(ctx)
	if err != nil {
		return result, err
	}
	delivery, err := e.state.DeliveryState(ctx)
	if err != nil {
		return app.Result{}, err
	}
	result.Dispatch = e.queue.status(e.now(), delivery.Pairs)
	result.DispatchError = e.dispatchError
	return result, nil
}

func (e *Engine) SetPublicationState(ctx context.Context, id domain.PublicationID, state domain.PublicationState) (int64, error) {
	e.gate.Lock()
	defer e.gate.Unlock()
	revision, err := e.state.SetPublicationState(ctx, id, state)
	if err == nil {
		if state == domain.Stopped {
			e.removePublication(id)
		}
		e.changed()
	}
	return revision, err
}

func (e *Engine) SetInterval(ctx context.Context, id domain.PublicationID, interval time.Duration) (int64, error) {
	e.gate.Lock()
	defer e.gate.Unlock()
	revision, err := e.state.SetInterval(ctx, id, interval)
	if err == nil {
		e.changed()
	}
	return revision, err
}

func (e *Engine) SavePublication(ctx context.Context, expectedRevision int64, publication domain.Publication) (int64, error) {
	e.gate.Lock()
	defer e.gate.Unlock()
	revision, err := e.state.SavePublication(ctx, expectedRevision, publication)
	if err == nil {
		for pair := range e.queue {
			if pair.PublicationID == publication.ID &&
				(publication.State == domain.Stopped || !containsDestination(publication, pair.ChatID)) {
				delete(e.queue, pair)
			}
		}
		e.changed()
	}
	return revision, err
}

func containsDestination(p domain.Publication, chat domain.ChatID) bool {
	for _, id := range p.Destinations {
		if id == chat {
			return true
		}
	}
	return false
}

func (e *Engine) removePublication(id domain.PublicationID) {
	for pair := range e.queue {
		if pair.PublicationID == id {
			delete(e.queue, pair)
		}
	}
}

func (e *Engine) changed() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}
