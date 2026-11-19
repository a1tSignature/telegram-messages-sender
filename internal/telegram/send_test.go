package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
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
	dispatch.HandleFunc(tg.UpdatesGetDifferenceRequestTypeID, func(s *tgtest.Server, req *tgtest.Request) error {
		var message tg.UpdatesGetDifferenceRequest
		if err := message.Decode(req.Buf); err != nil {
			return err
		}
		if message.Pts != 20 || message.Qts != 4 || message.Date != date-1 {
			t.Errorf("difference RPC did not use the stored checkpoint: %+v", message)
		}
		return s.SendResult(req, &tg.UpdatesDifference{
			State: tg.UpdatesState{Pts: 21, Qts: 4, Date: date, Seq: 2},
			NewMessages: []tg.MessageClass{
				&tg.Message{ID: 14, Out: true, PeerID: &tg.PeerChat{ChatID: 12}, Date: date},
				&tg.Message{ID: 14, Out: true, PeerID: &tg.PeerChannel{ChannelID: 55}, Date: date - 2},
			},
			OtherUpdates: []tg.UpdateClass{&tg.UpdateMessageID{RandomID: 10, ID: 14}},
		})
	})

	dispatch.HandleFunc(tg.ChannelsGetMessagesRequestTypeID, func(s *tgtest.Server, req *tgtest.Request) error {
		var message tg.ChannelsGetMessagesRequest
		if err := message.Decode(req.Buf); err != nil {
			return err
		}
		channel, ok := message.Channel.(*tg.InputChannel)
		if !ok || channel.ChannelID != 55 || channel.AccessHash != 89 || len(message.ID) != 1 {
			return errors.New("channel lookup did not target the stored supergroup")
		}
		id, ok := message.ID[0].(*tg.InputMessageID)
		if !ok {
			return errors.New("channel lookup did not target the correlated message")
		}
		switch id.ID {
		case 15:
		case 16:
			return tgerr.New(500, "INTERNAL_SERVER_ERROR")
		case 17:
			return tgerr.New(400, "MSG_ID_INVALID")
		case 18:
			return tgerr.New(406, "CHANNEL_PRIVATE")
		default:
			return errors.New("channel lookup did not target the correlated message")
		}
		return s.SendResult(req, &tg.MessagesChannelMessages{Pts: 30, Count: 3, Messages: []tg.MessageClass{
			&tg.Message{ID: 15, Out: false, PeerID: &tg.PeerChannel{ChannelID: 55}, Date: date - 2},
			&tg.Message{ID: 15, Out: true, PeerID: &tg.PeerChannel{ChannelID: 72}, Date: date - 1},
			&tg.Message{ID: 15, Out: true, PeerID: &tg.PeerChannel{ChannelID: 55}, Date: date},
		}})
	})
	dispatch.HandleFunc(tg.MessagesGetDialogsRequestTypeID, func(*tgtest.Server, *tgtest.Request) error {
		return tgerr.New(500, "INTERNAL_SERVER_ERROR")
	})

	var thumbnail bytes.Buffer
	if err := jpeg.Encode(&thumbnail, image.NewRGBA(image.Rect(0, 0, 48, 32)), nil); err != nil {
		t.Fatal(err)
	}
	dispatch.HandleFunc(tg.MessagesGetHistoryRequestTypeID, func(s *tgtest.Server, req *tgtest.Request) error {
		var request tg.MessagesGetHistoryRequest
		if err := request.Decode(req.Buf); err != nil {
			return err
		}
		if _, ok := request.Peer.(*tg.InputPeerSelf); !ok {
			return errors.New("history was not loaded from Saved Messages")
		}
		video := &tg.Document{
			ID: 72, DCID: 2, MimeType: "video/mp4",
			Thumbs: []tg.PhotoSizeClass{&tg.PhotoSize{Type: "s", W: 48, H: 32, Size: thumbnail.Len()}},
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeVideo{W: 1920, H: 1080, Duration: 8.5},
				&tg.DocumentAttributeFilename{FileName: "sample.mp4"},
			},
		}
		return s.SendResult(req, &tg.MessagesMessages{Messages: []tg.MessageClass{
			&tg.Message{ID: 72, Out: true, PeerID: &tg.PeerUser{UserID: 42}, Date: date,
				Media: &tg.MessageMediaDocument{Video: true, Document: video}},
		}})
	})
	dispatch.HandleFunc(tg.UploadGetFileRequestTypeID, func(s *tgtest.Server, req *tgtest.Request) error {
		var request tg.UploadGetFileRequest
		if err := request.Decode(req.Buf); err != nil {
			return err
		}
		location, ok := request.Location.(*tg.InputDocumentFileLocation)
		if !ok || location.ID != 72 || location.ThumbSize != "s" || request.Offset != 0 || request.Limit != thumbnailReadLimit {
			return errors.New("picker requested something other than the bounded Saved Messages thumbnail")
		}
		return s.SendResult(req, &tg.UploadFile{Type: &tg.StorageFileJpeg{}, Bytes: thumbnail.Bytes()})
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
		batch, err := recoverDifference(live, client.API(), func(context.Context, domain.RecoveryReference) (time.Time, error) {
			return time.Time{}, nil
		}, domain.UpdateCheckpoint{PTS: 20, QTS: 4, Date: date - 1}, []domain.RecoveryReference{
			{RandomID: 10, ChatID: "group:12"},
			{RandomID: 11, ChatID: "supergroup:55"},
		})
		if err != nil || batch.More || batch.Checkpoint.PTS != 21 || len(batch.Evidence) != 1 ||
			batch.Evidence[0].RandomID != 10 || batch.Evidence[0].MessageID != 14 ||
			batch.Evidence[0].SentAt.Unix() != date {
			return fmt.Errorf("recovery did not correlate the persisted random ID: %+v, %v", batch, err)
		}

		connection := &Connection{api: client.API()}
		connection.sendPeers.Store(domain.ChatID("supergroup:55"), &tg.InputPeerChannel{ChannelID: 55, AccessHash: 89})
		sentAt, err := connection.recoveredMessage(live, domain.RecoveryReference{RandomID: 10, ChatID: "supergroup:55", MessageID: 15})
		if err != nil || sentAt.Unix() != date {
			return fmt.Errorf("exact supergroup result did not confirm the message: %v, %v", sentAt, err)
		}
		checkpoint := domain.UpdateCheckpoint{PTS: 20, QTS: 4, Date: date - 1}
		ref := domain.RecoveryReference{RandomID: 16, ChatID: "supergroup:55", MessageID: 16}
		for _, id := range []domain.ChatID{"supergroup:55", "supergroup:56"} {
			ref.ChatID = id
			batch, err = recoverDifference(live, client.API(), connection.recoveredMessage, checkpoint, []domain.RecoveryReference{ref})
			if !errors.Is(err, ErrTransport) || batch.Checkpoint != checkpoint || len(batch.Evidence) != 0 {
				return fmt.Errorf("incomplete lookup for %s did not preserve the recovery checkpoint: %+v, %v", id, batch, err)
			}
		}
		ref.ChatID, ref.MessageID = "supergroup:55", 17
		batch, err = recoverDifference(live, client.API(), connection.recoveredMessage, checkpoint, []domain.RecoveryReference{ref})
		if err != nil || batch.Checkpoint.PTS != 21 || len(batch.Evidence) != 1 || !batch.Evidence[0].SentAt.IsZero() {
			return fmt.Errorf("definitively missing message did not allow recovery without confirmation: %+v, %v", batch, err)
		}
		ref.MessageID = 18
		batch, err = recoverDifference(live, client.API(), connection.recoveredMessage, checkpoint, []domain.RecoveryReference{
			ref, {RandomID: 10, ChatID: "group:12"},
		})
		if err != nil || batch.Checkpoint.PTS != 21 || len(batch.Evidence) != 2 ||
			!batch.Evidence[0].SentAt.IsZero() || batch.Evidence[1].SentAt.Unix() != date {
			return fmt.Errorf("inaccessible supergroup blocked recovery for another chat: %+v, %v", batch, err)
		}

		var ready atomic.Bool
		ready.Store(true)
		session := &stagedSession{dc: 2}
		if err := session.commit(live, nil, 2); err != nil {
			return err
		}
		connection.account, connection.ready, connection.ctx, connection.session = 42, &ready, live, session
		connection.fileDC = client.FileDC
		page, err := connection.ListSaved(live, 0, 20)
		if err != nil || len(page.Items) != 1 {
			return fmt.Errorf("Saved Messages page returned %d items: %v", len(page.Items), err)
		}
		item := page.Items[0]
		if item.Text != "" || item.Media == nil || item.Media.Filename != "sample.mp4" ||
			!bytes.Equal(item.Media.InlineThumbnail, thumbnail.Bytes()) {
			return errors.New("captionless Saved Messages media did not retain its fetched preview")
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
