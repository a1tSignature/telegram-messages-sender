package telegram

import (
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func TestDestinationsCanLoadDuringReconnect(t *testing.T) {
	var ready atomic.Bool
	session := &stagedSession{}
	session.available.Store(true)
	connection := &Connection{
		ctx: t.Context(), ready: &ready, session: session,
		api: tg.NewClient(rpcFixture(func(input bin.Encoder) (bin.Encoder, error) {
			if _, ok := input.(*tg.MessagesGetDialogsRequest); !ok {
				t.Fatalf("unexpected request %T", input)
			}
			ready.Store(true)
			return &tg.MessagesDialogs{
				Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChat{ChatID: 19}}},
				Chats:   []tg.ChatClass{&tg.Chat{ID: 19, Title: "Announcements", Photo: &tg.ChatPhotoEmpty{}}},
			}, nil
		})),
	}

	items, err := connection.Destinations(t.Context())
	if want := []Destination{{ID: "group:19", Title: "Announcements"}}; err != nil || !slices.Equal(items, want) {
		t.Fatalf("got %+v, %v; want %+v", items, err, want)
	}
}

func TestDestinationsReportsTelegramRateLimit(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	session := &stagedSession{}
	session.available.Store(true)
	connection := &Connection{
		ctx: t.Context(), ready: &ready, session: session,
		api: tg.NewClient(rpcFixture(func(bin.Encoder) (bin.Encoder, error) {
			return nil, tgerr.New(420, "FLOOD_WAIT_60")
		})),
	}

	if _, err := connection.Destinations(t.Context()); err == nil || !strings.Contains(err.Error(), "60 seconds") {
		t.Fatalf("expected the Telegram wait duration, got %v", err)
	}
}
