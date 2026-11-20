package dispatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/transport"

	"telegram-messages-sender/internal/app"
	"telegram-messages-sender/internal/content"
	"telegram-messages-sender/internal/domain"
	"telegram-messages-sender/internal/store"
	"telegram-messages-sender/internal/telegram"
)

const firstChat domain.ChatID = "group:12"

type controlledTelegram struct {
	resolve func(domain.SavedMessage) (content.Kind, error)
	check   func(domain.ChatID) telegram.DestinationCheck
	prepare func(domain.ChatID, content.PreparedVariant)
	send    func(domain.ChatID, content.PreparedVariant, int64) telegram.SendResult
}

func (f *controlledTelegram) ResolveSaved(_ context.Context, ref domain.SavedMessage) (content.Kind, error) {
	return f.resolve(ref)
}

func (f *controlledTelegram) VerifyDestinationMembership(_ context.Context, chat domain.ChatID) telegram.DestinationCheck {
	if f.check != nil {
		return f.check(chat)
	}
	return telegram.DestinationCheck{State: telegram.DestinationVerified, Role: telegram.DestinationAdmin}
}

func (f *controlledTelegram) PrepareSend(ctx context.Context, chat domain.ChatID, variant content.PreparedVariant) (telegram.PreparedSend, telegram.DestinationCheck) {
	if f.prepare != nil {
		f.prepare(chat, variant)
	}
	check := f.VerifyDestinationMembership(ctx, chat)
	return func(ctx context.Context, randomID int64, handoff telegram.SendHandoff) telegram.SendResult {
		if handoff(ctx, func(context.Context) error { return nil }) != nil {
			return telegram.SendResult{}
		}
		return f.send(chat, variant, randomID)
	}, check
}

type deliveryFixture struct {
	engine *Engine
	state  *store.Store
	path   string
	time   time.Time
}

func newDeliveryFixture(t *testing.T) *deliveryFixture {
	t.Helper()
	f := &deliveryFixture{path: filepath.Join(t.TempDir(), "state.db"), time: time.Unix(1_800_000_000, 0)}
	var err error
	f.state, err = store.Open(t.Context(), f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.state.Close() })
	f.engine = newEngine(f.state, func() time.Time { return f.time }, func(int) int { return 0 })
	return f
}

