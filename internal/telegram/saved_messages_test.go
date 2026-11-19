package telegram

import (
	"bytes"
	"errors"
	"image/jpeg"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"telegram-messages-sender/internal/content"
	"telegram-messages-sender/internal/domain"
)

func TestSavedMessagesUseSelfDialogAndFetchAnEditedSource(t *testing.T) {
	self := &tg.PeerUser{UserID: 42}
	caption := &tg.Message{ID: 12, PeerID: self, Message: "photo caption", Media: &tg.MessageMediaPhoto{Photo: &tg.Photo{ID: 1}}}
	caption.Entities = []tg.MessageEntityClass{&tg.MessageEntityBold{Offset: 0, Length: 5}}
	video := &tg.Message{ID: 11, PeerID: self, Message: "video caption", Media: &tg.MessageMediaDocument{
		Video: true, Document: &tg.Document{ID: 2, Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeVideo{}}},
	}}
	current := caption
	var lookups int
	api := tg.NewClient(rpcFixture(func(input bin.Encoder) (bin.Encoder, error) {
		switch req := input.(type) {
		case *tg.MessagesGetHistoryRequest:
			if _, ok := req.Peer.(*tg.InputPeerSelf); !ok || req.Limit != 1 {
				t.Fatalf("invalid Saved Messages request %T", req.Peer)
			}
			switch req.OffsetID {
			case 0:
				return &tg.MessagesMessagesSlice{Messages: []tg.MessageClass{caption}, Count: 2}, nil
			case 12:
				return &tg.MessagesMessages{Messages: []tg.MessageClass{video}}, nil
			case 11:
				return &tg.MessagesMessages{}, nil
			default:
				t.Fatalf("wrong history cursor %d", req.OffsetID)
			}
		case *tg.MessagesGetMessagesRequest:
			lookups++
			if len(req.ID) != 1 || req.ID[0].(*tg.InputMessageID).ID != caption.ID {
				t.Fatal("lookup must fetch the source message ID")
			}
			return &tg.MessagesMessages{Messages: []tg.MessageClass{current}}, nil
		}
		t.Fatalf("unexpected Saved Messages request %T", input)
		return nil, nil
	}))
	first, err := listSaved(t.Context(), api, 42, 0, 1)
	if err != nil || first.NextOffset != 12 || len(first.Items) != 1 {
		t.Fatalf("first page: %v", err)
	}
	preview := first.Items[0]
	bold, ok := preview.Entities[0].(*tg.MessageEntityBold)
	if !ok || bold.Length != 5 || preview.Kind != content.Photo || preview.Text != "photo caption" {
		t.Fatal("photo preview lost caption or formatting")
	}
	second, err := listSaved(t.Context(), api, 42, first.NextOffset, 1)
	if err != nil || len(second.Items) != 1 || second.Items[0].Kind != content.Video || second.Items[0].Text != "video caption" {
		t.Fatalf("second page: %v", err)
	}
	end, err := listSaved(t.Context(), api, 42, second.NextOffset, 1)
	if err != nil || len(end.Items) != 0 || end.NextOffset != 0 {
		t.Fatalf("pagination did not terminate: %v", err)
	}

	if kind, err := resolveSaved(t.Context(), api, 42, preview.Reference); kind != content.Photo || err != nil {
		t.Fatalf("initial source kind %q, %v", kind, err)
	}
	current = &tg.Message{ID: caption.ID, PeerID: self, Message: "edited text"}
	if kind, err := resolveSaved(t.Context(), api, 42, preview.Reference); kind != content.Text || err != nil || lookups != 2 {
		t.Fatalf("fresh lookup did not resolve the edit: %q, %v", kind, err)
	}
}

func TestSavedSourceRejectsForeignAndUnsupportedResponses(t *testing.T) {
	ref := domain.SavedMessage{AccountID: 42, MessageID: 81}
	self := &tg.PeerUser{UserID: 42}
	for _, tt := range []struct {
		name   string
		result bin.Encoder
		rpcErr error
		want   error
	}{
		{"explicitly missing", &tg.MessagesMessages{Messages: []tg.MessageClass{&tg.MessageEmpty{ID: 81}}}, nil, content.ErrSourceDeleted},
		{"empty complete response", &tg.MessagesMessages{}, nil, content.ErrSourceDeleted},
		{"invalid message ID", nil, tgerr.New(400, "MESSAGE_ID_INVALID"), content.ErrSourceDeleted},
		{"invalid short message ID", nil, tgerr.New(400, "MSG_ID_INVALID"), content.ErrSourceDeleted},
		{"temporary failure", nil, errors.New("temporary transport failure"), content.ErrSourceUnavailable},
		{"different dialog", &tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{
			ID: 81, PeerID: &tg.PeerChat{ChatID: 42}, Message: "foreign",
		}}}, nil, content.ErrSourceAccount},
		{"different message", &tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{
			ID: 82, PeerID: self, Message: "foreign",
		}}}, nil, content.ErrSourceUnavailable},
		{"protected", &tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{
			ID: 81, PeerID: self, Message: "private", Noforwards: true,
		}}}, nil, content.ErrSourceProtected},
		{"album", &tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{
			ID: 81, PeerID: self, Message: "album caption", GroupedID: 15,
		}}}, nil, content.ErrSourceUnsupported},
		{"document", &tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{
			ID: 81, PeerID: self, Message: "document caption",
			Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: 2}},
		}}}, nil, content.ErrSourceUnsupported},
	} {
		t.Run(tt.name, func(t *testing.T) {
			api := tg.NewClient(rpcFixture(func(_ bin.Encoder) (bin.Encoder, error) {
				return tt.result, tt.rpcErr
			}))
			kind, err := resolveSaved(t.Context(), api, 42, ref)
			if kind != "" || err != tt.want {
				t.Fatalf("got kind %q, error %v, want %v", kind, err, tt.want)
			}
		})
	}
	t.Run("different account never queries message ID", func(t *testing.T) {
		api := tg.NewClient(rpcFixture(func(_ bin.Encoder) (bin.Encoder, error) {
			t.Fatal("attempted to query another account's message")
			return nil, nil
		}))
		if _, err := resolveSaved(t.Context(), api, 43, ref); err != content.ErrSourceAccount {
			t.Fatalf("unexpected account mismatch result: %v", err)
		}
	})
}

