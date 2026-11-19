package domain

import "time"

type AccountID int64

type PublicationID string

type VariantID string

// ChatID includes the peer kind so a basic group and a supergroup can share a numeric ID.
type ChatID string

type SavedMessage struct {
	AccountID AccountID `json:"account_id"`
	MessageID int64     `json:"message_id"`
}

type Variant struct {
	ID           VariantID
	Text         string
	SavedMessage *SavedMessage
}

type PublicationState string

const (
	Enabled PublicationState = "enabled"
	Stopped PublicationState = "stopped"
)

type Publication struct {
	ID           PublicationID
	State        PublicationState
	Interval     time.Duration
	Destinations []ChatID
	Variants     []Variant
}

type AttemptOutcome string

const (
	AttemptPending AttemptOutcome = "pending"
	AttemptSuccess AttemptOutcome = "success"
	AttemptFailure AttemptOutcome = "failure"
	AttemptWait    AttemptOutcome = "server_wait"
	AttemptUnknown AttemptOutcome = "unknown"
)

type Attempt struct {
	ID            string
	PublicationID PublicationID
	ChatID        ChatID
	VariantID     VariantID
	Outcome       AttemptOutcome
}
