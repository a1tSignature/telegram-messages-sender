package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"telegram-messages-sender/internal/app"
	"telegram-messages-sender/internal/domain"
)

type Pair struct {
	PublicationID domain.PublicationID
	ChatID        domain.ChatID
}

type PairDelivery struct {
	FirstDue         time.Time
	ConfirmedAt      time.Time
	ConfirmedVariant domain.VariantID
	LastOutcome      domain.AttemptOutcome
	LastCompleted    time.Time
	Reason           string
	Unresolved       int
}

type DeliveryState struct {
	Pairs map[Pair]PairDelivery
	Waits map[domain.ChatID]time.Time
}

type Submission struct {
	Pair        Pair
	VariantID   domain.VariantID
	RandomID    int64
	FirstDue    time.Time
	SubmittedAt time.Time
}

type DeliveryResult struct {
	Outcome     domain.AttemptOutcome
	CompletedAt time.Time
	ConfirmedAt time.Time
	MessageID   int
	Reason      string
	WaitUntil   time.Time
	WaitChat    domain.ChatID
	DisableChat bool
}

func (s *Store) DeliveryState(ctx context.Context) (DeliveryState, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DeliveryState{}, err
	}
	defer tx.Rollback()
	result := DeliveryState{Pairs: make(map[Pair]PairDelivery), Waits: make(map[domain.ChatID]time.Time)}
	err = scanRows(ctx, tx, `
		SELECT p.publication_id, p.chat_id, p.first_due_ns, p.confirmed_ns,
		       coalesce(p.confirmed_variant, ''), coalesce(a.outcome, ''), a.completed_ns, coalesce(a.reason, ''),
		       (SELECT count(*) FROM delivery_attempts u WHERE u.publication_id = p.publication_id
		          AND u.chat_id = p.chat_id AND u.outcome IN ('pending', 'unknown'))
		FROM delivery_pairs p
		LEFT JOIN delivery_attempts a ON a.id = (
		    SELECT id FROM delivery_attempts WHERE publication_id = p.publication_id AND chat_id = p.chat_id
		    ORDER BY id DESC LIMIT 1)`, func(rows *sql.Rows) error {
		var pair Pair
		var delivery PairDelivery
		var first int64
		var confirmed, completed sql.NullInt64
		if err := rows.Scan(&pair.PublicationID, &pair.ChatID, &first, &confirmed, &delivery.ConfirmedVariant, &delivery.LastOutcome, &completed, &delivery.Reason, &delivery.Unresolved); err != nil {
			return err
		}
		delivery.FirstDue = time.Unix(0, first)
		if confirmed.Valid {
			delivery.ConfirmedAt = time.Unix(0, confirmed.Int64)
		}
		if completed.Valid {
			delivery.LastCompleted = time.Unix(0, completed.Int64)
		}
		result.Pairs[pair] = delivery
		return nil
	})
	if err != nil {
		return DeliveryState{}, err
	}
	err = scanRows(ctx, tx, "SELECT chat_id, until_ns FROM delivery_waits", func(rows *sql.Rows) error {
		var chat domain.ChatID
		var until int64
		if err := rows.Scan(&chat, &until); err != nil {
			return err
		}
		result.Waits[chat] = time.Unix(0, until)
		return nil
	})
	if err != nil {
		return DeliveryState{}, err
	}
	return result, tx.Commit()
}