func TestSavedMessagesExposeTransientMediaPreviewsWithoutCaptions(t *testing.T) {
	self := &tg.PeerUser{UserID: 42}
	const sentAt = 1700000000
	photo := &tg.Photo{ID: 5, DCID: 2, Sizes: []tg.PhotoSizeClass{
		&tg.PhotoStrippedSize{Type: "i", Bytes: []byte{1, 8, 12, 0x2a}},
		&tg.PhotoSize{Type: "m", W: 320, H: 240, Size: 4900},
		&tg.PhotoSize{Type: "s", W: 100, H: 75, Size: 1000},
	}}
	video := &tg.Document{
		ID: 6, DCID: 4, MimeType: "video/mp4",
		Thumbs: []tg.PhotoSizeClass{&tg.PhotoSize{Type: "m", W: 320, H: 180, Size: 3000}},
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeVideo{Duration: 3.5, W: 1920, H: 1080},
			&tg.DocumentAttributeFilename{FileName: "fixture.mp4"},
		},
	}
	api := tg.NewClient(rpcFixture(func(input bin.Encoder) (bin.Encoder, error) {
		if _, ok := input.(*tg.MessagesGetHistoryRequest); !ok {
			t.Fatalf("unexpected media request %T", input)
		}
		return &tg.MessagesMessages{Messages: []tg.MessageClass{
			&tg.Message{ID: 3, Date: sentAt, PeerID: self, Media: &tg.MessageMediaPhoto{Photo: photo}},
			&tg.Message{ID: 2, Date: sentAt - 1, PeerID: self, Media: &tg.MessageMediaDocument{Video: true, Document: video}},
			&tg.Message{ID: 1, PeerID: self, Media: &tg.MessageMediaPhoto{Photo: photo}, Noforwards: true},
		}}, nil
	}))

	page, err := listSaved(t.Context(), api, 42, 0, 10)
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("Saved Messages page: %d items, %v", len(page.Items), err)
	}
	photoItem, videoItem, protected := page.Items[0], page.Items[1], page.Items[2]
	if photoItem.Kind != content.Photo || photoItem.Text != "" || photoItem.SentAt.Unix() != sentAt || photoItem.Media == nil {
		t.Fatal("captionless photo lacks a preview")
	}
	image, err := jpeg.DecodeConfig(bytes.NewReader(photoItem.Media.InlineThumbnail))
	if err != nil || image.Width != 12 || image.Height != 8 {
		t.Fatalf("stripped photo thumbnail was not expanded: %v", err)
	}
	photoLocation, ok := photoItem.Media.ThumbnailLocation.(*tg.InputPhotoFileLocation)
	if !ok || photoLocation.ID != photo.ID || photoLocation.ThumbSize != "s" || photoItem.Media.ThumbnailDC != photo.DCID {
		t.Fatal("photo has no thumbnail reference for the picker")
	}
	if videoItem.Kind != content.Video || videoItem.Text != "" || videoItem.Media == nil {
		t.Fatal("captionless video lacks a preview")
	}
	media := videoItem.Media
	videoLocation, ok := media.ThumbnailLocation.(*tg.InputDocumentFileLocation)
	if len(media.InlineThumbnail) != 0 || !ok || videoLocation.ID != video.ID || videoLocation.ThumbSize != "m" || media.ThumbnailDC != video.DCID {
		t.Fatal("video has no thumbnail reference for the picker")
	}
	if media.Filename != "fixture.mp4" || media.DurationSeconds != 3.5 || media.Width != 1920 || media.Height != 1080 {
		t.Fatal("video has no identifying metadata")
	}
	if protected.Unavailable != content.ErrSourceProtected || protected.Media != nil {
		t.Fatal("protected item exposed its thumbnail")
	}
}
