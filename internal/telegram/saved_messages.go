package telegram

import (
	"context"
	"math"
	"time"

	"github.com/gotd/td/telegram/thumbnail"
	"github.com/gotd/td/tg"

	"telegram-messages-sender/internal/content"
	"telegram-messages-sender/internal/domain"
)

type SavedPreview struct {
	Reference   domain.SavedMessage
	SentAt      time.Time
	Media       *SavedMediaPreview
	Kind        content.Kind
	Text        string
	Entities    []tg.MessageEntityClass
	Unavailable error
}

type SavedMediaPreview struct {
	InlineThumbnail   []byte
	ThumbnailLocation tg.InputFileLocationClass
	ThumbnailDC       int
	Width             int
	Height            int
	DurationSeconds   float64
	Filename          string
	Spoiler           bool
}

type SavedPage struct {
	Items      []SavedPreview
	NextOffset int
}

type savedMessagesAPI interface {
	MessagesGetHistory(context.Context, *tg.MessagesGetHistoryRequest) (tg.MessagesMessagesClass, error)
	MessagesGetMessages(context.Context, []tg.InputMessageClass) (tg.MessagesMessagesClass, error)
}

var _ content.Source = (*Connection)(nil)

// ListSaved uses offset zero for the first page. A zero NextOffset ends pagination.
func (c *Connection) ListSaved(ctx context.Context, offset, limit int) (SavedPage, error) {
	if !c.Status().TransportAvailable {
		return SavedPage{}, content.ErrSourceUnavailable
	}
	return listSaved(ctx, c.api, c.account, offset, limit)
}

func (c *Connection) ResolveSaved(ctx context.Context, ref domain.SavedMessage) (content.Kind, error) {
	if !c.Status().TransportAvailable {
		return "", content.ErrSourceUnavailable
	}
	return resolveSaved(ctx, c.api, c.account, ref)
}

func listSaved(ctx context.Context, api savedMessagesAPI, account domain.AccountID, offset, limit int) (SavedPage, error) {
	if offset < 0 || offset > math.MaxInt32 || limit < 1 || limit > 100 {
		return SavedPage{}, content.ErrSourceUnavailable
	}
	response, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer: &tg.InputPeerSelf{}, OffsetID: offset, Limit: limit,
	})
	if err != nil {
		return SavedPage{}, savedLookupError(ctx)
	}
	messages, ok := savedResults(response)
	if !ok {
		return SavedPage{}, content.ErrSourceUnavailable
	}

	var result SavedPage
	last := 0
	for _, message := range messages {
		if message == nil || message.GetID() <= 0 || (offset != 0 && message.GetID() >= offset) {
			return SavedPage{}, content.ErrSourceUnavailable
		}
		if last == 0 || message.GetID() < last {
			last = message.GetID()
		}
		if _, empty := message.(*tg.MessageEmpty); empty {
			continue
		}

		preview, err := classifySaved(account, message)
		if err == content.ErrSourceAccount {
			return SavedPage{}, err
		}
		preview.Unavailable = err
		if plain, ok := message.(*tg.Message); ok && err == nil {
			preview.SentAt = time.Unix(int64(plain.Date), 0).UTC()
			preview.Media = savedMediaPreview(plain, preview.Kind)
		}
		result.Items = append(result.Items, preview)
	}
	if _, sliced := response.(*tg.MessagesMessagesSlice); sliced || len(messages) == limit {
		result.NextOffset = last
	}
	return result, nil
}

func resolveSaved(ctx context.Context, api savedMessagesAPI, account domain.AccountID, ref domain.SavedMessage) (content.Kind, error) {
	if ref.AccountID != account {
		return "", content.ErrSourceAccount
	}
	if ref.MessageID <= 0 || ref.MessageID > math.MaxInt32 {
		return "", content.ErrSourceUnsupported
	}

	id := int(ref.MessageID)
	response, err := api.MessagesGetMessages(ctx, []tg.InputMessageClass{&tg.InputMessageID{ID: id}})
	if err != nil {
		if wait := waitFromError(err); wait != nil {
			return "", wait
		}
		return "", savedLookupError(ctx)
	}
	messages, ok := savedResults(response)
	if !ok {
		return "", content.ErrSourceUnavailable
	}
	if len(messages) == 0 {
		if _, complete := response.(*tg.MessagesMessages); complete {
			return "", content.ErrSourceDeleted
		}
		return "", content.ErrSourceUnavailable
	}
	if len(messages) != 1 || messages[0] == nil || messages[0].GetID() != id {
		return "", content.ErrSourceUnavailable
	}

	preview, err := classifySaved(account, messages[0])
	return preview.Kind, err
}

func savedResults(response tg.MessagesMessagesClass) ([]tg.MessageClass, bool) {
	switch response := response.(type) {
	case *tg.MessagesMessages:
		return response.Messages, true
	case *tg.MessagesMessagesSlice:
		return response.Messages, true
	default:
		return nil, false
	}
}

