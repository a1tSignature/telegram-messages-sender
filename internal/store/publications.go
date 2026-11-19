package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"telegram-messages-sender/internal/app"
	"telegram-messages-sender/internal/domain"
)

var (
	ErrAccountMismatch      = errors.New("configuration is bound to a different account")
	ErrConfigurationChanged = errors.New("configuration changed since it was read")
)

type Snapshot struct {
	Revision      int64
	AccountID     domain.AccountID
	Publications  []domain.Publication
	DisabledChats []domain.ChatID
	Paused        map[domain.PublicationID]app.StatusReason
}

func (s *Store) BoundAccount(ctx context.Context) (domain.AccountID, error) {
	var id domain.AccountID
	err := s.db.QueryRowContext(ctx, "SELECT coalesce(account_id, 0) FROM configuration").Scan(&id)
	return id, err
}

func (s *Store) BindAccount(ctx context.Context, id domain.AccountID) (int64, error) {
	return s.change(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE configuration SET account_id = ?
			WHERE account_id IS NULL OR account_id = ?`, id, id)
		if err != nil {
			return err
		}
		return requireUpdated(result, ErrAccountMismatch)
	})
}

func (s *Store) SavePublication(ctx context.Context, expectedRevision int64, publication domain.Publication) (int64, error) {
	return s.change(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			INSERT INTO publications (id, state, interval_ns)
			SELECT ?, ?, ? FROM configuration WHERE revision = ?
			ON CONFLICT (id) DO UPDATE SET state = excluded.state, interval_ns = excluded.interval_ns,
			pause_reason = CASE WHEN excluded.state = 'enabled' THEN '' ELSE publications.pause_reason END`,
			publication.ID, publication.State, int64(publication.Interval), expectedRevision)
		if err != nil {
			return err
		}
		if err := requireUpdated(result, ErrConfigurationChanged); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM publication_destinations WHERE publication_id = ?", publication.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM publication_variants WHERE publication_id = ?", publication.ID); err != nil {
			return err
		}
		for position, chat := range publication.Destinations {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO publication_destinations (publication_id, chat_id, position) VALUES (?, ?, ?)`,
				publication.ID, chat, position); err != nil {
				return err
			}
		}
		for position, variant := range publication.Variants {
			var text, accountID, messageID any
			switch {
			case variant.SavedMessage == nil:
				text = variant.Text
			case variant.Text == "":
				accountID = variant.SavedMessage.AccountID
				messageID = variant.SavedMessage.MessageID
			default:
				return errors.New("variant must have one content source")
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO publication_variants
					(publication_id, id, position, manual_text, source_account_id, source_message_id)
				VALUES (?, ?, ?, ?, ?, ?)`,
				publication.ID, variant.ID, position, text, accountID, messageID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) SetPublicationState(ctx context.Context, id domain.PublicationID, state domain.PublicationState) (int64, error) {
	return s.updatePublication(ctx, "UPDATE publications SET state = ?, pause_reason = '' WHERE id = ?", state, id)
}

func (s *Store) SetInterval(ctx context.Context, id domain.PublicationID, interval time.Duration) (int64, error) {
	return s.updatePublication(ctx, "UPDATE publications SET interval_ns = ? WHERE id = ?", int64(interval), id)
}

func (s *Store) updatePublication(ctx context.Context, query string, value any, id domain.PublicationID) (int64, error) {
	return s.change(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, query, value, id)
		if err != nil {
			return err
		}
		return requireUpdated(result, app.ErrUnknownPublication)
	})
}

func (s *Store) SetChatDisabled(ctx context.Context, chat domain.ChatID, disabled bool) (int64, error) {
	return s.change(ctx, func(tx *sql.Tx) error {
		query := "DELETE FROM disabled_chats WHERE chat_id = ?"
		if disabled {
			query = "INSERT INTO disabled_chats (chat_id) VALUES (?) ON CONFLICT DO NOTHING"
		}
		_, err := tx.ExecContext(ctx, query, chat)
		return err
	})
}

func requireUpdated(result sql.Result, missing error) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return missing
	}
	return nil
}

func (s *Store) change(ctx context.Context, apply func(*sql.Tx) error) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := apply(tx); err != nil {
		return 0, err
	}
	var revision int64
	err = tx.QueryRowContext(ctx, "UPDATE configuration SET revision = revision + 1 RETURNING revision").Scan(&revision)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return revision, nil
}

