package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"telegram-messages-sender/internal/domain"
)

func TestDeliveryMigrationPreservesVersionOneConfiguration(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, initialSchema+`
		PRAGMA user_version = 1;
		UPDATE configuration SET account_id = 42, revision = 5;
		INSERT INTO publications VALUES ('original', 'enabled', 60000000000);
		INSERT INTO publication_destinations VALUES ('original', 'group:12', 0);
		INSERT INTO publication_variants VALUES ('original', 'original', 0, 'manual text', NULL, NULL);
		INSERT INTO disabled_chats VALUES ('group:13');`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	s := openState(t, path)
	snapshot, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.AccountID != 42 || snapshot.Revision != 5 || len(snapshot.Publications) != 1 ||
		len(snapshot.DisabledChats) != 1 || snapshot.DisabledChats[0] != "group:13" {
		t.Fatalf("migration changed existing configuration: %+v", snapshot)
	}
	pub := snapshot.Publications[0]
	if pub.ID != "original" || pub.State != domain.Enabled || pub.Interval != time.Minute ||
		len(pub.Destinations) != 1 || pub.Destinations[0] != "group:12" ||
		len(pub.Variants) != 1 || pub.Variants[0].Text != "manual text" {
		t.Fatalf("migration changed existing publication: %+v", pub)
	}
	now := time.Now()
	if _, err := s.BeginSubmission(ctx, snapshot.Revision, Submission{
		Pair:      Pair{PublicationID: pub.ID, ChatID: pub.Destinations[0]},
		VariantID: pub.Variants[0].ID, RandomID: 32, FirstDue: now, SubmittedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}
