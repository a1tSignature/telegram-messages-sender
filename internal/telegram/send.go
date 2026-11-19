package telegram

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/telegram/query/dialogs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/gotd/td/transport"

	"telegram-messages-sender/internal/content"
	"telegram-messages-sender/internal/domain"
)

type ServerWait struct {
	Duration   time.Duration
	ChatScoped bool
}

func (ServerWait) Error() string { return "Telegram requested a wait" }

func waitFromError(err error) *ServerWait {
	rpc, ok := tgerr.As(err)
	if !ok || (rpc.Code != 420 && !strings.HasSuffix(rpc.Type, "_WAIT")) {
		return nil
	}
	seconds := min(max(int64(rpc.Argument), 1), math.MaxInt32)
	return &ServerWait{Duration: time.Duration(seconds) * time.Second, ChatScoped: rpc.Type == "SLOWMODE_WAIT"}
}

type SendResult struct {
	Outcome     domain.AttemptOutcome
	ConfirmedAt time.Time
	MessageID   int
	Reason      string
	Wait        *ServerWait
	CheckRights bool
}

// SendHandoff authorizes and executes the transport write before returning.
type SendHandoff func(ctx context.Context, write func(context.Context) error) error

type PreparedSend func(ctx context.Context, randomID int64, handoff SendHandoff) SendResult

func (c *Connection) VerifyDestination(ctx context.Context, chat domain.ChatID, kind content.Kind) DestinationCheck {
	generation := c.generation.Load()
	_, result := c.destination(ctx, chat, kind)
	if result.State == DestinationVerified && c.generation.Load() != generation {
		return DestinationCheck{State: DestinationCheckUnavailable}
	}
	return result
}

func (c *Connection) PrepareSend(ctx context.Context, chat domain.ChatID, variant content.PreparedVariant) (PreparedSend, DestinationCheck) {
	generation := c.generation.Load()
	peer, check := c.destination(ctx, chat, variant.Kind)
	if check.State != DestinationVerified {
		return nil, check
	}
	return func(ctx context.Context, randomID int64, handoff SendHandoff) SendResult {
		if !c.Status().TransportAvailable {
			return SendResult{Outcome: domain.AttemptUnknown, Reason: "transport_unavailable"}
		}
		return sendOnce(ctx, c.api.Invoker(), peer, check.SendAs, variant, randomID, func(ctx context.Context, write func(context.Context) error) error {
			if !c.Status().TransportAvailable || c.generation.Load() != generation {
				return ErrTransport
			}
			return handoff(ctx, func(writeCtx context.Context) error {
				if !c.Status().TransportAvailable || c.generation.Load() != generation {
					return ErrTransport
				}
				return write(writeCtx)
			})
		})
	}, check
}

func (c *Connection) destination(ctx context.Context, chat domain.ChatID, kind content.Kind) (tg.InputPeerClass, DestinationCheck) {
	if !c.Status().TransportAvailable {
		return nil, DestinationCheck{State: DestinationCheckUnavailable}
	}
	peer, state, err := c.resolveDestination(ctx, chat)
	if peer == nil {
		return nil, DestinationCheck{State: state, Wait: waitFromError(err)}
	}
	check := c.CheckDestination(ctx, peer, kind)
	if check.State == DestinationCheckUnavailable {
		c.sendPeers.Delete(chat)
	}
	return peer, check
}

func (c *Connection) resolveDestination(ctx context.Context, chat domain.ChatID) (tg.InputPeerClass, DestinationState, error) {
	prefix, idText, _ := strings.Cut(string(chat), ":")
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != idText {
		return nil, DestinationUnsupported, nil
	}
	switch prefix {
	case "group":
		return &tg.InputPeerChat{ChatID: id}, "", nil
	case "supergroup":
		if cached, ok := c.sendPeers.Load(chat); ok {
			return cached.(*tg.InputPeerChannel), "", nil
		}
		iter := dialogs.NewQueryBuilder(c.api).GetDialogs().BatchSize(100).Iter()
		for iter.Next(ctx) {
			peer, ok := iter.Value().Peer.(*tg.InputPeerChannel)
			if ok && peer.ChannelID == id && peer.AccessHash != 0 {
				c.sendPeers.Store(chat, peer)
				return peer, "", nil
			}
		}
		return nil, DestinationCheckUnavailable, iter.Err()
	default:
		return nil, DestinationUnsupported, nil
	}
}