func (s *Store) Snapshot(ctx context.Context) (Snapshot, error) {
	return s.readSnapshot(ctx, true)
}

func (s *Store) Status(ctx context.Context) (app.Result, error) {
	state, err := s.readSnapshot(ctx, false)
	if err != nil {
		return app.Result{}, err
	}
	result := app.Result{Revision: state.Revision}
	for _, id := range state.DisabledChats {
		result.DisabledChats = append(result.DisabledChats, app.DisabledChatStatus{
			ID: id, Reason: app.ReasonChatDisabled,
		})
	}
	for _, publication := range state.Publications {
		var reason app.StatusReason
		if publication.State == domain.Stopped {
			reason = state.Paused[publication.ID]
			if reason == "" {
				reason = app.ReasonManualStop
			}
		}
		result.Publications = append(result.Publications, app.PublicationStatus{
			ID:           publication.ID,
			State:        publication.State,
			Reason:       reason,
			Interval:     publication.Interval,
			Destinations: publication.Destinations,
		})
	}
	return result, nil
}

func (s *Store) readSnapshot(ctx context.Context, includeVariants bool) (Snapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Snapshot{}, err
	}
	defer tx.Rollback()

	var state Snapshot
	err = tx.QueryRowContext(ctx, "SELECT revision, coalesce(account_id, 0) FROM configuration").
		Scan(&state.Revision, &state.AccountID)
	if err != nil {
		return Snapshot{}, err
	}

	indices := make(map[domain.PublicationID]int)
	err = scanRows(ctx, tx, "SELECT id, state, interval_ns, pause_reason FROM publications ORDER BY id", func(rows *sql.Rows) error {
		var publication domain.Publication
		var reason app.StatusReason
		if err := rows.Scan(&publication.ID, &publication.State, &publication.Interval, &reason); err != nil {
			return err
		}
		if reason != "" {
			if state.Paused == nil {
				state.Paused = make(map[domain.PublicationID]app.StatusReason)
			}
			state.Paused[publication.ID] = reason
		}
		indices[publication.ID] = len(state.Publications)
		state.Publications = append(state.Publications, publication)
		return nil
	})
	if err != nil {
		return Snapshot{}, err
	}

	err = scanRows(ctx, tx, `
		SELECT publication_id, chat_id FROM publication_destinations ORDER BY publication_id, position`, func(rows *sql.Rows) error {
		var id domain.PublicationID
		var chat domain.ChatID
		if err := rows.Scan(&id, &chat); err != nil {
			return err
		}
		publication := &state.Publications[indices[id]]
		publication.Destinations = append(publication.Destinations, chat)
		return nil
	})
	if err != nil {
		return Snapshot{}, err
	}

	if includeVariants {
		err = scanRows(ctx, tx, `
			SELECT publication_id, id, manual_text,
			       coalesce(source_account_id, 0), coalesce(source_message_id, 0)
			FROM publication_variants ORDER BY publication_id, position`, func(rows *sql.Rows) error {
			var id domain.PublicationID
			var variant domain.Variant
			var source domain.SavedMessage
			var text sql.NullString
			if err := rows.Scan(&id, &variant.ID, &text, &source.AccountID, &source.MessageID); err != nil {
				return err
			}
			if text.Valid {
				variant.Text = text.String
			} else {
				variant.SavedMessage = &source
			}
			publication := &state.Publications[indices[id]]
			publication.Variants = append(publication.Variants, variant)
			return nil
		})
		if err != nil {
			return Snapshot{}, err
		}
	}

	err = scanRows(ctx, tx, "SELECT chat_id FROM disabled_chats ORDER BY chat_id", func(rows *sql.Rows) error {
		var chat domain.ChatID
		if err := rows.Scan(&chat); err != nil {
			return err
		}
		state.DisabledChats = append(state.DisabledChats, chat)
		return nil
	})
	if err != nil {
		return Snapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return Snapshot{}, err
	}
	return state, nil
}

func scanRows(ctx context.Context, tx *sql.Tx, query string, scan func(*sql.Rows) error) error {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return fmt.Errorf("read configuration row: %w", err)
		}
	}
	return rows.Err()
}
