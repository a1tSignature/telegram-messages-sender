package telegram

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"telegram-messages-sender/internal/content"
)

type destinationFixture struct {
	full        *tg.MessagesChatFull
	group       tg.ChatClass
	groupErr    error
	participant tg.ChannelParticipantClass
	memberErr   error
	senders     *tg.ChannelsSendAsPeers
	sendersErr  error
}

func (f destinationFixture) MessagesGetFullChat(_ context.Context, _ int64) (*tg.MessagesChatFull, error) {
	return f.full, nil
}

func (f destinationFixture) ChannelsGetChannels(_ context.Context, _ []tg.InputChannelClass) (tg.MessagesChatsClass, error) {
	return &tg.MessagesChats{Chats: []tg.ChatClass{f.group}}, f.groupErr
}

func (f destinationFixture) ChannelsGetParticipant(_ context.Context, _ *tg.ChannelsGetParticipantRequest) (*tg.ChannelsChannelParticipant, error) {
	return &tg.ChannelsChannelParticipant{Participant: f.participant}, f.memberErr
}

func (f destinationFixture) ChannelsGetSendAs(_ context.Context, _ *tg.ChannelsGetSendAsRequest) (*tg.ChannelsSendAsPeers, error) {
	return f.senders, f.sendersErr
}

func TestBasicGroupVerification(t *testing.T) {
	chat := &tg.Chat{ID: 19}
	report := func(group *tg.Chat, members tg.ChatParticipantsClass) destinationFixture {
		return destinationFixture{full: &tg.MessagesChatFull{
			Chats:    []tg.ChatClass{group},
			FullChat: &tg.ChatFull{ID: group.ID, Participants: members},
		}}
	}
	members := func(member tg.ChatParticipantClass) *tg.ChatParticipants {
		return &tg.ChatParticipants{ChatID: 19, Participants: []tg.ChatParticipantClass{member}}
	}
	migrated := &tg.Chat{ID: 19, Deactivated: true, MigratedTo: &tg.InputChannel{ChannelID: 101, AccessHash: 202}}
	for _, tt := range []struct {
		name string
		api  destinationFixture
		want DestinationState
		role DestinationRole
	}{
		{"creator", report(chat, members(&tg.ChatParticipantCreator{UserID: 42})), DestinationVerified, DestinationOwner},
		{"administrator", report(chat, members(&tg.ChatParticipantAdmin{UserID: 42})), DestinationVerified, DestinationAdmin},
		{"ordinary member", report(chat, members(&tg.ChatParticipant{UserID: 42})), DestinationAdminLost, ""},
		{"only another administrator", report(chat, members(&tg.ChatParticipantAdmin{UserID: 12})), DestinationAdminLost, ""},
		{"matching forbidden group", destinationFixture{full: &tg.MessagesChatFull{
			Chats: []tg.ChatClass{&tg.ChatForbidden{ID: 19}},
		}}, DestinationAdminLost, ""},
		{"different forbidden group", destinationFixture{full: &tg.MessagesChatFull{
			Chats: []tg.ChatClass{&tg.ChatForbidden{ID: 55}},
		}}, DestinationCheckUnavailable, ""},
		{"unavailable", report(chat, &tg.ChatParticipantsForbidden{ChatID: 19}), DestinationCheckUnavailable, ""},
		{"self role despite hidden list", report(chat, &tg.ChatParticipantsForbidden{
			ChatID: 19, SelfParticipant: &tg.ChatParticipantAdmin{UserID: 42},
		}), DestinationVerified, DestinationAdmin},
		{"migrated despite stale creator status", report(migrated, members(&tg.ChatParticipantCreator{UserID: 42})), DestinationMigrated, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := checkDestination(t.Context(), tt.api, 42, &tg.InputPeerChat{ChatID: 19}, content.Text, time.Now)
			if got.State != tt.want || got.Role != tt.role {
				t.Fatalf("got state %q role %q, want %q %q", got.State, got.Role, tt.want, tt.role)
			}
			if tt.want == DestinationMigrated && (got.Replacement == nil || got.Replacement.ChannelID != 101 || got.Replacement.AccessHash != 202) {
				t.Fatal("migration must return the replacement peer for a fresh check")
			}
		})
	}
}

