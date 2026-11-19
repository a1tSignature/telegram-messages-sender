package domain

import "time"

type UpdateCheckpoint struct {
	PTS  int
	QTS  int
	Date int
	Seq  int
}

type RecoveryReference struct {
	RandomID  int64
	ChatID    ChatID
	MessageID int
}

type RecoveryEvidence struct {
	RecoveryReference
	SentAt time.Time
}

type RecoveryBatch struct {
	Checkpoint UpdateCheckpoint
	Evidence   []RecoveryEvidence
	More       bool
}
