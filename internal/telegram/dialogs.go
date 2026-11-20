package telegram

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gotd/td/telegram/query/dialogs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"telegram-messages-sender/internal/domain"
)

type Destination struct {
	ID    domain.ChatID
	Title string
}

func (c *Connection) Destinations(ctx context.Context) ([]Destination, error) {
	if c.ctx.Err() != nil {
		return nil, ErrTransport
	}
	// gotd can wait for a reconnect within the caller's deadline for this read.
	var result []Destination
	iter := dialogs.NewQueryBuilder(c.api).GetDialogs().BatchSize(100).Iter()
	seen := make(map[domain.ChatID]bool)
	for iter.Next(ctx) {
		dialog := iter.Value()
		var item Destination
		switch peer := dialog.Peer.(type) {
		case *tg.InputPeerChat:
			group := dialog.Entities.Chats()[peer.ChatID]
			if group != nil && !group.Left && !group.Deactivated && group.MigratedTo == nil {
				item = Destination{ID: domain.ChatID(fmt.Sprintf("group:%d", peer.ChatID)), Title: group.Title}
			}
		case *tg.InputPeerChannel:
			group := dialog.Entities.Channels()[peer.ChannelID]
			if group != nil && group.Megagroup && !group.Left && !group.Forum && !group.Monoforum && !group.Broadcast && peer.AccessHash != 0 {
				item = Destination{ID: domain.ChatID(fmt.Sprintf("supergroup:%d", peer.ChannelID)), Title: group.Title}
				c.sendPeers.Store(item.ID, peer)
			}
		}
		if item.ID != "" && !seen[item.ID] {
			seen[item.ID] = true
			result = append(result, item)
		}
	}
	if err := iter.Err(); err != nil {
		if wait, ok := tgerr.AsFloodWait(err); ok && ctx.Err() == nil {
			return nil, fmt.Errorf("Telegram rate limit reached. Try loading groups again in %d seconds", wait/time.Second)
		}
		return nil, connectionError(ctx, err)
	}
	slices.SortFunc(result, func(a, b Destination) int {
		if order := strings.Compare(a.Title, b.Title); order != 0 {
			return order
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})
	return result, nil
}
