package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"telegram-messages-sender/internal/app"
	"telegram-messages-sender/internal/domain"
)

func TestConfigurationSurvivesReopen(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "state with spaces?.db")
	state := openState(t, path)
	revision, err := state.BindAccount(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	first := domain.Publication{
		ID: "news", State: domain.Enabled, Interval: 20 * time.Minute,
		Destinations: []domain.ChatID{"supergroup:9", "group:9"},
		Variants: []domain.Variant{
			{ID: "source", SavedMessage: &domain.SavedMessage{AccountID: 42, MessageID: 321}},
		},
	}
	for i := range 16 {
		first.Variants = append(first.Variants, domain.Variant{ID: domain.VariantID(fmt.Sprintf("manual-%d", i)), Text: "example"})
	}
	for i := range 6 {
		first.Destinations = append(first.Destinations, domain.ChatID(fmt.Sprintf("group:%d", i+10)))
	}
	second := domain.Publication{
		ID: "updates", State: domain.Enabled, Interval: time.Minute,
		Destinations: []domain.ChatID{"supergroup:9"},
	}
	for _, publication := range []domain.Publication{second, first} {
		revision, err = state.SavePublication(ctx, revision, publication)
		if err != nil {
			t.Fatal(err)
		}
	}
	handler := app.NewHandler(state)
	if _, err := handler(ctx, app.Command{Operation: app.Stop, PublicationID: first.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := handler(ctx, app.Command{Operation: app.ChangeInterval, PublicationID: first.ID, Interval: 30 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	for _, chat := range []domain.ChatID{"supergroup:9", "group:77"} {
		if _, err := state.SetChatDisabled(ctx, chat, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := state.BindAccount(ctx, 43); !errors.Is(err, ErrAccountMismatch) {
		t.Fatalf("rebind to a different account: %v", err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openState(t, path)
	first.State = domain.Stopped
	first.Interval = 30 * time.Minute
	want := Snapshot{
		Revision: 7, AccountID: 42, Publications: []domain.Publication{first, second},
		DisabledChats: []domain.ChatID{"group:77", "supergroup:9"},
	}
	got, err := reopened.Snapshot(ctx)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened configuration = %+v, want %+v, err = %v", got, want, err)
	}
}

func TestFailedPublicationSaveRollsBackAllChanges(t *testing.T) {
	ctx := t.Context()
	state := openState(t, filepath.Join(t.TempDir(), "state.db"))
	original := domain.Publication{
		ID: "news", State: domain.Stopped, Interval: time.Minute,
		Destinations: []domain.ChatID{"group:12"},
		Variants:     []domain.Variant{{ID: "manual", Text: "original"}},
	}
	revision, err := state.SavePublication(ctx, 0, original)
	if err != nil {
		t.Fatal(err)
	}

	edit := domain.Publication{
		ID: original.ID, State: domain.Enabled, Interval: time.Second,
		Destinations: []domain.ChatID{"supergroup:9"},
		Variants: []domain.Variant{
			{ID: "new", Text: "replacement"},
			{ID: "foreign", SavedMessage: &domain.SavedMessage{AccountID: 43, MessageID: 12}},
		},
	}
	if got, err := state.SavePublication(ctx, revision, edit); err == nil || got != 0 {
		t.Fatalf("invalid reference save returned revision %d, error %v", got, err)
	}
	got, err := state.Snapshot(ctx)
	want := Snapshot{Revision: revision, Publications: []domain.Publication{original}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("configuration after rollback = %+v, want %+v, err = %v", got, want, err)
	}
}

func TestStalePublicationEditPreservesCommittedControls(t *testing.T) {
	ctx := t.Context()
	state := openState(t, filepath.Join(t.TempDir(), "state.db"))
	publication := domain.Publication{
		ID: "news", State: domain.Enabled, Interval: time.Minute,
		Variants: []domain.Variant{{ID: "manual", Text: "original"}},
	}
	if _, err := state.SavePublication(ctx, 0, publication); err != nil {
		t.Fatal(err)
	}
	stale, err := state.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	handler := app.NewHandler(state)
	if _, err := handler(ctx, app.Command{Operation: app.Stop, PublicationID: publication.ID}); err != nil {
		t.Fatal(err)
	}
	changed, err := handler(ctx, app.Command{Operation: app.ChangeInterval, PublicationID: publication.ID, Interval: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	edit := stale.Publications[0]
	edit.Variants[0].Text = "updated"
	if revision, err := state.SavePublication(ctx, stale.Revision, edit); revision != 0 || !errors.Is(err, ErrConfigurationChanged) {
		t.Fatalf("stale edit returned revision %d, error %v", revision, err)
	}
	want := publication
	want.State = domain.Stopped
	want.Interval = 2 * time.Minute
	current, err := state.Snapshot(ctx)
	if err != nil || !reflect.DeepEqual(current, Snapshot{Revision: changed.Revision, Publications: []domain.Publication{want}}) {
		t.Fatalf("configuration after rejected edit = %+v, err = %v", current, err)
	}
	edit = current.Publications[0]
	edit.Variants[0].Text = "updated"
	if revision, err := state.SavePublication(ctx, current.Revision, edit); err != nil || revision != current.Revision+1 {
		t.Fatalf("edit from current revision returned revision %d, error %v", revision, err)
	}
}

func TestCommitFailureDoesNotAcknowledgeControl(t *testing.T) {
	ctx := t.Context()
	state := openState(t, filepath.Join(t.TempDir(), "state.db"))
	revision, err := state.SavePublication(ctx, 0, domain.Publication{
		ID: "news", State: domain.Enabled, Interval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = state.db.ExecContext(ctx, `
		CREATE TABLE commit_guard (
			chat_id TEXT REFERENCES disabled_chats(chat_id) DEFERRABLE INITIALLY DEFERRED
		);
		CREATE TRIGGER fail_at_commit AFTER UPDATE OF revision ON configuration
		BEGIN
			INSERT INTO commit_guard (chat_id) VALUES ('missing');
		END;`)
	if err != nil {
		t.Fatal(err)
	}

	handler := app.NewHandler(state)
	result, err := handler(ctx, app.Command{Operation: app.Stop, PublicationID: "news"})
	if err == nil || result.Revision != 0 {
		t.Fatalf("stop with failing commit returned %+v, %v", result, err)
	}
	got, err := handler(ctx, app.Command{Operation: app.Status})
	if err != nil || got.Revision != revision || len(got.Publications) != 1 || got.Publications[0].State != domain.Enabled {
		t.Fatalf("status after failed commit = %+v, %v", got, err)
	}
}

func openState(t *testing.T, path string) *Store {
	t.Helper()
	state, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	return state
}
