package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	gotd "github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/gotd/td/tgtest"
	"github.com/gotd/td/tgtest/cluster"
	"github.com/gotd/td/transport"

	"telegram-messages-sender/internal/app"
	"telegram-messages-sender/internal/content"
	"telegram-messages-sender/internal/domain"
	"telegram-messages-sender/internal/store"
)

func TestSubmissionOverLocalMTProto(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	network := cluster.NewCluster(cluster.Options{})
	dispatch := network.Dispatch(2, "local send fixture")
	const date = 1_800_000_000
	var manual, forwards, noAck, directRetries atomic.Int32
	writeDone := make(chan struct{})
	dispatch.HandleFunc(tg.MessagesSendMessageRequestTypeID, func(s *tgtest.Server, req *tgtest.Request) error {
		var message tg.MessagesSendMessageRequest
		if err := message.Decode(req.Buf); err != nil {
			return err
		}
		manual.Add(1)
		if peer, ok := message.Peer.(*tg.InputPeerChat); !ok || peer.ChatID != 12 {
			t.Error("manual message had the wrong peer")
		}
		switch message.RandomID {
		case 1:
			if message.Message != "fixture text" || message.SendAs != nil {
				t.Error("basic group request changed its manual text or sender")
			}
			return s.SendResult(req, &tg.UpdateShortSentMessage{ID: 10, Date: date})
		case 2:
			return tgerr.New(420, "SLOWMODE_WAIT_17")
		case 3:
			return tgerr.New(420, "SOME_WAIT_19")
		case 4:
			return tgerr.New(403, "CHAT_WRITE_FORBIDDEN")
		case 5:
			noAck.Add(1)
			return nil
		case 8:
			if directRetries.Add(1) == 1 {
				return nil
			}
			return s.SendResult(req, &tg.UpdateShortSentMessage{ID: 12, Date: date})
		case 9:
			return s.SendResult(req, &tg.UpdateShortSentMessage{ID: 13, Date: date})
		case 10:
			select {
			case <-writeDone:
			case <-req.RequestCtx.Done():
				return req.RequestCtx.Err()
			}
			return s.SendResult(req, &tg.UpdateShortSentMessage{ID: 14, Date: date})
		default:
			t.Errorf("unexpected send with correlation %d", message.RandomID)
			return errors.New("unexpected local request")
		}
	})
	dispatch.HandleFunc(tg.MessagesForwardMessagesRequestTypeID, func(s *tgtest.Server, req *tgtest.Request) error {
		var message tg.MessagesForwardMessagesRequest
		if err := message.Decode(req.Buf); err != nil {
			return err
		}
		forwards.Add(1)
		_, selfSource := message.FromPeer.(*tg.InputPeerSelf)
		_, selfSender := message.SendAs.(*tg.InputPeerSelf)
		peer, channel := message.ToPeer.(*tg.InputPeerChannel)
		if !selfSource || !selfSender || !channel || peer.ChannelID != 55 || peer.AccessHash != 89 ||
			!message.DropAuthor || message.DropMediaCaptions || len(message.RandomID) != 1 ||
			message.RandomID[0] != 6 || len(message.ID) != 1 || message.ID[0] != 72 {
			t.Error("Saved Messages request had the wrong source, destination, sender, caption or correlation fields")
		}
		return s.SendResult(req, &tg.Updates{Date: date, Updates: []tg.UpdateClass{
			&tg.UpdateMessageID{ID: 8, RandomID: 81},
			&tg.UpdateMessageID{ID: 11, RandomID: 6},
		}})
	})
	serverDone := make(chan error, 1)
	go func() { serverDone <- network.Up(ctx) }()
	defer func() {
		cancel()
		if err := <-serverDone; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("local MTProto server: %v", err)
		}
	}()
	select {
	case <-network.Ready():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	client := gotd.NewClient(1, "fixture", gotd.Options{
		PublicKeys: network.Keys(), Resolver: interruptedSendResolver{Resolver: publicationResolver{Resolver: network.Resolver()}}, DCList: network.List(), NoUpdates: true,
		RetryInterval: 300 * time.Millisecond,
	})
	err := client.Run(ctx, func(live context.Context) error {
		peer := &tg.InputPeerChat{ChatID: 12}
		variant := content.PreparedVariant{Text: "fixture text"}
		var begins int
		begin := func(ctx context.Context, write func(context.Context) error) error {
			begins++
			return write(ctx)
		}

		for _, test := range []struct {
			id      int64
			outcome domain.AttemptOutcome
			wait    time.Duration
			scoped  bool
		}{
			{1, domain.AttemptSuccess, 0, false},
			{2, domain.AttemptWait, 17 * time.Second, true},
			{3, domain.AttemptWait, 19 * time.Second, false},
			{4, domain.AttemptFailure, 0, false},
			{5, domain.AttemptUnknown, 0, false},
		} {
			result := sendOnce(live, client.API().Invoker(), peer, nil, variant, test.id, begin)
			if result.Outcome != test.outcome {
				return fmt.Errorf("request %d yielded %+v", test.id, result)
			}
			if test.wait > 0 && (result.Wait == nil || result.Wait.Duration != test.wait || result.Wait.ChatScoped != test.scoped) {
				return fmt.Errorf("request %d yielded wrong wait: %+v", test.id, result.Wait)
			}
			if test.id == 1 && (result.MessageID != 10 || result.ConfirmedAt.Unix() != date) ||
				test.id == 4 && !result.CheckRights {
				return fmt.Errorf("request %d yielded unexpected details: %+v", test.id, result)
			}
		}
		if noAck.Load() != 1 || begins != 5 {
			return fmt.Errorf("gotd replay reached the network: handoffs %d, unacknowledged sends %d", begins, noAck.Load())
		}
		result := sendOnce(live, client.API().Invoker(), &tg.InputPeerChannel{ChannelID: 55, AccessHash: 89}, &tg.InputPeerSelf{},
			content.PreparedVariant{Kind: content.Photo, SavedMessage: &domain.SavedMessage{MessageID: 72, AccountID: 42}}, 6, begin)
		if result.Outcome != domain.AttemptSuccess || result.MessageID != 11 || result.ConfirmedAt.Unix() != date ||
			begins != 6 || forwards.Load() != 1 {
			return fmt.Errorf("forward correlation was not confirmed: %+v", result)
		}

		sendOnce(live, client.API().Invoker(), peer, nil, variant, 7, func(context.Context, func(context.Context) error) error { return errors.New("fixture commit failure") })
		if manual.Load() != 5 {
			return fmt.Errorf("failed commit reached MTProto: %d manual requests", manual.Load())
		}
		if _, err := client.API().MessagesSendMessage(live, &tg.MessagesSendMessageRequest{
			Peer: peer, Message: variant.Text, RandomID: 8,
		}); err != nil || directRetries.Load() != 2 {
			return fmt.Errorf("raw gotd replay control did not observe a retry: %d, %v", directRetries.Load(), err)
		}
		state, err := store.Open(live, filepath.Join(t.TempDir(), "stop-before-write.db"))
		if err != nil {
			return err
		}
		defer state.Close()
		pair := store.Pair{PublicationID: "a", ChatID: "group:12"}
		revision, err := state.SavePublication(live, 0, domain.Publication{
			ID: pair.PublicationID, State: domain.Enabled, Interval: time.Minute, Destinations: []domain.ChatID{pair.ChatID},
			Variants: []domain.Variant{{ID: "first", Text: variant.Text}},
		})
		if err != nil {
			return err
		}
		stopCalls := 0
		stop := func() error {
			stopCalls++
			_, err := app.NewHandler(state)(live, app.Command{Operation: app.Stop, PublicationID: pair.PublicationID})
			return err
		}
		var attemptErr error
		before := manual.Load()
		result = sendOnce(context.WithValue(live, interruptSendKey{}, stop), client.API().Invoker(), peer, nil, variant, 9, func(ctx context.Context, write func(context.Context) error) error {
			now := time.Now()
			_, attemptErr = state.BeginSubmission(ctx, revision, store.Submission{
				Pair: pair, VariantID: "first", RandomID: 9, FirstDue: now, SubmittedAt: now,
			})
			if attemptErr != nil {
				return attemptErr
			}
			return write(ctx)
		})
		delivery, err := state.DeliveryState(live)
		if err != nil {
			return err
		}
		if stopCalls != 1 || !errors.Is(attemptErr, store.ErrConfigurationChanged) ||
			manual.Load() != before || len(delivery.Pairs) != 0 {
			return fmt.Errorf("committed stop before transport write escaped the gate: stop=%d writes=%d attempts=%d result=%+v error=%v",
				stopCalls, manual.Load()-before, len(delivery.Pairs), result, attemptErr)
		}

		result = sendOnce(live, client.API().Invoker(), peer, nil, variant, 10, func(ctx context.Context, write func(context.Context) error) error {
			writeCtx, cancelWrite := context.WithCancel(ctx)
			err := write(writeCtx)
			cancelWrite()
			close(writeDone)
			return err
		})
		if result.Outcome != domain.AttemptSuccess || result.MessageID != 14 {
			return fmt.Errorf("ending the write context interrupted the response: %+v", result)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

type interruptSendKey struct{}

type interruptedSendResolver struct{ dcs.Resolver }

func (r interruptedSendResolver) Primary(ctx context.Context, dc int, list dcs.List) (transport.Conn, error) {
	conn, err := r.Resolver.Primary(ctx, dc, list)
	if err != nil {
		return nil, err
	}
	return interruptedSendConn{Conn: conn}, nil
}

type interruptedSendConn struct{ transport.Conn }

func (c interruptedSendConn) Send(ctx context.Context, b *bin.Buffer) error {
	if stop, ok := ctx.Value(interruptSendKey{}).(func() error); ok {
		if err := stop(); err != nil {
			return err
		}
	}
	return c.Conn.Send(ctx, b)
}

func TestBusyMTProtoConnectionWaitsOutsidePublicationHandoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	writer, reader := net.Pipe()
	defer writer.Close()
	defer reader.Close()
	if err := reader.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	protocol := transport.NewProtocol(func() transport.Codec { return transport.Intermediate.CodecNoHeader() })
	raw, err := protocol.Handshake(writer)
	if err != nil {
		t.Fatal(err)
	}
	conn := &publicationConn{Conn: raw, writeSlot: make(chan struct{}, 1)}
	unrelatedDone := make(chan error, 1)
	go func() {
		unrelatedDone <- conn.Send(ctx, &bin.Buffer{Buf: []byte{1, 2, 3, 4}})
	}()
	var frameHeader [4]byte
	if _, err := io.ReadFull(reader, frameHeader[:]); err != nil {
		t.Fatal(err)
	}

	var enteredHandoff bool
	pending := &oneShotRequest{handoff: func(context.Context, func(context.Context) error) error {
		enteredHandoff = true
		return errors.New("publication handoff ran before the previous write finished")
	}}
	pending.encoded.Store(true)
	requestCtx, stop := context.WithTimeout(context.WithValue(ctx, publicationRequestKey{}, pending), 30*time.Millisecond)
	defer stop()
	err = conn.Send(requestCtx, &bin.Buffer{Buf: []byte{5, 6, 7, 8}})
	if !errors.Is(err, context.DeadlineExceeded) || enteredHandoff {
		t.Fatalf("publication entered the handoff while transport was busy: %v", err)
	}
	reader.Close()
	<-unrelatedDone
}