func (s *Store) BeginSubmission(ctx context.Context, revision int64, send Submission) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var current int64
	if err := tx.QueryRowContext(ctx, "SELECT revision FROM configuration").Scan(&current); err != nil {
		return 0, err
	}
	if current != revision {
		return 0, ErrConfigurationChanged
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO delivery_pairs (publication_id, chat_id, first_due_ns) VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING`, send.Pair.PublicationID, send.Pair.ChatID, send.FirstDue.UnixNano()); err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO delivery_attempts (random_id, publication_id, chat_id, variant_id, submitted_ns, outcome)
		SELECT ?, p.id, ?, ?, ?, 'pending' FROM publications p
		JOIN publication_destinations d ON d.publication_id = p.id AND d.chat_id = ?
		JOIN publication_variants v ON v.publication_id = p.id AND v.id = ?
		WHERE p.id = ? AND p.state = 'enabled'
		  AND NOT EXISTS (SELECT 1 FROM disabled_chats WHERE chat_id = d.chat_id)
		  AND NOT EXISTS (SELECT 1 FROM delivery_waits WHERE chat_id IN ('', d.chat_id) AND until_ns > ?)
		  AND NOT EXISTS (SELECT 1 FROM delivery_attempts
		                  WHERE publication_id = p.id AND chat_id = d.chat_id AND outcome = 'pending')
		RETURNING id`, send.RandomID, send.Pair.ChatID, send.VariantID, send.SubmittedAt.UnixNano(),
		send.Pair.ChatID, send.VariantID, send.Pair.PublicationID, send.SubmittedAt.UnixNano()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrConfigurationChanged
	}
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) FinishSubmission(ctx context.Context, id int64, result DeliveryResult) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := finishSubmission(ctx, tx, id, result); err != nil {
		return err
	}
	return tx.Commit()
}

func finishSubmission(ctx context.Context, tx *sql.Tx, id int64, result DeliveryResult) error {
	var pair Pair
	var variant domain.VariantID
	var submitted int64
	err := tx.QueryRowContext(ctx, `
		UPDATE delivery_attempts SET outcome = ?, completed_ns = ?, telegram_message_id = ?, reason = ?
		WHERE id = ? AND (outcome = 'pending' OR (outcome = 'unknown' AND ? = 'success'))
		RETURNING publication_id, chat_id, variant_id, submitted_ns`, result.Outcome, result.CompletedAt.UnixNano(),
		result.MessageID, result.Reason, id, result.Outcome).Scan(&pair.PublicationID, &pair.ChatID, &variant, &submitted)
	if err != nil {
		return err
	}

	if result.Outcome == domain.AttemptSuccess {
		// Telegram timestamps have second precision. A send cannot precede its handoff.
		confirmed := max(result.ConfirmedAt.UnixNano(), submitted)
		if _, err := tx.ExecContext(ctx, `
			UPDATE delivery_pairs SET confirmed_ns = ?, confirmed_variant = ?, confirmed_attempt = ?
			WHERE publication_id = ? AND chat_id = ? AND
			      (confirmed_ns IS NULL OR (confirmed_ns, confirmed_attempt) < (?, ?))`,
			confirmed, variant, id, pair.PublicationID, pair.ChatID, confirmed, id); err != nil {
			return err
		}
	}
	if !result.WaitUntil.IsZero() {
		if err := persistWait(ctx, tx, result.WaitChat, result.WaitUntil); err != nil {
			return err
		}
	}
	if result.DisableChat {
		if _, err := tx.ExecContext(ctx, "INSERT INTO disabled_chats (chat_id) VALUES (?) ON CONFLICT DO NOTHING", pair.ChatID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE configuration SET revision = revision + 1"); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SetDeliveryWait(ctx context.Context, chat domain.ChatID, until time.Time) error {
	return persistWait(ctx, s.db, chat, until)
}

type executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func persistWait(ctx context.Context, exec executor, chat domain.ChatID, until time.Time) error {
	_, err := exec.ExecContext(ctx, `
		INSERT INTO delivery_waits (chat_id, until_ns) VALUES (?, ?)
		ON CONFLICT (chat_id) DO UPDATE SET until_ns = max(until_ns, excluded.until_ns)`, chat, until.UnixNano())
	return err
}

func (s *Store) PausePublication(ctx context.Context, revision int64, id domain.PublicationID) error {
	_, err := s.change(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE publications SET state = 'stopped', pause_reason = ?
			WHERE id = ? AND state = 'enabled' AND (SELECT revision FROM configuration) = ?`,
			app.ReasonVariantsExhausted, id, revision)
		if err != nil {
			return err
		}
		return requireUpdated(result, ErrConfigurationChanged)
	})
	return err
}
