package telegram

import (
	"context"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"telegram-messages-sender/internal/content"
	"telegram-messages-sender/internal/domain"
)

type DestinationState string

const (
	DestinationVerified         DestinationState = "verified"
	DestinationAdminLost        DestinationState = "admin_lost"
	DestinationCheckUnavailable DestinationState = "check_unavailable"
	DestinationRestricted       DestinationState = "publishing_restricted"
	DestinationIdentityMissing  DestinationState = "personal_identity_unavailable"
	DestinationUnsupported      DestinationState = "unsupported_destination"
	DestinationMigrated         DestinationState = "migrated"
)

type DestinationRole string

const (
	DestinationAdmin DestinationRole = "administrator"
	DestinationOwner DestinationRole = "owner"
)

type DestinationCheck struct {
	State       DestinationState
	Role        DestinationRole
	SendAs      tg.InputPeerClass
	Replacement *tg.InputPeerChannel
	Wait        *ServerWait
}

type destinationAPI interface {
	MessagesGetFullChat(context.Context, int64) (*tg.MessagesChatFull, error)
	ChannelsGetChannels(context.Context, []tg.InputChannelClass) (tg.MessagesChatsClass, error)
	ChannelsGetParticipant(context.Context, *tg.ChannelsGetParticipantRequest) (*tg.ChannelsChannelParticipant, error)
	ChannelsGetSendAs(context.Context, *tg.ChannelsGetSendAsRequest) (*tg.ChannelsSendAsPeers, error)
}

func (c *Connection) CheckDestination(ctx context.Context, peer tg.InputPeerClass, kind content.Kind) DestinationCheck {
	if !c.Status().TransportAvailable {
		return DestinationCheck{State: DestinationCheckUnavailable}
	}
	result := checkDestination(ctx, c.api, c.account, peer, kind, time.Now)
	if result.State != DestinationAdminLost && (ctx.Err() != nil || !c.Status().TransportAvailable) {
		return DestinationCheck{State: DestinationCheckUnavailable, Wait: result.Wait}
	}
	return result
}

func checkDestination(ctx context.Context, api destinationAPI, account domain.AccountID, peer tg.InputPeerClass, kind content.Kind, now func() time.Time) DestinationCheck {
	switch kind {
	case content.Text, content.Photo, content.Video:
	default:
		return DestinationCheck{State: DestinationRestricted}
	}

	switch peer := peer.(type) {
	case *tg.InputPeerChat:
		if peer.ChatID <= 0 {
			return DestinationCheck{State: DestinationUnsupported}
		}
		return checkBasicGroup(ctx, api, account, peer.ChatID)
	case *tg.InputPeerChannel:
		if peer.ChannelID <= 0 {
			return DestinationCheck{State: DestinationUnsupported}
		}
		return checkSupergroup(ctx, api, account, peer, kind, now)
	default:
		return DestinationCheck{State: DestinationUnsupported}
	}
}

func checkBasicGroup(ctx context.Context, api destinationAPI, account domain.AccountID, id int64) DestinationCheck {
	unavailable := DestinationCheck{State: DestinationCheckUnavailable}
	response, err := api.MessagesGetFullChat(ctx, id)
	if err != nil || response == nil {
		unavailable.Wait = waitFromError(err)
		return unavailable
	}
	var chat *tg.Chat
	for _, peer := range response.Chats {
		if group, ok := peer.(*tg.Chat); ok && group.ID == id {
			chat = group
			break
		}
		if group, ok := peer.(*tg.ChatForbidden); ok && group.ID == id {
			return DestinationCheck{State: DestinationAdminLost}
		}
	}
	if chat == nil {
		return unavailable
	}
	if chat.MigratedTo != nil {
		result := DestinationCheck{State: DestinationMigrated}
		if target, ok := chat.MigratedTo.(*tg.InputChannel); ok {
			result.Replacement = &tg.InputPeerChannel{ChannelID: target.ChannelID, AccessHash: target.AccessHash}
		}
		return result
	}
	if chat.Left {
		return DestinationCheck{State: DestinationAdminLost}
	}
	if chat.Deactivated {
		return DestinationCheck{State: DestinationUnsupported}
	}
	full, ok := response.FullChat.(*tg.ChatFull)
	if !ok || full.ID != id {
		return unavailable
	}

	result := basicGroupRole(full.Participants, id, int64(account))
	if result.State == DestinationVerified && chat.AdminRights.Anonymous {
		result.State = DestinationIdentityMissing
	}
	return result
}

func basicGroupRole(participants tg.ChatParticipantsClass, id, self int64) DestinationCheck {
	switch participants := participants.(type) {
	case *tg.ChatParticipants:
		if participants.ChatID == id {
			for _, participant := range participants.Participants {
				if participant.GetUserID() == self {
					return basicParticipantRole(participant)
				}
			}
			return DestinationCheck{State: DestinationAdminLost}
		}
	case *tg.ChatParticipantsForbidden:
		if participants.ChatID == id && participants.SelfParticipant != nil && participants.SelfParticipant.GetUserID() == self {
			return basicParticipantRole(participants.SelfParticipant)
		}
	}
	return DestinationCheck{State: DestinationCheckUnavailable}
}

func basicParticipantRole(participant tg.ChatParticipantClass) DestinationCheck {
	switch participant.(type) {
	case *tg.ChatParticipantCreator:
		return DestinationCheck{State: DestinationVerified, Role: DestinationOwner}
	case *tg.ChatParticipantAdmin:
		return DestinationCheck{State: DestinationVerified, Role: DestinationAdmin}
	case *tg.ChatParticipant:
		return DestinationCheck{State: DestinationAdminLost}
	default:
		return DestinationCheck{State: DestinationCheckUnavailable}
	}
}

