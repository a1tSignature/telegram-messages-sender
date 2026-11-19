package telegram

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"telegram-messages-sender/internal/domain"
)

var errInvalidDifference = errors.New("Telegram update difference returned an invalid checkpoint")

type recoveryAPI interface {
	UpdatesGetState(context.Context) (*tg.UpdatesState, error)
	UpdatesGetDifference(context.Context, *tg.UpdatesGetDifferenceRequest) (tg.UpdatesDifferenceClass, error)
}

func (c *Connection) Recover(ctx context.Context, checkpoint domain.UpdateCheckpoint, refs []domain.RecoveryReference) (domain.RecoveryBatch, error) {
	if !c.Status().TransportAvailable {
		return domain.RecoveryBatch{}, ErrTransport
	}
	return recoverDifference(ctx, c.api, c.recoveredMessage, checkpoint, refs)
}

func recoverDifference(ctx context.Context, api recoveryAPI, lookup func(context.Context, domain.RecoveryReference) (time.Time, error), checkpoint domain.UpdateCheckpoint, refs []domain.RecoveryReference) (domain.RecoveryBatch, error) {
	result := domain.RecoveryBatch{Checkpoint: checkpoint}
	var updates []tg.UpdateClass
	var messages []tg.MessageClass
	if checkpoint.Date == 0 {
		state, err := api.UpdatesGetState(ctx)
		if err != nil {
			return result, recoveryError(ctx, err)
		}
		if state == nil || state.Date <= 0 {
			return result, errInvalidDifference
		}
		result.Checkpoint = updateCheckpoint(*state)
	} else {
		request := &tg.UpdatesGetDifferenceRequest{Pts: checkpoint.PTS, Qts: checkpoint.QTS, Date: checkpoint.Date}
		request.SetPtsLimit(500)
		response, err := api.UpdatesGetDifference(ctx, request)
		if err != nil {
			return result, recoveryError(ctx, err)
		}
		switch difference := response.(type) {
		case *tg.UpdatesDifferenceEmpty:
			result.Checkpoint.Date, result.Checkpoint.Seq = difference.Date, difference.Seq
		case *tg.UpdatesDifference:
			result.Checkpoint = updateCheckpoint(difference.State)
			updates, messages = difference.OtherUpdates, difference.NewMessages
		case *tg.UpdatesDifferenceSlice:
			result.Checkpoint = updateCheckpoint(difference.IntermediateState)
			result.More = true
			updates, messages = difference.OtherUpdates, difference.NewMessages
		case *tg.UpdatesDifferenceTooLong:
			result.Checkpoint.PTS = difference.Pts
			result.More = true
		default:
			return result, errInvalidDifference
		}
	}
	if result.Checkpoint.Date <= 0 || result.Checkpoint.PTS < checkpoint.PTS ||
		result.Checkpoint.QTS < checkpoint.QTS || result.Checkpoint.Date < checkpoint.Date ||
		(result.More && result.Checkpoint == checkpoint) {
		return result, errInvalidDifference
	}

	mapped := make(map[int64]int)
	for _, update := range updates {
		switch update := update.(type) {
		case *tg.UpdateMessageID:
			if update.RandomID != 0 && update.ID > 0 {
				if prior, exists := mapped[update.RandomID]; exists && prior != update.ID {
					mapped[update.RandomID] = -1
				} else {
					mapped[update.RandomID] = update.ID
				}
			}
		case *tg.UpdateNewMessage:
			messages = append(messages, update.Message)
		case *tg.UpdateNewChannelMessage:
			messages = append(messages, update.Message)
		}
	}
	for _, ref := range refs {
		mappedID := mapped[ref.RandomID]
		if mappedID < 0 || (mappedID > 0 && ref.MessageID > 0 && mappedID != ref.MessageID) {
			continue
		}
		if ref.MessageID == 0 {
			ref.MessageID = mappedID
		}
		if ref.MessageID == 0 {
			continue
		}
		sent := matchingMessageDate(ref, messages)
		if sent.IsZero() && !result.More {
			var err error
			sent, err = lookup(ctx, ref)
			if err != nil {
				return domain.RecoveryBatch{Checkpoint: checkpoint}, err
			}
		}
		result.Evidence = append(result.Evidence, domain.RecoveryEvidence{RecoveryReference: ref, SentAt: sent})
	}
	return result, nil
}

func recoveryError(ctx context.Context, err error) error {
	if wait := waitFromError(err); wait != nil {
		wait.ChatScoped = false
		return wait
	}
	return connectionError(ctx, err)
}

func updateCheckpoint(state tg.UpdatesState) domain.UpdateCheckpoint {
	return domain.UpdateCheckpoint{PTS: state.Pts, QTS: state.Qts, Date: state.Date, Seq: state.Seq}
}

func (c *Connection) recoveredMessage(ctx context.Context, ref domain.RecoveryReference) (time.Time, error) {
	peer, _, err := c.resolveDestination(ctx, ref.ChatID)
	if err != nil {
		return time.Time{}, recoveryError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	ids := []tg.InputMessageClass{&tg.InputMessageID{ID: ref.MessageID}}
	var response tg.MessagesMessagesClass
	switch peer := peer.(type) {
	case *tg.InputPeerChat:
		response, err = c.api.MessagesGetMessages(ctx, ids)
	case *tg.InputPeerChannel:
		response, err = c.api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: peer.ChannelID, AccessHash: peer.AccessHash}, ID: ids,
		})
	default:
		return time.Time{}, nil
	}
	if err != nil {
		if ctx.Err() == nil && tgerr.Is(err, "MESSAGE_ID_INVALID", "MSG_ID_INVALID") {
			return time.Time{}, nil
		}
		return time.Time{}, recoveryError(ctx, err)
	}
	if channel, ok := response.(*tg.MessagesChannelMessages); ok {
		if _, isChannel := peer.(*tg.InputPeerChannel); isChannel {
			return matchingMessageDate(ref, channel.Messages), nil
		}
	}
	messages, ok := savedResults(response)
	if !ok {
		return time.Time{}, ErrTransport
	}
	return matchingMessageDate(ref, messages), nil
}

func matchingMessageDate(ref domain.RecoveryReference, messages []tg.MessageClass) time.Time {
	for _, item := range messages {
		message, ok := item.(*tg.Message)
		if ok && message.Out && message.Date > 0 && message.ID == ref.MessageID && recoveryChatID(message.PeerID) == ref.ChatID {
			return time.Unix(int64(message.Date), 0)
		}
	}
	return time.Time{}
}

func recoveryChatID(peer tg.PeerClass) domain.ChatID {
	switch peer := peer.(type) {
	case *tg.PeerChat:
		return domain.ChatID(fmt.Sprintf("group:%d", peer.ChatID))
	case *tg.PeerChannel:
		return domain.ChatID(fmt.Sprintf("supergroup:%d", peer.ChannelID))
	default:
		return ""
	}
}
