// Package friends implements nn.npln.friends.v1.Friends and
// nn.npln.friends.v1.PresenceService.
//
// # Where the friend graph comes from
//
// Nowhere in this server. Nextendo has ONE friend graph, owned by
// nextendo-account and shared by the website, the Switch home menu and every
// game. This service translates it into the NPLN shape Splatoon 3 expects:
//
//	nextendo-account /internal/npln-friends?pid=…
//	  -> { user_id, account_hex, friends:[{pid,user_id,account_hex,name,presence}] }
//	  -> FriendUser { name, friend_user, nsa_id, relationship }
//
// # Why the relationship flags are set the way they are
//
// FriendUser.Relationship carries presence_deliverable / presence_receivable.
// If they are false the client will not SHOW a friend's presence and will not
// PUBLISH its own to them — the friend list then looks correct while everybody
// appears permanently offline. Nextendo friendships are mutual and there is no
// per-friend presence privacy setting in the account model, so both flags are
// true for an accepted friend. This is one of the two halves of the "friends
// never appear online" bug (docs/FRIENDS.md); the other is that somebody has to
// report presence in the first place, which internal/presence does.
package friends

import (
	"context"
	"log"
	"sort"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	friendsv1 "github.com/n-popescu/splatoon-3/gen/npln/friends/v1"
	"github.com/n-popescu/splatoon-3/internal/account"
	"github.com/n-popescu/splatoon-3/internal/names"
	"github.com/n-popescu/splatoon-3/internal/nplnerr"
	"github.com/n-popescu/splatoon-3/internal/presence"
	"github.com/n-popescu/splatoon-3/internal/server"
)

// Service implements the Friends service.
type Service struct {
	names    names.Builder
	accounts *account.Client
	hub      *presence.Hub
	// pollInterval is how often an open subscription re-reads the friend graph.
	// The graph changes rarely (a friend request accepted on the website or on
	// the console), so a poll is enough and needs no push channel from the
	// account server.
	pollInterval time.Duration
}

// Options configures the service.
type Options struct {
	Names        names.Builder
	Accounts     *account.Client
	Hub          *presence.Hub
	PollInterval time.Duration
}

// New builds the Friends service.
func New(o Options) *Service {
	if o.PollInterval <= 0 {
		o.PollInterval = 15 * time.Second
	}
	return &Service{names: o.Names, accounts: o.Accounts, hub: o.Hub, pollInterval: o.PollInterval}
}

// ActivateUser tells the server the player wants to take part in the friend /
// presence system for this title.
//
// Retail NPLN uses it to start delivering presence for that user. We use it as
// the point where the player becomes ONLINE for their friends, so a friend sees
// them the moment they enter online play — before any lobby exists.
func (s *Service) ActivateUser(ctx context.Context, req *friendsv1.ActivateUserRequest) (*friendsv1.ActivateUserResponse, error) {
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetName() != "" {
		userID, err := s.names.UserID(req.GetName())
		if err != nil {
			return nil, nplnerr.InvalidArgument(err.Error())
		}
		if userID != caller.UserID {
			return nil, nplnerr.UserMismatch("cannot activate another user")
		}
	}
	s.hub.Set(caller.UserID, caller.PID, friendsv1.State_ONLINE, nil)
	log.Printf("[friends] ActivateUser pid=%d user=%s -> ONLINE", caller.PID, caller.UserID)
	return &friendsv1.ActivateUserResponse{}, nil
}

// ListFriendUsers returns the caller's friends as NPLN users.
func (s *Service) ListFriendUsers(ctx context.Context, req *friendsv1.ListFriendUsersRequest) (*friendsv1.ListFriendUsersResponse, error) {
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetParent() != "" {
		userID, err := s.names.UserID(req.GetParent())
		if err != nil {
			return nil, nplnerr.InvalidArgument(err.Error())
		}
		// A player may only list THEIR OWN friends. Serving somebody else's on
		// request would leak the whole social graph to any authenticated client.
		if userID != caller.UserID {
			return nil, nplnerr.PermissionDenied("cannot list another user's friends")
		}
	}
	graph, err := s.accounts.NplnFriends(ctx, caller.PID)
	if err != nil {
		log.Printf("[friends] ListFriendUsers pid=%d: account server unreachable: %v", caller.PID, err)
		return nil, nplnerr.Unavailable("the account service is unreachable")
	}
	friendUsers := s.friendUsers(caller.UserID, graph.Friends)
	log.Printf("[friends] ListFriendUsers pid=%d -> %d friend(s)", caller.PID, len(friendUsers))
	return &friendsv1.ListFriendUsersResponse{FriendUsers: friendUsers}, nil
}

// ListBlockingUsers returns the users the caller has blocked.
//
// The account server owns the block list too. An account server that does not
// publish it yet simply yields an empty list, which is the safe answer: an empty
// block list never wrongly hides a friend.
func (s *Service) ListBlockingUsers(ctx context.Context, req *friendsv1.ListBlockingUsersRequest) (*friendsv1.ListBlockingUsersResponse, error) {
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	graph, err := s.accounts.NplnFriends(ctx, caller.PID)
	if err != nil {
		return nil, nplnerr.Unavailable("the account service is unreachable")
	}
	out := make([]*friendsv1.BlockingUser, 0, len(graph.Blocked))
	for _, b := range graph.Blocked {
		out = append(out, &friendsv1.BlockingUser{
			Name:         s.names.User(caller.UserID) + "/blockingUsers/" + b.UserID,
			BlockingUser: s.names.User(b.UserID),
			NsaId:        b.AccountHex,
		})
	}
	return &friendsv1.ListBlockingUsersResponse{BlockingUsers: out}, nil
}

