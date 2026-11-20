package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"time"

	"telegram-messages-sender/internal/app"
	"telegram-messages-sender/internal/content"
	"telegram-messages-sender/internal/domain"
	"telegram-messages-sender/internal/store"
	"telegram-messages-sender/internal/telegram"
)

// The handoff leaves time for a waiting owner command to commit within its five-second deadline.
const submissionHandoffTimeout = 2 * time.Second

type Transport interface {
	content.Source
	PrepareSend(context.Context, domain.ChatID, content.PreparedVariant) (telegram.PreparedSend, telegram.DestinationCheck)
	VerifyDestinationMembership(context.Context, domain.ChatID) telegram.DestinationCheck
}

func (e *Engine) deliver(ctx context.Context, transport Transport, revision int64, allowNonAdmin bool, work occurrence) error {
	prepareCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	prepared, err := content.NewSelector(transport).Prepare(prepareCtx, work.publication.Variants, work.history.ConfirmedVariant)
	if err != nil {
		return e.handlePreparationError(ctx, revision, work.pair, err)
	}

	variant := prepared.Variant
	send, check := transport.PrepareSend(prepareCtx, work.pair.ChatID, variant)
	if !allowNonAdmin && !check.HasAdminRole() &&
		(check.Role == telegram.DestinationMember || check.State == telegram.DestinationVerified) {
		check.State = telegram.DestinationAdminRequired
	}
	if check.State != telegram.DestinationVerified || send == nil {
		return e.handlePreflight(ctx, work.pair, check)
	}

	var randomID int64
	for randomID == 0 {
		var random [8]byte
		rand.Read(random[:])
		randomID = int64(binary.LittleEndian.Uint64(random[:]))
	}

	var attempt int64
	var beginErr error
	submitCtx, cancelSend := drainingSubmission(ctx)
	defer cancelSend()
	result := send(submitCtx, randomID, func(writeCtx context.Context, write func(context.Context) error) error {
		e.gate.Lock()
		defer e.gate.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		if !e.recoveryReady(transport) {
			return telegram.ErrTransport
		}
		handoffCtx, cancelHandoff := context.WithTimeout(writeCtx, submissionHandoffTimeout)
		defer cancelHandoff()
		if err := handoffCtx.Err(); err != nil {
			return err
		}
		anchor := work.history.FirstDue
		if anchor.IsZero() {
			anchor = work.firstDue
		}
		attempt, beginErr = e.state.BeginSubmission(handoffCtx, revision, store.Submission{
			Pair: work.pair, VariantID: variant.ID, RandomID: randomID, FirstDue: anchor, SubmittedAt: e.now(),
		})
		if beginErr != nil {
			return beginErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !e.recoveryReady(transport) {
			return telegram.ErrTransport
		}
		return write(handoffCtx)
	})

	switch {
	case errors.Is(beginErr, store.ErrConfigurationChanged):
		return nil
	case beginErr != nil:
		return fmt.Errorf("persist delivery before submission: %w", beginErr)
	case attempt == 0:
		e.gate.Lock()
		defer e.gate.Unlock()
		e.deferPair(work.pair, result.Reason)
		return nil
	}

	now := e.now()
	outcome := store.DeliveryResult{
		Outcome: result.Outcome, CompletedAt: now, ConfirmedAt: result.ConfirmedAt, MessageID: result.MessageID, Reason: result.Reason,
	}
	if result.Wait != nil {
		outcome.WaitUntil = now.Add(result.Wait.Duration)
		outcome.WaitChat = waitDestination(work.pair.ChatID, result.Wait)
	}
	if result.CheckRights && ctx.Err() == nil {
		checkCtx, cancelCheck := context.WithTimeout(ctx, 5*time.Second)
		check := transport.VerifyDestinationMembership(checkCtx, work.pair.ChatID)
		cancelCheck()
		outcome.DisableChat = check.State == telegram.DestinationAccessLost
		if check.Wait != nil {
			outcome.WaitUntil = e.now().Add(check.Wait.Duration)
			outcome.WaitChat = waitDestination(work.pair.ChatID, check.Wait)
		}
	}

	// A canceled transport can still produce an outcome that must be recorded.
	commitCtx, cancelCommit := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelCommit()
	e.gate.Lock()
	defer e.gate.Unlock()
	if err := e.state.FinishSubmission(commitCtx, attempt, outcome); err != nil {
		return fmt.Errorf("persist delivery outcome: %w", err)
	}
	if outcome.DisableChat {
		e.removeChat(work.pair.ChatID)
	}
	if result.Outcome == domain.AttemptFailure {
		e.deferPair(work.pair, result.Reason)
	}
	return nil
}

func (e *Engine) recoveryReady(transport Transport) bool {
	if e.wakeGeneration.Load() != e.recoveredWake {
		return false
	}
	if current, ok := transport.(RecoveryTransport); ok {
		status := current.Status()
		return status.TransportAvailable && status.Outage == e.recoveredOutage
	}
	return true
}

func (e *Engine) handlePreparationError(ctx context.Context, revision int64, pair store.Pair, err error) error {
	e.gate.Lock()
	defer e.gate.Unlock()
	var wait *telegram.ServerWait
	if errors.As(err, &wait) {
		return e.persistWait(ctx, pair.ChatID, wait)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, content.ErrVariantsExhausted) {
		err := e.state.PausePublication(ctx, revision, pair.PublicationID)
		if errors.Is(err, store.ErrConfigurationChanged) {
			return nil
		}
		if err == nil {
			e.removePublication(pair.PublicationID)
		}
		return err
	}
	e.deferPair(pair, "source_unavailable")
	return nil
}

func (e *Engine) handlePreflight(ctx context.Context, pair store.Pair, check telegram.DestinationCheck) error {
	e.gate.Lock()
	defer e.gate.Unlock()
	if check.Wait != nil {
		return e.persistWait(ctx, pair.ChatID, check.Wait)
	}
	if check.State == telegram.DestinationAccessLost {
		commitCtx, cancelCommit := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancelCommit()
		if _, err := e.state.SetChatDisabled(commitCtx, pair.ChatID, true); err != nil {
			return err
		}
		e.removeChat(pair.ChatID)
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	e.deferPair(pair, string(check.State))
	return nil
}

func (e *Engine) persistWait(ctx context.Context, chat domain.ChatID, wait *telegram.ServerWait) error {
	commitCtx, cancelCommit := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelCommit()
	if err := e.state.SetDeliveryWait(commitCtx, waitDestination(chat, wait), e.now().Add(wait.Duration)); err != nil {
		return fmt.Errorf("persist Telegram wait: %w", err)
	}
	e.changed()
	return nil
}

func waitDestination(chat domain.ChatID, wait *telegram.ServerWait) domain.ChatID {
	if wait.ChatScoped {
		return chat
	}
	return ""
}

func (e *Engine) deferPair(pair store.Pair, reason string) {
	if item := e.queue[pair]; item != nil {
		item.delayRetry(e.now())
		item.blockedReason = reason
	}
}

func (e *Engine) removeChat(chat domain.ChatID) {
	for pair := range e.queue {
		if pair.ChatID == chat {
			delete(e.queue, pair)
		}
	}
}

func (e *Engine) reenable(ctx context.Context, chat domain.ChatID) (app.Result, error) {
	e.gate.Lock()
	transport := e.transport
	snapshot, err := e.state.Snapshot(ctx)
	e.gate.Unlock()
	if err != nil {
		return app.Result{}, err
	}
	if !slices.Contains(snapshot.DisabledChats, chat) {
		return app.Result{}, fmt.Errorf("%w. Select a disabled chat", app.ErrVerificationRequired)
	}
	if transport == nil {
		return app.Result{}, fmt.Errorf("%w. Connect to Telegram and try again", app.ErrVerificationRequired)
	}
	check := transport.VerifyDestinationMembership(ctx, chat)
	e.gate.Lock()
	defer e.gate.Unlock()
	if check.Wait != nil {
		if err := e.persistWait(ctx, chat, check.Wait); err != nil {
			return app.Result{}, err
		}
		return app.Result{}, fmt.Errorf("%w. Telegram asked you to wait %s before trying again", app.ErrVerificationRequired, check.Wait.Duration)
	}
	if err := ctx.Err(); err != nil {
		return app.Result{}, err
	}
	switch {
	case e.transport == nil, check.State == telegram.DestinationCheckUnavailable:
		return app.Result{}, fmt.Errorf("%w. Telegram could not verify membership. Try again when connected", app.ErrVerificationRequired)
	case check.State == telegram.DestinationAccessLost:
		return app.Result{}, fmt.Errorf("%w. Check that the connected Telegram account has access to the group", app.ErrVerificationRequired)
	case check.State == telegram.DestinationMigrated:
		return app.Result{}, fmt.Errorf("%w. The group moved. Select its replacement in the publication settings", app.ErrVerificationRequired)
	case check.State != telegram.DestinationVerified:
		return app.Result{}, fmt.Errorf("%w. Choose a Telegram group or a non-forum supergroup", app.ErrVerificationRequired)
	}
	current, err := e.state.Snapshot(ctx)
	if err != nil {
		return app.Result{}, err
	}
	if current.Revision != snapshot.Revision {
		return app.Result{}, store.ErrConfigurationChanged
	}
	if !current.AllowNonAdmin && !check.HasAdminRole() {
		return app.Result{}, fmt.Errorf("%w. Administrator or owner rights are required when sending without administrator rights is disabled", app.ErrVerificationRequired)
	}
	revision, err := e.state.SetChatDisabled(ctx, chat, false)
	if err == nil {
		e.changed()
	}
	return app.Result{Revision: revision}, err
}
