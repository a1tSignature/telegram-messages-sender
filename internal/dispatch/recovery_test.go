package dispatch

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"telegram-messages-sender/internal/app"
	"telegram-messages-sender/internal/content"
	"telegram-messages-sender/internal/domain"
	"telegram-messages-sender/internal/store"
	"telegram-messages-sender/internal/telegram"
)

const recoveryAccount domain.AccountID = 42

func TestRecoveryCheckpointRollsBackWithDeliveryTransitions(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := t.Context()
	f.save(t, "a", firstChat)
	f.save(t, "unresolved", "group:7")
	revision, err := f.state.BindAccount(ctx, recoveryAccount)
	if err != nil {
		t.Fatal(err)
	}
	cp := domain.UpdateCheckpoint{PTS: 10, Date: int(f.time.Unix())}
	if err := f.state.ApplyRecovery(ctx, recoveryAccount, domain.UpdateCheckpoint{}, domain.RecoveryBatch{Checkpoint: cp}, f.time); err != nil {
		t.Fatal(err)
	}

	submit := func(pub domain.PublicationID, chat domain.ChatID, random int64, at time.Time) int64 {
		t.Helper()
		attempt, err := f.state.BeginSubmission(ctx, revision, store.Submission{
			Pair: store.Pair{PublicationID: pub, ChatID: chat}, VariantID: "first", RandomID: random,
			FirstDue: f.time, SubmittedAt: at,
		})
		if err != nil {
			t.Fatal(err)
		}
		return attempt
	}
	old := submit("a", firstChat, 11, f.time)
	if err := f.state.FinishSubmission(ctx, old, store.DeliveryResult{Outcome: domain.AttemptUnknown, CompletedAt: f.time}); err != nil {
		t.Fatal(err)
	}
	newAt := f.time.Add(time.Minute)
	newer := submit("a", firstChat, 22, newAt)
	if err := f.state.FinishSubmission(ctx, newer, store.DeliveryResult{
		Outcome: domain.AttemptSuccess, CompletedAt: newAt, ConfirmedAt: newAt, MessageID: 44,
	}); err != nil {
		t.Fatal(err)
	}
	submit("unresolved", "group:7", 33, newAt)
	raw := f.raw(t)
	if _, err := raw.ExecContext(ctx, `
		CREATE TABLE fail_recovery_commit (id TEXT REFERENCES disabled_chats(chat_id) DEFERRABLE INITIALLY DEFERRED);
		CREATE TRIGGER fail_checkpoint AFTER UPDATE ON update_checkpoint BEGIN
			INSERT INTO fail_recovery_commit VALUES ('missing');
		END;`); err != nil {
		t.Fatal(err)
	}

	next := domain.UpdateCheckpoint{PTS: 15, Date: cp.Date + 30}
	batch := domain.RecoveryBatch{Checkpoint: next, Evidence: []domain.RecoveryEvidence{{
		RecoveryReference: domain.RecoveryReference{RandomID: 11, ChatID: firstChat, MessageID: 42}, SentAt: f.time,
	}}}
	if err := f.state.ApplyRecovery(ctx, recoveryAccount, cp, batch, newAt); err == nil {
		t.Fatal("recovery unexpectedly survived the forced checkpoint commit failure")
	}

	readRecovery := func() (domain.UpdateCheckpoint, []domain.RecoveryReference) {
		t.Helper()
		reopened, err := store.Open(ctx, f.path)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		got, refs, err := reopened.RecoveryState(ctx, recoveryAccount)
		if err != nil {
			t.Fatal(err)
		}
		return got, refs
	}
	if got, refs := readRecovery(); got != cp || len(refs) != 2 || !slices.ContainsFunc(refs, func(ref domain.RecoveryReference) bool {
		return ref.RandomID == 11 && ref.MessageID == 0
	}) {
		t.Fatalf("failed commit leaked checkpoint or confirmed an attempt: %+v %+v", got, refs)
	}
	pendingPair := store.Pair{PublicationID: "unresolved", ChatID: "group:7"}
	if f.delivery(t, true).Pairs[pendingPair].LastOutcome != domain.AttemptPending {
		t.Fatal("failed commit changed the pending outcome")
	}

	if _, err := raw.ExecContext(ctx, "DROP TRIGGER fail_checkpoint"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.state.ApplyRecovery(ctx, recoveryAccount, cp, batch, newAt); err != nil {
			t.Fatal(err)
		}
	}
	if got, refs := readRecovery(); got != next || len(refs) != 1 || refs[0].RandomID != 33 {
		t.Fatalf("replayed batch did not confirm only its match: %+v %+v", got, refs)
	}
	state := f.delivery(t, true)
	if pair := state.Pairs[store.Pair{PublicationID: "a", ChatID: firstChat}]; !pair.ConfirmedAt.Equal(newAt) || pair.Unresolved != 0 {
		t.Fatalf("older recovery replaced newer confirmation: %+v", pair)
	}
	if pair := state.Pairs[pendingPair]; pair.LastOutcome != domain.AttemptUnknown || !pair.ConfirmedAt.IsZero() || pair.Unresolved != 1 {
		t.Fatalf("unmatched attempt was lost or confirmed: %+v", pair)
	}
	if _, _, err := f.state.RecoveryState(ctx, recoveryAccount+1); !errors.Is(err, store.ErrAccountMismatch) {
		t.Fatalf("different account accessed recovery state: %v", err)
	}
}

