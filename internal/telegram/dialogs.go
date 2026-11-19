package telegram

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/gotd/td/telegram/query/dialogs"
	"github.com/gotd/td/tg"

	"telegram-messages-sender/internal/domain"
)

type Destination struct {
	ID    domain.ChatID
	Title string
}

func (c *Connection) Destinations(ctx context.Context) ([]Destination, error) {
	if !c.Status().TransportAvailable {
		return nil, ErrTransport
	}
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