func TestSupergroupVerification(t *testing.T) {
	admin := &tg.ChannelParticipantAdmin{UserID: 42}
	owner := &tg.ChannelParticipantCreator{UserID: 42}
	anonymous := &tg.ChannelParticipantAdmin{UserID: 42, AdminRights: tg.ChatAdminRights{Anonymous: true}}
	self := &tg.ChannelsSendAsPeers{Peers: []tg.SendAsPeer{{Peer: &tg.PeerUser{UserID: 42}}}}
	onlyChannel := &tg.ChannelsSendAsPeers{Peers: []tg.SendAsPeer{{Peer: &tg.PeerChannel{ChannelID: 10}}}}
	group := &tg.Channel{ID: 101, Megagroup: true, DefaultBannedRights: tg.ChatBannedRights{SendMedia: true}}
	individuallyRestricted := &tg.Channel{ID: 101, Megagroup: true, BannedRights: tg.ChatBannedRights{SendPhotos: true}}
	forum := &tg.Channel{ID: 101, Megagroup: true, Forum: true}
	broadcast := &tg.Channel{ID: 101, Broadcast: true}
	unexpected := &tg.ChannelParticipantAdmin{UserID: 12}
	missing := tgerr.New(400, "USER_NOT_PARTICIPANT")
	private := tgerr.New(406, "CHANNEL_PRIVATE")
	invalid := tgerr.New(400, "CHANNEL_INVALID")
	forbidden := &tg.ChannelForbidden{ID: 101, Megagroup: true}
	noResponse := errors.New("offline")
	for _, tt := range []struct {
		name string
		api  destinationFixture
		kind content.Kind
		want DestinationState
		role DestinationRole
	}{
		{"verified admin can post despite member defaults", destinationFixture{group: group, participant: admin, senders: self}, content.Photo, DestinationVerified, DestinationAdmin},
		{"owner", destinationFixture{group: group, participant: owner, senders: self}, content.Video, DestinationVerified, DestinationOwner},
		{"non-member", destinationFixture{group: group, memberErr: missing}, content.Text, DestinationAdminLost, ""},
		{"matching forbidden supergroup", destinationFixture{group: forbidden}, content.Text, DestinationAdminLost, ""},
		{"different forbidden supergroup", destinationFixture{group: &tg.ChannelForbidden{ID: 3, Megagroup: true}}, content.Text, DestinationCheckUnavailable, ""},
		{"forbidden broadcast", destinationFixture{group: &tg.ChannelForbidden{ID: 101, Broadcast: true}}, content.Text, DestinationUnsupported, ""},
		{"no type in forbidden metadata", destinationFixture{group: &tg.ChannelForbidden{ID: 101}}, content.Text, DestinationCheckUnavailable, ""},
		{"private on group lookup", destinationFixture{groupErr: private}, content.Text, DestinationAdminLost, ""},
		{"private on self participant lookup", destinationFixture{group: group, memberErr: private}, content.Text, DestinationAdminLost, ""},
		{"invalid channel", destinationFixture{groupErr: invalid}, content.Text, DestinationCheckUnavailable, ""},
		{"ordinary member", destinationFixture{group: group, participant: &tg.ChannelParticipantSelf{UserID: 42}}, content.Text, DestinationAdminLost, ""},
		{"other user response", destinationFixture{group: group, participant: unexpected}, content.Text, DestinationCheckUnavailable, ""},
		{"participant failed", destinationFixture{group: group, memberErr: noResponse}, content.Text, DestinationCheckUnavailable, ""},
		{"identity list failed", destinationFixture{group: group, participant: admin, sendersErr: noResponse}, content.Text, DestinationCheckUnavailable, DestinationAdmin},
		{"anonymous", destinationFixture{group: group, participant: anonymous}, content.Text, DestinationIdentityMissing, DestinationAdmin},
		{"channel identity only", destinationFixture{group: group, participant: admin, senders: onlyChannel}, content.Text, DestinationIdentityMissing, DestinationAdmin},
		{"photos restricted", destinationFixture{group: individuallyRestricted, participant: admin}, content.Photo, DestinationRestricted, DestinationAdmin},
		{"video allowed with photos restricted", destinationFixture{group: individuallyRestricted, participant: admin, senders: self}, content.Video, DestinationVerified, DestinationAdmin},
		{"forum", destinationFixture{group: forum, participant: admin}, content.Text, DestinationUnsupported, ""},
		{"broadcast", destinationFixture{group: broadcast, participant: admin}, content.Text, DestinationUnsupported, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := checkDestination(t.Context(), tt.api, 42, &tg.InputPeerChannel{ChannelID: 101}, tt.kind, time.Now)
			if got.State != tt.want || got.Role != tt.role {
				t.Fatalf("got state %q role %q, want %q %q", got.State, got.Role, tt.want, tt.role)
			}
			_, self := got.SendAs.(*tg.InputPeerSelf)
			if self != (tt.want == DestinationVerified) {
				t.Fatal("only a verified personal identity can supply an explicit sender")
			}
		})
	}
}

func TestConfirmedDestinationLossSurvivesCancellationDuringRPC(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var ready atomic.Bool
	ready.Store(true)
	saved := &stagedSession{dc: 2}
	if err := saved.commit(t.Context(), nil, 2); err != nil {
		t.Fatal(err)
	}
	connection := &Connection{
		ctx: t.Context(), account: 42, ready: &ready, session: saved,
		api: tg.NewClient(rpcFixture(func(bin.Encoder) (bin.Encoder, error) {
			cancel()
			return &tg.MessagesChatFull{
				FullChat: &tg.ChatFull{ID: 12, Participants: &tg.ChatParticipantsForbidden{ChatID: 12}},
				Chats:    []tg.ChatClass{&tg.ChatForbidden{ID: 12}},
			}, nil
		})),
	}
	result := connection.CheckDestination(ctx, &tg.InputPeerChat{ChatID: 12}, content.Text)
	if result.State != DestinationAdminLost {
		t.Fatalf("completed loss was discarded after cancellation: %+v", result)
	}
}