type recoveringFixture struct {
	controlledTelegram
	status  atomic.Pointer[telegram.Status]
	batches chan domain.UpdateCheckpoint
}

func (f *recoveringFixture) Status() telegram.Status { return *f.status.Load() }

func (f *recoveringFixture) Recover(ctx context.Context, previous domain.UpdateCheckpoint, _ []domain.RecoveryReference) (domain.RecoveryBatch, error) {
	select {
	case f.batches <- previous:
	case <-ctx.Done():
		return domain.RecoveryBatch{}, ctx.Err()
	}
	next := previous
	if next.Date == 0 {
		next.Date = 1_800_000_000
	}
	next.PTS++
	return domain.RecoveryBatch{Checkpoint: next}, nil
}

func TestRecoveryHonorsPersistedAccountWaitAfterRestart(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := t.Context()
	if _, err := f.state.BindAccount(ctx, recoveryAccount); err != nil {
		t.Fatal(err)
	}
	until := f.time.Add(time.Minute)
	if err := f.state.SetDeliveryWait(ctx, "", until); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	engine := newEngine(reopened, func() time.Time { return f.time }, nil)
	transport := &recoveringFixture{batches: make(chan domain.UpdateCheckpoint, 2)}
	if retry, err := engine.reconcile(ctx, transport, recoveryAccount); err != nil || !retry.Equal(until) || len(transport.batches) != 0 {
		t.Fatalf("recovery during persisted wait: retry=%v, calls=%d, err=%v", retry, len(transport.batches), err)
	}
	f.time = until
	if retry, err := engine.reconcile(ctx, transport, recoveryAccount); err != nil || !retry.IsZero() || len(transport.batches) != 1 {
		t.Fatalf("recovery after wait: retry=%v, calls=%d, err=%v", retry, len(transport.batches), err)
	}
}