func checkSupergroup(ctx context.Context, api destinationAPI, account domain.AccountID, peer *tg.InputPeerChannel, kind content.Kind, now func() time.Time) DestinationCheck {
	unavailable := DestinationCheck{State: DestinationCheckUnavailable}
	channel := &tg.InputChannel{ChannelID: peer.ChannelID, AccessHash: peer.AccessHash}
	chats, err := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{channel})
	if tgerr.Is(err, "CHANNEL_PRIVATE") {
		return DestinationCheck{State: DestinationAdminLost}
	}
	if err != nil {
		unavailable.Wait = waitFromError(err)
		return unavailable
	}
	response, ok := chats.(interface{ GetChats() []tg.ChatClass })
	if !ok {
		return unavailable
	}
	var group *tg.Channel
	for _, chat := range response.GetChats() {
		if candidate, ok := chat.(*tg.Channel); ok && candidate.ID == peer.ChannelID && !candidate.Min {
			group = candidate
			break
		}
		if candidate, ok := chat.(*tg.ChannelForbidden); ok && candidate.ID == peer.ChannelID {
			switch {
			case candidate.Broadcast || candidate.Monoforum:
				return DestinationCheck{State: DestinationUnsupported}
			case candidate.Megagroup:
				return DestinationCheck{State: DestinationAdminLost}
			}
		}
	}
	if group == nil {
		return unavailable
	}
	if !group.Megagroup || group.Broadcast || group.Forum || group.Monoforum {
		return DestinationCheck{State: DestinationUnsupported}
	}
	if group.Left {
		return DestinationCheck{State: DestinationAdminLost}
	}

	self := &tg.InputPeerSelf{}
	participant, err := api.ChannelsGetParticipant(ctx, &tg.ChannelsGetParticipantRequest{Channel: channel, Participant: self})
	if tgerr.Is(err, "USER_NOT_PARTICIPANT", "CHANNEL_PRIVATE") {
		return DestinationCheck{State: DestinationAdminLost}
	}
	if err != nil || participant == nil {
		unavailable.Wait = waitFromError(err)
		return unavailable
	}

	result := supergroupRole(participant.Participant, int64(account))
	if result.State != DestinationVerified {
		return result
	}
	if group.AdminRights.Anonymous {
		result.State = DestinationIdentityMissing
		return result
	}
	if restricted(group.BannedRights, kind, now()) {
		result.State = DestinationRestricted
		return result
	}

	senders, err := api.ChannelsGetSendAs(ctx, &tg.ChannelsGetSendAsRequest{Peer: peer})
	if err != nil || senders == nil {
		result.State = DestinationCheckUnavailable
		result.Wait = waitFromError(err)
		return result
	}
	for _, sender := range senders.Peers {
		if personalPeer(sender.Peer, account) && eligibleSender(sender, senders.Users, account) {
			result.SendAs = self
			return result
		}
	}
	result.State = DestinationIdentityMissing
	return result
}

func supergroupRole(participant tg.ChannelParticipantClass, self int64) DestinationCheck {
	switch p := participant.(type) {
	case *tg.ChannelParticipantCreator:
		if p.UserID == self {
			return supergroupAdmin(DestinationOwner, p.AdminRights)
		}
	case *tg.ChannelParticipantAdmin:
		if p.UserID == self {
			return supergroupAdmin(DestinationAdmin, p.AdminRights)
		}
	case *tg.ChannelParticipant:
		if p.UserID == self {
			return DestinationCheck{State: DestinationAdminLost}
		}
	case *tg.ChannelParticipantSelf:
		if p.UserID == self {
			return DestinationCheck{State: DestinationAdminLost}
		}
	case *tg.ChannelParticipantBanned:
		if personalPeer(p.Peer, domain.AccountID(self)) {
			return DestinationCheck{State: DestinationAdminLost}
		}
	case *tg.ChannelParticipantLeft:
		if personalPeer(p.Peer, domain.AccountID(self)) {
			return DestinationCheck{State: DestinationAdminLost}
		}
	}
	return DestinationCheck{State: DestinationCheckUnavailable}
}

func supergroupAdmin(role DestinationRole, rights tg.ChatAdminRights) DestinationCheck {
	if rights.Anonymous {
		return DestinationCheck{State: DestinationIdentityMissing, Role: role}
	}
	return DestinationCheck{State: DestinationVerified, Role: role}
}

func eligibleSender(sender tg.SendAsPeer, users []tg.UserClass, account domain.AccountID) bool {
	if !sender.PremiumRequired {
		return true
	}
	for _, candidate := range users {
		if user, ok := candidate.(*tg.User); ok && user.ID == int64(account) {
			return user.Premium
		}
	}
	return false
}

func personalPeer(peer tg.PeerClass, account domain.AccountID) bool {
	user, ok := peer.(*tg.PeerUser)
	return ok && user.UserID == int64(account)
}

func restricted(rights tg.ChatBannedRights, kind content.Kind, now time.Time) bool {
	if rights.UntilDate != 0 && int64(rights.UntilDate) <= now.Unix() {
		return false
	}
	if rights.ViewMessages || rights.SendMessages {
		return true
	}
	switch kind {
	case content.Text:
		return rights.SendPlain
	case content.Photo:
		return rights.SendMedia || rights.SendPhotos
	case content.Video:
		return rights.SendMedia || rights.SendVideos
	}
	return true
}