func (f *deliveryFixture) save(t *testing.T, id domain.PublicationID, chats ...domain.ChatID) {
	t.Helper()
	ctx := t.Context()
	snapshot, err := f.state.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.engine.SavePublication(ctx, snapshot.Revision, domain.Publication{
		ID: id, State: domain.Enabled, Interval: time.Minute, Destinations: chats,
		Variants: []domain.Variant{{ID: "first", Text: "original"}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *deliveryFixture) step(t *testing.T, transport Transport) (time.Time, bool) {
	t.Helper()
	wake, worked, err := f.engine.step(t.Context(), transport)
	if err != nil {
		t.Fatal(err)
	}
	return wake, worked
}

func (f *deliveryFixture) delivery(t *testing.T, reopen bool) store.DeliveryState {
	t.Helper()
	s := f.state
	if reopen {
		var err error
		s, err = store.Open(t.Context(), f.path)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
	}
	state, err := s.DeliveryState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func (f *deliveryFixture) raw(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSubmissionGateAllowsStopAndEditOnlyBeforeHandoff(t *testing.T) {
	for _, operation := range []string{"stop", "edit", "interval", "after-handoff"} {
		t.Run(operation, func(t *testing.T) {
			f := newDeliveryFixture(t)
			f.save(t, "a", firstChat)
			var sends int
			mutate := func() {
				ctx := t.Context()
				switch operation {
				case "edit":
					state, err := f.state.Snapshot(ctx)
					if err != nil {
						t.Fatal(err)
					}
					publication := state.Publications[0]
					publication.Variants[0].Text = "edited"
					_, err = f.engine.SavePublication(ctx, state.Revision, publication)
					if err != nil {
						t.Fatal(err)
					}
				case "interval":
					if _, err := f.engine.SetInterval(ctx, "a", 2*time.Minute); err != nil {
						t.Fatal(err)
					}
				default:
					if _, err := f.engine.Handler()(ctx, app.Command{Operation: app.Stop, PublicationID: "a"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			transport := &controlledTelegram{send: func(_ domain.ChatID, variant content.PreparedVariant, id int64) telegram.SendResult {
				sends++
				var storedID int64
				err := f.raw(t).QueryRowContext(t.Context(), "SELECT random_id FROM delivery_attempts WHERE outcome = 'pending'").Scan(&storedID)
				if err != nil || id == 0 || storedID != id {
					t.Fatalf("handoff preceded durable correlation: %d, %d, %v", id, storedID, err)
				}
				switch operation {
				case "after-handoff":
					mutate()
					if variant.Text != "original" {
						t.Fatal("submitted variant changed")
					}
				case "edit":
					if variant.Text != "edited" {
						t.Fatal("stale prepared text was submitted")
					}
				}
				return telegram.SendResult{Outcome: domain.AttemptSuccess, ConfirmedAt: f.time, MessageID: 1}
			}}
			if operation != "after-handoff" {
				transport.prepare = func(_ domain.ChatID, _ content.PreparedVariant) {
					transport.prepare = nil
					mutate()
				}
			}
			f.step(t, transport)
			want := 0
			if operation == "after-handoff" {
				want = 1
			}
			if sends != want {
				t.Fatalf("initial sends = %d, want %d", sends, want)
			}
			f.step(t, transport)
			if operation == "edit" || operation == "interval" {
				want = 1
			}
			if sends != want {
				t.Fatalf("final sends = %d, want %d", sends, want)
			}
			if operation == "after-handoff" {
				state, err := f.engine.Status(t.Context())
				if err != nil || state.Publications[0].State != domain.Stopped || state.Dispatch[0].Outcome != domain.AttemptSuccess {
					t.Fatalf("outcome overwrote committed stop: %+v, %v", state, err)
				}
			}
		})
	}
}

func TestCoincidentPublicationsUseOneInFlightSubmission(t *testing.T) {
	f := newDeliveryFixture(t)
	f.save(t, "b", "group:13")
	f.save(t, "a", firstChat)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	release := make(chan struct{})
	starts := make(chan domain.ChatID, 2)
	var inFlight atomic.Int32
	premature := make(chan struct{}, 1)
	transport := &controlledTelegram{send: func(chat domain.ChatID, _ content.PreparedVariant, _ int64) telegram.SendResult {
		if inFlight.Add(1) != 1 {
			premature <- struct{}{}
		}
		defer inFlight.Add(-1)
		starts <- chat
		select {
		case <-release:
		case <-ctx.Done():
		}
		return telegram.SendResult{Outcome: domain.AttemptSuccess, ConfirmedAt: f.time, MessageID: 1}
	}}
	done := make(chan error, 1)
	go func() { done <- f.engine.Run(ctx, transport) }()
	t.Cleanup(func() { cancel(); <-done })
	assertStart := func(want domain.ChatID) {
		t.Helper()
		select {
		case got := <-starts:
			if got != want {
				t.Fatalf("submitted to %s before %s", got, want)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	assertStart(firstChat)
	if err := f.engine.Run(ctx, transport); err == nil {
		t.Fatal("a second worker was admitted")
	}
	close(release)
	assertStart("group:13")
	select {
	case <-premature:
		t.Fatal("dispatch submitted two coincident publications in parallel")
	default:
	}
}

func TestDeliveryCommitFailuresStopWithoutLosingPendingCorrelation(t *testing.T) {
	for _, phase := range []string{"insert", "update"} {
		t.Run(phase, func(t *testing.T) {
			f := newDeliveryFixture(t)
			f.save(t, "a", firstChat)
			_, err := f.raw(t).ExecContext(t.Context(), fmt.Sprintf(`
				CREATE TABLE commit_guard (
					chat_id TEXT REFERENCES disabled_chats(chat_id) DEFERRABLE INITIALLY DEFERRED
				);
				CREATE TRIGGER fail_at_commit AFTER %s ON delivery_attempts
				BEGIN
					INSERT INTO commit_guard (chat_id) VALUES ('missing');
				END;`, phase))
			if err != nil {
				t.Fatal(err)
			}
			var sends int
			transport := &controlledTelegram{send: func(_ domain.ChatID, _ content.PreparedVariant, _ int64) telegram.SendResult {
				sends++
				return telegram.SendResult{Outcome: domain.AttemptSuccess, ConfirmedAt: f.time, MessageID: 1}
			}}
			_, _, err = f.engine.step(t.Context(), transport)
			contextMessage := "before submission"
			wantSends := 0
			if phase == "update" {
				contextMessage, wantSends = "delivery outcome", 1
			}
			if err == nil || !strings.Contains(err.Error(), contextMessage) || sends != wantSends {
				t.Fatalf("failed %s returned %v after %d sends", phase, err, sends)
			}
			state := f.delivery(t, true)
			row := state.Pairs[store.Pair{PublicationID: "a", ChatID: firstChat}]
			if !row.ConfirmedAt.IsZero() || phase == "insert" && len(state.Pairs) != 0 ||
				phase == "update" && row.LastOutcome != domain.AttemptPending {
				t.Fatalf("partial state after failed commit: %+v", state)
			}
			if phase == "update" {
				if _, worked := f.step(t, transport); worked || sends != 1 {
					t.Fatal("unresolved attempt was submitted again")
				}
			}
		})
	}
}

func TestNonAdminSendingRequiresOptIn(t *testing.T) {
	ctx := t.Context()
	f := newDeliveryFixture(t)
	f.save(t, "a", firstChat)
	sends := 0
	transport := &controlledTelegram{
		check: func(domain.ChatID) telegram.DestinationCheck {
			return telegram.DestinationCheck{State: telegram.DestinationVerified, Role: telegram.DestinationMember}
		},
		send: func(domain.ChatID, content.PreparedVariant, int64) telegram.SendResult {
			sends++
			return telegram.SendResult{Outcome: domain.AttemptSuccess, ConfirmedAt: f.time, MessageID: sends}
		},
	}
	f.step(t, transport)
	status, err := f.engine.Status(ctx)
	if err != nil || status.AllowNonAdmin || sends != 0 || len(status.Dispatch) != 1 ||
		status.Dispatch[0].Reason != string(telegram.DestinationAdminRequired) || len(status.DisabledChats) != 0 {
		t.Fatalf("default did not block non-admin sending: %+v, %v, sends=%d", status, err, sends)
	}

	if err := f.engine.SetAllowNonAdmin(ctx, true); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	snapshot, err := reopened.Snapshot(ctx)
	if err != nil || !snapshot.AllowNonAdmin {
		t.Fatalf("opt-in was not persisted: %+v, %v", snapshot, err)
	}
	f.step(t, transport)
	if sends != 1 {
		t.Fatalf("opt-in did not resume dispatch: sends=%d", sends)
	}

	f.time = f.time.Add(time.Minute)
	transport.prepare = func(domain.ChatID, content.PreparedVariant) {
		if err := f.engine.SetAllowNonAdmin(ctx, false); err != nil {
			t.Fatal(err)
		}
	}
	f.step(t, transport)
	transport.prepare = nil
	f.step(t, transport)
	if sends != 1 {
		t.Fatalf("non-admin send proceeded after opt-out: sends=%d", sends)
	}
}

func TestDestinationLossDisablesEveryPublicationAndReenableNeedsCheck(t *testing.T) {
	f := newDeliveryFixture(t)
	f.save(t, "a", firstChat)
	f.save(t, "b", firstChat, "group:13")
	verification := telegram.DestinationCheckUnavailable
	var sends []domain.ChatID
	transport := &controlledTelegram{
		check: func(chat domain.ChatID) telegram.DestinationCheck {
			if chat == firstChat {
				return telegram.DestinationCheck{State: verification, Role: telegram.DestinationAdmin}
			}
			return telegram.DestinationCheck{State: telegram.DestinationVerified, Role: telegram.DestinationAdmin}
		},
		send: func(chat domain.ChatID, _ content.PreparedVariant, _ int64) telegram.SendResult {
			sends = append(sends, chat)
			return telegram.SendResult{Outcome: domain.AttemptSuccess, ConfirmedAt: f.time, MessageID: 1}
		},
	}
	f.step(t, transport)
	if result, _ := f.engine.Status(t.Context()); len(result.DisabledChats) != 0 || len(sends) != 0 {
		t.Fatal("temporary failure either sent or disabled the chat")
	}
	verification = telegram.DestinationAccessLost
	f.step(t, transport)
	f.time = f.time.Add(31 * time.Second)
	f.step(t, transport)
	result, err := f.engine.Status(t.Context())
	if err != nil || len(sends) != 1 || sends[0] != "group:13" || len(result.DisabledChats) != 1 ||
		result.DisabledChats[0].ID != firstChat {
		t.Fatalf("rights loss did not isolate destination: %+v, %v, sends %v", result, err, sends)
	}
	f.engine.transport = transport
	command := app.Command{Operation: app.ReenableChat, ChatID: firstChat}
	if _, err := f.engine.Handler()(t.Context(), command); !errors.Is(err, app.ErrVerificationRequired) {
		t.Fatalf("re-enable without rights: %v", err)
	}
	verification = telegram.DestinationVerified
	f.step(t, transport)
	if len(sends) != 1 {
		t.Fatal("restored rights re-enabled dispatch without a command")
	}
	if _, err := f.engine.Handler()(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	f.step(t, transport)
	if len(sends) != 2 || sends[1] != firstChat {
		t.Fatalf("verified re-enable did not restore dispatch: %v", sends)
	}
}

func TestWaitScopesSurviveSQLiteReopen(t *testing.T) {
	for _, chatScoped := range []bool{true, false} {
		t.Run(fmt.Sprint(chatScoped), func(t *testing.T) {
			f := newDeliveryFixture(t)
			f.save(t, "a", firstChat)
			f.save(t, "b", firstChat)
			f.save(t, "c", "group:13")
			var sends []domain.ChatID
			transport := &controlledTelegram{send: func(chat domain.ChatID, _ content.PreparedVariant, _ int64) telegram.SendResult {
				sends = append(sends, chat)
				if chat == firstChat {
					return telegram.SendResult{Outcome: domain.AttemptWait, Wait: &telegram.ServerWait{Duration: time.Minute, ChatScoped: chatScoped}}
				}
				return telegram.SendResult{Outcome: domain.AttemptSuccess, ConfirmedAt: f.time, MessageID: 1}
			}}
			f.step(t, transport)
			chat := domain.ChatID("")
			if chatScoped {
				chat = firstChat
			}
			state := f.delivery(t, true)
			if !state.Waits[chat].Equal(f.time.Add(time.Minute)) || state.Pairs[store.Pair{PublicationID: "a", ChatID: firstChat}].LastOutcome != domain.AttemptWait {
				t.Fatalf("wait missing after reopen: %+v", state)
			}
			wake, worked := f.step(t, transport)
			if chatScoped {
				if !worked || len(sends) != 2 || sends[1] != "group:13" {
					t.Fatalf("chat wait leaked to another destination: %v", sends)
				}
			} else if worked || !wake.Equal(f.time.Add(time.Minute)) || len(sends) != 1 {
				t.Fatalf("account-wide wait did not hold dispatch: %v %v %v", worked, wake, sends)
			}
		})
	}
}

func TestLateConfirmationKeepsNewerVariantAndProtectsUnknownHistory(t *testing.T) {
	f := newDeliveryFixture(t)
	f.save(t, "a", firstChat)
	ctx := t.Context()
	snapshot, err := f.state.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pub := snapshot.Publications[0]
	pub.Variants = append(pub.Variants, domain.Variant{ID: "second", Text: "second text"})
	revision, err := f.engine.SavePublication(ctx, snapshot.Revision, pub)
	if err != nil {
		t.Fatal(err)
	}

	pair := store.Pair{PublicationID: "a", ChatID: firstChat}
	submit := func(random int64, variant domain.VariantID, at time.Time) int64 {
		t.Helper()
		id, err := f.state.BeginSubmission(ctx, revision, store.Submission{
			Pair: pair, VariantID: variant, RandomID: random, FirstDue: f.time, SubmittedAt: at,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	finish := func(id int64, outcome domain.AttemptOutcome, confirmed time.Time) {
		t.Helper()
		if err := f.state.FinishSubmission(ctx, id, store.DeliveryResult{
			Outcome: outcome, CompletedAt: f.time.Add(5 * time.Minute), ConfirmedAt: confirmed,
		}); err != nil {
			t.Fatal(err)
		}
	}

	old := submit(10, "first", f.time)
	finish(old, domain.AttemptUnknown, time.Time{})
	newTime := f.time.Add(time.Minute)
	newer := submit(20, "second", newTime)
	finish(newer, domain.AttemptSuccess, newTime)
	last := submit(30, "first", newTime.Add(time.Minute))
	finish(last, domain.AttemptUnknown, time.Time{})
	finish(old, domain.AttemptSuccess, f.time)

	got := f.delivery(t, true).Pairs[pair]
	if got.LastOutcome != domain.AttemptUnknown || got.ConfirmedVariant != "second" || !got.ConfirmedAt.Equal(newTime) {
		t.Fatalf("older or unresolved result replaced latest confirmed state: %+v", got)
	}
}

func TestPreflightRejectionAndVariantExhaustionAreVisible(t *testing.T) {
	f := newDeliveryFixture(t)
	f.save(t, "a", firstChat)
	var reads int
	transport := &controlledTelegram{
		check: func(domain.ChatID) telegram.DestinationCheck {
			reads++
			if reads > 1 {
				return telegram.DestinationCheck{State: telegram.DestinationAccessLost}
			}
			return telegram.DestinationCheck{State: telegram.DestinationVerified, Role: telegram.DestinationAdmin}
		},
		send: func(domain.ChatID, content.PreparedVariant, int64) telegram.SendResult {
			return telegram.SendResult{Outcome: domain.AttemptFailure, Reason: "telegram_rejected", CheckRights: true}
		},
	}
	f.step(t, transport)
	status, err := f.engine.Status(t.Context())
	if err != nil || len(status.DisabledChats) != 1 || len(status.Dispatch) != 1 || status.Dispatch[0].Outcome != domain.AttemptFailure {
		t.Fatalf("post-preflight rejection did not record the outcome and confirmed rights loss: %+v, %v", status, err)
	}

	f.save(t, "b", "group:13")
	snapshot, err := f.state.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	pub := snapshot.Publications[1]
	pub.Variants = nil
	if _, err := f.engine.SavePublication(t.Context(), snapshot.Revision, pub); err != nil {
		t.Fatal(err)
	}
	f.step(t, transport)
	status, err = f.engine.Status(t.Context())
	if err != nil || status.Publications[1].State != domain.Stopped || status.Publications[1].Reason != app.ReasonVariantsExhausted {
		t.Fatalf("exhausted publication was not paused with a reason: %+v, %v", status, err)
	}
}

func TestConfirmedRightsLossSurvivesUnrelatedIntervalChange(t *testing.T) {
	f := newDeliveryFixture(t)
	f.save(t, "a", firstChat)
	f.save(t, "b", firstChat)
	f.save(t, "unrelated", "group:13")
	ctx := t.Context()
	var sendsToDisabledChat int
	transport := &controlledTelegram{
		check: func(chat domain.ChatID) telegram.DestinationCheck {
			if chat == firstChat {
				if _, err := f.engine.Handler()(ctx, app.Command{
					Operation: app.ChangeInterval, PublicationID: "unrelated", Interval: 2 * time.Minute,
				}); err != nil {
					t.Fatal(err)
				}
				return telegram.DestinationCheck{State: telegram.DestinationAccessLost}
			}
			return telegram.DestinationCheck{State: telegram.DestinationVerified, Role: telegram.DestinationAdmin}
		},
		send: func(chat domain.ChatID, _ content.PreparedVariant, _ int64) telegram.SendResult {
			if chat == firstChat {
				sendsToDisabledChat++
			}
			return telegram.SendResult{Outcome: domain.AttemptSuccess, ConfirmedAt: f.time, MessageID: 1}
		},
	}

	f.step(t, transport)
	transport.check = nil
	f.step(t, transport)

	reopened, err := store.Open(ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	status, err := reopened.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.DisabledChats) != 1 || status.DisabledChats[0].ID != firstChat || sendsToDisabledChat != 0 {
		t.Fatalf("confirmed loss was discarded after an unrelated control: disabled=%v sends=%d",
			status.DisabledChats, sendsToDisabledChat)
	}

	f.engine.transport = transport
	if _, err := f.engine.Handler()(ctx, app.Command{Operation: app.ReenableChat, ChatID: firstChat}); err != nil {
		t.Fatal(err)
	}
	f.step(t, transport)
	if sendsToDisabledChat != 1 {
		t.Fatalf("verified re-enable did not restore dispatch: sent %d", sendsToDisabledChat)
	}
}

type stalledWriteTelegram struct {
	controlledTelegram
	conn    transport.Conn
	writing chan struct{}
}

func (f *stalledWriteTelegram) PrepareSend(context.Context, domain.ChatID, content.PreparedVariant) (telegram.PreparedSend, telegram.DestinationCheck) {
	return func(ctx context.Context, _ int64, handoff telegram.SendHandoff) telegram.SendResult {
		err := handoff(ctx, func(writeCtx context.Context) error {
			close(f.writing)
			return f.conn.Send(writeCtx, &bin.Buffer{Buf: []byte{1, 2, 3, 4}})
		})
		if err != nil {
			return telegram.SendResult{Outcome: domain.AttemptUnknown, Reason: "transport_write_interrupted"}
		}
		return telegram.SendResult{Outcome: domain.AttemptSuccess}
	}, telegram.DestinationCheck{State: telegram.DestinationVerified, Role: telegram.DestinationAdmin}
}

func TestBlockedMTProtoWriteLeavesTimeForStop(t *testing.T) {
	f := newDeliveryFixture(t)
	f.save(t, "a", firstChat)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client, server := transport.Intermediate.Pipe()
	defer client.Close()
	defer server.Close()
	transport := &stalledWriteTelegram{conn: client, writing: make(chan struct{})}
	dispatchDone := make(chan error, 1)
	go func() {
		_, _, err := f.engine.step(ctx, transport)
		dispatchDone <- err
	}()
	select {
	case <-transport.writing:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	stopCtx, cancelStop := context.WithTimeout(ctx, 5*time.Second)
	defer cancelStop()
	stopDone := make(chan error, 1)
	go func() {
		_, err := f.engine.Handler()(stopCtx, app.Command{Operation: app.Stop, PublicationID: "a"})
		stopDone <- err
	}()
	var stopErr error
	select {
	case stopErr = <-stopDone:
	case <-stopCtx.Done():
		server.Close()
		stopErr = <-stopDone
	}
	if err := <-dispatchDone; err != nil {
		t.Fatal(err)
	}
	status, err := f.state.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stopErr != nil || status.Publications[0].State != domain.Stopped {
		t.Fatalf("stalled write prevented Stop: state=%s error=%v", status.Publications[0].State, stopErr)
	}
	delivery := f.delivery(t, true).Pairs[store.Pair{PublicationID: "a", ChatID: firstChat}]
	if delivery.LastOutcome != domain.AttemptUnknown || !delivery.ConfirmedAt.IsZero() {
		t.Fatalf("stalled write lost or confirmed its pre-write attempt: %+v", delivery)
	}
}

func TestObservedPreflightEffectsSurviveCancellation(t *testing.T) {
	for _, name := range []string{"rights-loss", "preflight-wait", "source-wait"} {
		t.Run(name, func(t *testing.T) {
			f := newDeliveryFixture(t)
			f.save(t, "a", firstChat)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			transport := &controlledTelegram{}
			wait := &telegram.ServerWait{Duration: time.Minute}
			switch name {
			case "rights-loss":
				transport.check = func(domain.ChatID) telegram.DestinationCheck {
					cancel()
					return telegram.DestinationCheck{State: telegram.DestinationAccessLost}
				}
			case "preflight-wait":
				transport.check = func(domain.ChatID) telegram.DestinationCheck {
					cancel()
					return telegram.DestinationCheck{State: telegram.DestinationCheckUnavailable, Wait: wait}
				}
			case "source-wait":
				if _, err := f.state.BindAccount(ctx, 42); err != nil {
					t.Fatal(err)
				}
				snapshot, err := f.state.Snapshot(ctx)
				if err != nil {
					t.Fatal(err)
				}
				pub := snapshot.Publications[0]
				pub.Variants[0] = domain.Variant{ID: "saved", SavedMessage: &domain.SavedMessage{AccountID: 42, MessageID: 14}}
				if _, err := f.engine.SavePublication(ctx, snapshot.Revision, pub); err != nil {
					t.Fatal(err)
				}
				transport.resolve = func(domain.SavedMessage) (content.Kind, error) {
					cancel()
					return "", wait
				}
			}
			if _, _, err := f.engine.step(ctx, transport); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			reopened, err := store.Open(t.Context(), f.path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			status, err := reopened.Status(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			delivery, err := reopened.DeliveryState(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if name == "rights-loss" {
				if len(status.DisabledChats) != 1 || status.DisabledChats[0].ID != firstChat {
					t.Fatalf("confirmed rights loss was discarded: %+v", status.DisabledChats)
				}
			} else if !delivery.Waits[""].Equal(f.time.Add(wait.Duration)) {
				t.Fatalf("server wait was discarded: %v", delivery.Waits)
			}
			if len(delivery.Pairs) != 0 {
				t.Fatalf("canceled preflight registered a send: %+v", delivery.Pairs)
			}
		})
	}
}