// SubscribeFriendUsers streams the friend graph: a full snapshot immediately,
// then a new one whenever it changes, plus the keep-alive interval the client
// should expect.
//
// The console keeps this stream open for the whole session. Answering it once
// and closing (or never answering) is what makes a friend list that never
// updates — a friend added mid-session would not appear until the game is
// restarted.
func (s *Service) SubscribeFriendUsers(req *friendsv1.SubscribeFriendUsersRequest, stream friendsv1.Friends_SubscribeFriendUsersServer) error {
	ctx := stream.Context()
	caller, err := s.caller(ctx)
	if err != nil {
		return err
	}
	if req.GetUser() != "" {
		if userID, err := s.names.UserID(req.GetUser()); err == nil && userID != caller.UserID {
			return nplnerr.UserMismatch("cannot subscribe to another user's friends")
		}
	}

	// keepAliveInterval is what the client is told to expect between messages.
	// We send a snapshot at least that often, so the stream doubles as its own
	// keep-alive and the console never times it out.
	keepAlive := 60 * time.Second

	var lastFingerprint string
	send := func() error {
		graph, err := s.accounts.NplnFriends(ctx, caller.PID)
		if err != nil {
			// A hiccup on the account server must not tear the stream down: the
			// console would treat that as "friends unavailable" and stop asking.
			log.Printf("[friends] subscribe pid=%d: %v (keeping the stream open)", caller.PID, err)
			return nil
		}
		friendUsers := s.friendUsers(caller.UserID, graph.Friends)
		fp := fingerprint(friendUsers)
		if fp == lastFingerprint {
			return nil
		}
		lastFingerprint = fp

		// NPLN groups friends by ACCOUNT (one console account can hold several
		// users of the same tenant). A Nextendo account maps to exactly one
		// account and one user, so each group has a single entry — but the
		// grouping is still built properly, because the client indexes on nsa_id.
		accounts := make([]*friendsv1.SubscribeFriendUsersResponse_FriendAccount, 0, len(friendUsers))
		for _, fu := range friendUsers {
			accounts = append(accounts, &friendsv1.SubscribeFriendUsersResponse_FriendAccount{
				NsaId: fu.GetNsaId(),
				Users: []*friendsv1.FriendUser{fu},
			})
		}
		log.Printf("[friends] subscribe pid=%d -> pushing %d friend account(s)", caller.PID, len(accounts))
		return stream.Send(&friendsv1.SubscribeFriendUsersResponse{
			FriendAccounts:    accounts,
			KeepAliveInterval: durationpb.New(keepAlive),
		})
	}

	if err := send(); err != nil {
		return err
	}
	poll := time.NewTicker(s.pollInterval)
	defer poll.Stop()
	beat := time.NewTicker(keepAlive)
	defer beat.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-poll.C:
			// Any RPC from this player also proves they are still here.
			s.hub.Touch(caller.UserID, caller.PID)
			if err := send(); err != nil {
				return err
			}
		case <-beat.C:
			// Force a send even when nothing changed, so the stream stays warm.
			lastFingerprint = ""
			if err := send(); err != nil {
				return err
			}
		}
	}
}

// friendUsers converts account-server friends into NPLN FriendUser resources.
func (s *Service) friendUsers(selfUserID string, friends []account.Friend) []*friendsv1.FriendUser {
	out := make([]*friendsv1.FriendUser, 0, len(friends))
	for _, f := range friends {
		if f.UserID == "" {
			// The account server could not derive an NPLN identity for this
			// friend. Skipping is right: an entry with an empty name would make
			// the client reject the whole list.
			log.Printf("[friends] skipping friend pid=%d: no NPLN user id from the account server", f.PID)
			continue
		}
		out = append(out, &friendsv1.FriendUser{
			Name:       s.names.FriendUser(selfUserID, f.UserID),
			FriendUser: s.names.User(f.UserID),
			NsaId:      f.AccountHex,
			Relationship: &friendsv1.FriendUser_Relationship{
				Favorite: f.Favorite,
				// Both directions ON: see the package comment. A Nextendo
				// friendship is mutual and carries no presence privacy switch,
				// so refusing to deliver or receive presence would only produce
				// the "everybody is offline" bug.
				PresenceDeliverable: true,
				PresenceReceivable:  true,
			},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetFriendUser() < out[j].GetFriendUser() })
	return out
}

// caller returns the authenticated caller, refusing the anonymous user (retail
// NPLN denies the social services to it, and so do we).
func (s *Service) caller(ctx context.Context) (*server.Caller, error) {
	c, ok := server.CallerFrom(ctx)
	if !ok {
		return nil, nplnerr.TokenInvalid("no access token")
	}
	if c.Anonymous {
		return nil, nplnerr.PermissionDenied("the anonymous user has no friends")
	}
	if c.PID == 0 {
		return nil, nplnerr.InvalidAccount("this token carries no Nextendo account")
	}
	return c, nil
}

// fingerprint is a cheap change detector for a friend list, so an unchanged
// graph does not produce a stream message every poll.
func fingerprint(list []*friendsv1.FriendUser) string {
	b := make([]byte, 0, len(list)*48)
	for _, f := range list {
		b = append(b, f.GetFriendUser()...)
		b = append(b, '|')
		b = append(b, f.GetNsaId()...)
		if r := f.GetRelationship(); r != nil && r.GetFavorite() {
			b = append(b, '*')
		}
		b = append(b, ';')
	}
	return string(b)
}