func sendOnce(ctx context.Context, api tg.Invoker, peer, sendAs tg.InputPeerClass, variant content.PreparedVariant, randomID int64, handoff SendHandoff) SendResult {
	var request bin.Encoder
	if variant.SavedMessage == nil {
		request = &tg.MessagesSendMessageRequest{Peer: peer, SendAs: sendAs, Message: variant.Text, RandomID: randomID}
	} else {
		request = &tg.MessagesForwardMessagesRequest{
			FromPeer: &tg.InputPeerSelf{}, ToPeer: peer, SendAs: sendAs,
			ID: []int{int(variant.SavedMessage.MessageID)}, RandomID: []int64{randomID}, DropAuthor: true,
		}
	}
	var updates tg.UpdatesBox
	submission := &oneShotRequest{request: request, handoff: handoff}
	ctx = context.WithValue(ctx, publicationRequestKey{}, submission)
	err := api.Invoke(ctx, submission, &updates)
	if err != nil {
		return sendError(err)
	}
	return confirmedResult(updates.Updates, randomID)
}

var errSubmissionReplay = errors.New("Telegram submission cannot be replayed")

type oneShotRequest struct {
	request bin.Encoder
	handoff SendHandoff
	started atomic.Bool
	encoded atomic.Bool
}

func (r *oneShotRequest) Encode(buf *bin.Buffer) error {
	// gotd can re-encode an RPC after an ACK timeout or reconnect.
	if !r.started.CompareAndSwap(false, true) {
		return errSubmissionReplay
	}
	if err := r.request.Encode(buf); err != nil {
		return err
	}
	r.encoded.Store(true)
	return nil
}

type publicationRequestKey struct{}

type publicationResolver struct{ dcs.Resolver }

func (r publicationResolver) Primary(ctx context.Context, dc int, list dcs.List) (transport.Conn, error) {
	conn, err := r.Resolver.Primary(ctx, dc, list)
	if err != nil {
		return nil, err
	}
	return &publicationConn{Conn: conn, writeSlot: make(chan struct{}, 1)}, nil
}

type publicationConn struct {
	transport.Conn
	writeSlot chan struct{}
}

func (c *publicationConn) Send(ctx context.Context, b *bin.Buffer) error {
	// gotd takes its write lock before applying the context deadline.
	// Reserve the transport outside the submission gate so a busy writer cannot delay controls.
	select {
	case c.writeSlot <- struct{}{}:
		defer func() { <-c.writeSlot }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	request, ok := ctx.Value(publicationRequestKey{}).(*oneShotRequest)
	// Service packets can share the RPC context before its request is encoded.
	if ok && request.encoded.Swap(false) {
		return request.handoff(ctx, func(writeCtx context.Context) error {
			if err := writeCtx.Err(); err != nil {
				return err
			}
			return c.Conn.Send(writeCtx, b)
		})
	}
	return c.Conn.Send(ctx, b)
}

func sendError(err error) SendResult {
	if wait := waitFromError(err); wait != nil {
		return SendResult{Outcome: domain.AttemptWait, Reason: "telegram_wait", Wait: wait}
	}
	if rpc, ok := tgerr.As(err); ok && rpc.Code >= 400 && rpc.Code < 500 && !rpc.IsType("RANDOM_ID_DUPLICATE") {
		return SendResult{
			Outcome: domain.AttemptFailure, Reason: "telegram_rejected",
			CheckRights: rpc.IsOneOf("CHAT_ADMIN_REQUIRED", "CHAT_WRITE_FORBIDDEN", "CHANNEL_PRIVATE", "USER_NOT_PARTICIPANT", "USER_BANNED_IN_CHANNEL"),
		}
	}
	return SendResult{Outcome: domain.AttemptUnknown, Reason: "no_delivery_confirmation"}
}

func confirmedResult(response tg.UpdatesClass, randomID int64) SendResult {
	var date, id int
	var updates []tg.UpdateClass
	switch result := response.(type) {
	case *tg.UpdateShortSentMessage:
		id, date = result.ID, result.Date
	case *tg.Updates:
		date, updates = result.Date, result.Updates
	case *tg.UpdatesCombined:
		date, updates = result.Date, result.Updates
	case *tg.UpdateShort:
		date, updates = result.Date, []tg.UpdateClass{result.Update}
	}
	for _, update := range updates {
		if match, ok := update.(*tg.UpdateMessageID); ok && match.RandomID == randomID {
			id = match.ID
			break
		}
	}
	if id <= 0 || date <= 0 {
		return SendResult{Outcome: domain.AttemptUnknown, Reason: "no_delivery_confirmation"}
	}
	return SendResult{Outcome: domain.AttemptSuccess, MessageID: id, ConfirmedAt: time.Unix(int64(date), 0)}
}