func TestStartupWakeAndReconnectPreserveControlsAndCadence(t *testing.T) {
	f := newDeliveryFixture(t)
	f.save(t, "a", firstChat, "group:17")
	f.save(t, "paused", "group:19")
	_, err := f.state.BindAccount(t.Context(), recoveryAccount)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.SetPublicationState(t.Context(), "paused", domain.Stopped); err != nil {
		t.Fatal(err)
	}
	revision, err := f.state.SetChatDisabled(t.Context(), "group:17", true)
	if err != nil {
		t.Fatal(err)
	}
	initial := f.time.Add(-time.Minute)
	if _, err := f.state.BeginSubmission(t.Context(), revision, store.Submission{
		Pair: store.Pair{PublicationID: "a", ChatID: firstChat}, VariantID: "first", RandomID: 73,
		FirstDue: initial, SubmittedAt: initial,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.state.SetDeliveryWait(t.Context(), firstChat, f.time.Add(5*time.Minute+time.Second)); err != nil {
		t.Fatal(err)
	}

	var clock atomic.Int64
	clock.Store(f.time.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	f.engine.now = now
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	transport := &recoveringFixture{batches: make(chan domain.UpdateCheckpoint, 5)}
	setStatus := func(available bool, outage uint64) {
		transport.status.Store(&telegram.Status{AccountID: recoveryAccount, TransportAvailable: available, Outage: outage})
		f.engine.changed()
	}
	setStatus(true, 0)
	sent := make(chan domain.ChatID, 3)
	transport.send = func(chat domain.ChatID, _ content.PreparedVariant, id int64) telegram.SendResult {
		if id == 73 {
			t.Error("replayed the unresolved correlation")
		}
		sent <- chat
		return telegram.SendResult{Outcome: domain.AttemptSuccess, ConfirmedAt: now(), MessageID: 60}
	}
	done := make(chan error, 1)
	go func() { done <- f.engine.RunRecovering(ctx, transport) }()
	defer func() { cancel(); <-done }()
	recovered := func(pts int) {
		t.Helper()
		select {
		case cp := <-transport.batches:
			if cp.PTS != pts {
				t.Fatalf("checkpoint before recovery = %+v, expected pts %d", cp, pts)
			}
		case <-ctx.Done():
			t.Fatal("recovery did not run")
		}
	}
	recovered(0)
	f.engine.NotifyWake()
	recovered(1)
	setStatus(false, 0)
	clock.Add(int64(5 * time.Minute))
	setStatus(true, 1)
	recovered(2)
	var status app.Result
	expectDue := f.time.Add(6 * time.Minute)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		status, err = f.engine.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(status.Dispatch, func(item app.DispatchStatus) bool {
			return item.PublicationID == "a" && item.ChatID == firstChat &&
				item.DueAt != nil && item.DueAt.Equal(expectDue)
		}) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("outage cycle did not rebuild a future anchored slot: %+v", status.Dispatch)
		case <-tick.C:
		}
	}
	if status.Publications[1].State != domain.Stopped || len(status.DisabledChats) != 1 || len(status.Waits) != 1 ||
		status.Waits[0].ChatID != firstChat {
		t.Fatalf("controls and waits did not survive recovery: %+v", status)
	}
	select {
	case chat := <-sent:
		t.Fatalf("dispatched too early to %s", chat)
	default:
	}
	clock.Add(int64(time.Minute))
	f.engine.changed()
	select {
	case chat := <-sent:
		if chat != firstChat {
			t.Fatalf("sent to an excluded pair: %s", chat)
		}
	case <-ctx.Done():
		t.Fatal("the next future cadence slot did not dispatch")
	}
	select {
	case cp := <-transport.batches:
		t.Fatalf("one outage recovered twice: %+v", cp)
	default:
	}
}

func TestShortTransportInterruptionDoesNotRequestRecovery(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	cycle := recoveryCycle{initialized: true}
	offline := telegram.Status{}
	online := telegram.Status{TransportAvailable: true}
	if cycle.needsRecovery(offline, 0, start) || cycle.needsRecovery(online, 0, start.Add(time.Second)) {
		t.Fatal("a transient failure started a new recovery cycle")
	}
	if cycle.needsRecovery(offline, 0, start.Add(2*time.Second)) ||
		!cycle.needsRecovery(online, 0, start.Add(2*time.Second+telegram.SustainedInterruption)) {
		t.Fatal("a sustained outage did not wait for the connection and request recovery")
	}
}

type drainResponse struct {
	controlledTelegram
	entered chan struct{}
	reply   chan struct{}
}

func (f *drainResponse) PrepareSend(context.Context, domain.ChatID, content.PreparedVariant) (telegram.PreparedSend, telegram.DestinationCheck) {
	return func(ctx context.Context, _ int64, handoff telegram.SendHandoff) telegram.SendResult {
		if err := handoff(ctx, func(context.Context) error { return nil }); err != nil {
			return telegram.SendResult{Outcome: domain.AttemptUnknown}
		}
		close(f.entered)
		select {
		case <-f.reply:
			if ctx.Err() == nil {
				return telegram.SendResult{Outcome: domain.AttemptSuccess, ConfirmedAt: time.Now(), MessageID: 5}
			}
		case <-ctx.Done():
		}
		return telegram.SendResult{Outcome: domain.AttemptUnknown}
	}, telegram.DestinationCheck{State: telegram.DestinationVerified, Role: telegram.DestinationAdmin}
}

func TestShutdownDrainsAnAlreadySubmittedResponse(t *testing.T) {
	f := newDeliveryFixture(t)
	f.save(t, "a", firstChat)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	transport := &drainResponse{entered: make(chan struct{}), reply: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- f.engine.Run(ctx, transport) }()
	select {
	case <-transport.entered:
	case <-ctx.Done():
		t.Fatal("no submitted attempt to drain")
	}
	cancel()
	close(transport.reply)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if state := f.delivery(t, true).Pairs[store.Pair{PublicationID: "a", ChatID: firstChat}]; state.LastOutcome != domain.AttemptSuccess {
		t.Fatalf("shutdown did not persist the received response: %+v", state)
	}
}

func TestFreshRecoveryCycleKeepsConfirmedIntervalAndOffsets(t *testing.T) {
	confirmed := time.Unix(1_800_000_000, 0)
	resumed := confirmed.Add(2 * time.Minute)
	pub := domain.Publication{
		ID: "a", State: domain.Enabled, Interval: 20 * time.Minute,
		Destinations: []domain.ChatID{firstChat, "group:13", "group:14"},
	}
	first := store.Pair{PublicationID: pub.ID, ChatID: firstChat}
	state := store.DeliveryState{Pairs: map[store.Pair]store.PairDelivery{first: {ConfirmedAt: confirmed}}}
	draws := []int{4, 59}
	queue := make(schedule)
	queue.refresh(store.Snapshot{Publications: []domain.Publication{pub}}, state, resumed, func(int) int {
		result := draws[0]
		draws = draws[1:]
		return result
	})
	for chat, at := range map[domain.ChatID]time.Time{
		firstChat: confirmed.Add(20 * time.Minute), "group:13": resumed.Add(34 * time.Second), "group:14": resumed.Add(89 * time.Second),
	} {
		if got := queue[store.Pair{PublicationID: pub.ID, ChatID: chat}].due; !got.Equal(at) {
			t.Fatalf("recovered due time for %s was %s, want %s", chat, got, at)
		}
	}
	if ready, _ := queue.next(resumed, nil); ready != nil || len(queue) != 3 {
		t.Fatalf("recovery accumulated work or allowed an early repeat: %+v", ready)
	}
}
