package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"telegram-messages-sender/internal/domain"
)

var ErrCheckpointChanged = errors.New("Telegram update checkpoint changed")

func (s *Store) RecoveryState(ctx context.Context, account domain.AccountID) (domain.UpdateCheckpoint, []domain.RecoveryReference, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.UpdateCheckpoint{}, nil, err
	}
	defer tx.Rollback()
	if err := checkBoundAccount(ctx, tx, account); err != nil {
		return domain.UpdateCheckpoint{}, nil, err
	}
	checkpoint, err := readCheckpoint(ctx, tx, account)
	if err != nil {
		return checkpoint, nil, err
	}
	var refs []domain.RecoveryReference
	err = scanRows(ctx, tx, `
		SELECT random_id, chat_id, coalesce(telegram_message_id, 0)
		FROM delivery_attempts WHERE outcome IN ('pending', 'unknown')`, func(rows *sql.Rows) error {
		var ref domain.RecoveryReference
		if err := rows.Scan(&ref.RandomID, &ref.ChatID, &ref.MessageID); err != nil {
			return err
		}
		refs = append(refs, ref)
		return nil
	})
	if err != nil {
		return checkpoint, nil, err
	}
	return checkpoint, refs, tx.Commit()
}

func (s *Store) ApplyRecovery(ctx context.Context, account domain.AccountID, previous domain.UpdateCheckpoint, batch domain.RecoveryBatch, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkBoundAccount(ctx, tx, account); err != nil {
		return err
	}
	current, err := readCheckpoint(ctx, tx, account)
	if err != nil {
		return err
	}
	if current != previous {
		if current == batch.Checkpoint {
			return nil
		}
		return ErrCheckpointChanged
	}
	for _, item := range batch.Evidence {
		if err := applyRecoveryEvidence(ctx, tx, item, now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO update_checkpoint (account_id, pts, qts, date, seq) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (account_id) DO UPDATE SET pts = excluded.pts, qts = excluded.qts, date = excluded.date, seq = excluded.seq`,
		account, batch.Checkpoint.PTS, batch.Checkpoint.QTS, batch.Checkpoint.Date, batch.Checkpoint.Seq); err != nil {
		return err
	}
	if !batch.More {
		if _, err := tx.ExecContext(ctx, `
			UPDATE delivery_attempts SET outcome = 'unknown', completed_ns = ?, reason = 'no_delivery_confirmation'
			WHERE outcome = 'pending'`, now.UnixNano()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func applyRecoveryEvidence(ctx context.Context, tx *sql.Tx, found domain.RecoveryEvidence, now time.Time) error {
	if found.RandomID == 0 || found.MessageID <= 0 || found.ChatID == "" {
		return nil
	}
	var id int64
	err := tx.QueryRowContext(ctx, `
		UPDATE delivery_attempts SET telegram_message_id = ?
		WHERE random_id = ? AND chat_id = ? AND outcome IN ('pending', 'unknown')
		  AND coalesce(telegram_message_id, 0) IN (0, ?)
		RETURNING id`, found.MessageID, found.RandomID, found.ChatID, found.MessageID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil || found.SentAt.IsZero() {
		return err
	}
	return finishSubmission(ctx, tx, id, DeliveryResult{
		Outcome: domain.AttemptSuccess, CompletedAt: now, ConfirmedAt: found.SentAt, MessageID: found.MessageID,
	})
}

func readCheckpoint(ctx context.Context, tx *sql.Tx, account domain.AccountID) (domain.UpdateCheckpoint, error) {
	var checkpoint domain.UpdateCheckpoint
	err := tx.QueryRowContext(ctx, "SELECT pts, qts, date, seq FROM update_checkpoint WHERE account_id = ?", account).
		Scan(&checkpoint.PTS, &checkpoint.QTS, &checkpoint.Date, &checkpoint.Seq)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return checkpoint, err
}

func checkBoundAccount(ctx context.Context, tx *sql.Tx, account domain.AccountID) error {
	var bound sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT account_id FROM configuration").Scan(&bound); err != nil {
		return err
	}
	if !bound.Valid || domain.AccountID(bound.Int64) != account {
		return ErrAccountMismatch
	}
	return nil
}
