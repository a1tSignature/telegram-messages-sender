package app

import (
	"context"
	"errors"
	"time"

	"telegram-messages-sender/internal/domain"
)

type Operation string

const (
	Status         Operation = "status"
	Start          Operation = "start"
	Stop           Operation = "stop"
	ChangeInterval Operation = "change-interval"
	ReenableChat   Operation = "re-enable-chat"
)

type Command struct {
	Operation     Operation            `json:"operation"`
	PublicationID domain.PublicationID `json:"publication_id,omitempty"`
	ChatID        domain.ChatID        `json:"chat_id,omitempty"`
	Interval      time.Duration        `json:"interval_ns,omitempty"`
}

type StatusReason string

const (
	ReasonManualStop        StatusReason = "manual_stop"
	ReasonVariantsExhausted StatusReason = "variants_exhausted"
	ReasonChatDisabled      StatusReason = "chat_disabled"
)

type DisabledChatStatus struct {
	ID     domain.ChatID `json:"id"`
	Reason StatusReason  `json:"reason"`
}

type PublicationStatus struct {
	ID           domain.PublicationID    `json:"id"`
	State        domain.PublicationState `json:"state"`
	Reason       StatusReason            `json:"reason,omitempty"`
	Interval     time.Duration           `json:"interval_ns"`
	Destinations []domain.ChatID         `json:"destinations"`
}

type DispatchStatus struct {
	PublicationID domain.PublicationID  `json:"publication_id"`
	ChatID        domain.ChatID         `json:"chat_id"`
	DueAt         *time.Time            `json:"due_at,omitempty"`
	Delay         time.Duration         `json:"delay_ns"`
	Outcome       domain.AttemptOutcome `json:"outcome,omitempty"`
	Unresolved    int                   `json:"unresolved,omitempty"`
	Reason        string                `json:"reason,omitempty"`
}

type WaitStatus struct {
	ChatID domain.ChatID `json:"chat_id,omitempty"`
	Until  time.Time     `json:"until"`
}

type SessionStatus struct {
	AccountID          domain.AccountID `json:"account_id"`
	DisplayName        string           `json:"display_name,omitempty"`
	TransportAvailable bool             `json:"transport_available"`
}

type Result struct {
	Session       *SessionStatus       `json:"session,omitempty"`
	Waits         []WaitStatus         `json:"waits,omitempty"`
	Dispatch      []DispatchStatus     `json:"dispatch,omitempty"`
	DispatchError string               `json:"dispatch_error,omitempty"`
	Revision      int64                `json:"revision"`
	Publications  []PublicationStatus  `json:"publications,omitempty"`
	DisabledChats []DisabledChatStatus `json:"disabled_chats,omitempty"`
}

type Handler func(context.Context, Command) (Result, error)

var (
	ErrInvalidCommand       = errors.New("invalid control command")
	ErrUnknownPublication   = errors.New("publication not found")
	ErrVerificationRequired = errors.New("chat must be verified before re-enable")
)

type Configuration interface {
	Status(context.Context) (Result, error)
	SetPublicationState(context.Context, domain.PublicationID, domain.PublicationState) (int64, error)
	SetInterval(context.Context, domain.PublicationID, time.Duration) (int64, error)
}

func NewHandler(config Configuration) Handler {
	return func(ctx context.Context, command Command) (Result, error) {
		switch command.Operation {
		case Status:
			if command.PublicationID == "" && command.ChatID == "" && command.Interval == 0 {
				return config.Status(ctx)
			}
		case Start, Stop:
			if command.PublicationID != "" && command.ChatID == "" && command.Interval == 0 {
				state := domain.Enabled
				if command.Operation == Stop {
					state = domain.Stopped
				}
				revision, err := config.SetPublicationState(ctx, command.PublicationID, state)
				return Result{Revision: revision}, err
			}
		case ChangeInterval:
			if command.PublicationID != "" && command.ChatID == "" && command.Interval > 0 {
				revision, err := config.SetInterval(ctx, command.PublicationID, command.Interval)
				return Result{Revision: revision}, err
			}
		case ReenableChat:
			if command.ChatID != "" && command.PublicationID == "" && command.Interval == 0 {
				return Result{}, ErrVerificationRequired
			}
		}
		return Result{}, ErrInvalidCommand
	}
}