func classifySaved(account domain.AccountID, message tg.MessageClass) (SavedPreview, error) {
	preview := SavedPreview{Reference: domain.SavedMessage{AccountID: account, MessageID: int64(message.GetID())}}
	switch m := message.(type) {
	case *tg.MessageEmpty:
		if m.PeerID != nil && !personalPeer(m.PeerID, account) {
			return preview, content.ErrSourceAccount
		}
		return preview, content.ErrSourceDeleted
	case *tg.MessageService:
		if !personalPeer(m.PeerID, account) {
			return preview, content.ErrSourceAccount
		}
		return preview, content.ErrSourceUnsupported
	case *tg.Message:
		if !personalPeer(m.PeerID, account) {
			return preview, content.ErrSourceAccount
		}
		if m.Noforwards || len(m.RestrictionReason) != 0 {
			return preview, content.ErrSourceProtected
		}
		if m.VideoProcessingPending {
			return preview, content.ErrSourceUnavailable
		}
		if m.GroupedID != 0 {
			return preview, content.ErrSourceUnsupported
		}
		kind, err := savedKind(m)
		if err != nil {
			return preview, err
		}
		preview.Kind = kind
		preview.Text = m.Message
		preview.Entities = m.Entities
		return preview, nil
	default:
		return preview, content.ErrSourceUnsupported
	}
}

func savedKind(m *tg.Message) (content.Kind, error) {
	switch media := m.Media.(type) {
	case nil, *tg.MessageMediaEmpty, *tg.MessageMediaWebPage:
		if m.Message != "" {
			return content.Text, nil
		}
	case *tg.MessageMediaPhoto:
		if media.TTLSeconds != 0 {
			return "", content.ErrSourceProtected
		}
		if photo, ok := media.Photo.(*tg.Photo); ok && photo.ID > 0 && !media.LivePhoto && media.Video == nil {
			return content.Photo, nil
		}
	case *tg.MessageMediaDocument:
		if media.TTLSeconds != 0 {
			return "", content.ErrSourceProtected
		}
		if isSingleVideo(media) {
			return content.Video, nil
		}
	}
	return "", content.ErrSourceUnsupported
}

func isSingleVideo(media *tg.MessageMediaDocument) bool {
	doc, ok := media.Document.(*tg.Document)
	if !ok || doc.ID <= 0 || !media.Video || media.Round || media.Voice {
		return false
	}
	video := false
	for _, attr := range doc.Attributes {
		switch attr := attr.(type) {
		case *tg.DocumentAttributeVideo:
			video = !attr.RoundMessage
		case *tg.DocumentAttributeAnimated, *tg.DocumentAttributeSticker:
			return false
		}
	}
	return video
}

func savedMediaPreview(message *tg.Message, kind content.Kind) *SavedMediaPreview {
	switch kind {
	case content.Photo:
		media := message.Media.(*tg.MessageMediaPhoto)
		photo := media.Photo.(*tg.Photo)
		preview, remote := previewThumbnails(photo.Sizes)
		preview.Spoiler = media.Spoiler
		if remote != "" {
			preview.ThumbnailLocation = photo.AsInputPhotoFileLocation(remote)
			preview.ThumbnailDC = photo.DCID
		}
		return &preview
	case content.Video:
		media := message.Media.(*tg.MessageMediaDocument)
		video := media.Document.(*tg.Document)
		preview, remote := previewThumbnails(video.Thumbs)
		preview.Spoiler = media.Spoiler
		if remote != "" {
			preview.ThumbnailLocation = video.AsInputDocumentFileLocation(remote)
			preview.ThumbnailDC = video.DCID
		}
		for _, attribute := range video.Attributes {
			switch value := attribute.(type) {
			case *tg.DocumentAttributeVideo:
				preview.Width = value.W
				preview.Height = value.H
				preview.DurationSeconds = value.Duration
			case *tg.DocumentAttributeFilename:
				preview.Filename = value.FileName
			}
		}
		return &preview
	default:
		return nil
	}
}

func previewThumbnails(sizes []tg.PhotoSizeClass) (SavedMediaPreview, string) {
	var preview SavedMediaPreview
	var remote string
	var smallestArea int64
	var stripped, cached []byte

	for _, size := range sizes {
		switch value := size.(type) {
		case *tg.PhotoStrippedSize:
			stripped = value.Bytes
		case *tg.PhotoCachedSize:
			cached = value.Bytes
		}
		sized, ok := size.(interface {
			GetW() int
			GetH() int
		})
		if !ok || sized.GetW() <= 0 || sized.GetH() <= 0 {
			continue
		}

		area := int64(sized.GetW()) * int64(sized.GetH())
		if area > int64(preview.Width)*int64(preview.Height) {
			preview.Width, preview.Height = sized.GetW(), sized.GetH()
		}
		switch size.(type) {
		case *tg.PhotoSize, *tg.PhotoSizeProgressive:
			if size.GetType() != "" && (smallestArea == 0 || area < smallestArea) {
				smallestArea = area
				remote = size.GetType()
			}
		}
	}

	if len(cached) != 0 {
		preview.InlineThumbnail = cached
	} else if len(stripped) != 0 {
		preview.InlineThumbnail, _ = thumbnail.Expand(stripped)
	}
	return preview, remote
}

func savedLookupError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return content.ErrSourceUnavailable
}
