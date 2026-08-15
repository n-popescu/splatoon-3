package friends

import (
	"context"
	"errors"
	"io"
	"log"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	friendsv1 "github.com/n-popescu/splatoon-3/gen/npln/friends/v1"
	"github.com/n-popescu/splatoon-3/internal/account"
	"github.com/n-popescu/splatoon-3/internal/names"
	"github.com/n-popescu/splatoon-3/internal/nplnerr"
	"github.com/n-popescu/splatoon-3/internal/presence"
	"github.com/n-popescu/splatoon-3/internal/server"
)

// PresenceService implements nn.npln.friends.v1.PresenceService.
//
// Two streams, and the game depends on both:
//
//	KeepAlive           bidirectional. The client pushes ITS OWN presence
//	                    (state + game attributes) and we answer heartbeats. This
//	                    is the only place a player's own presence comes from,
//	                    and it doubles as the "am I still online" signal.
//	SubscribePresences  server-streaming. The client asks for the presence of a
//	                    list of users (its friends), gets them, gets an
//	                    "enumeration done" marker, and then receives updates as
//	                    they happen, interleaved with heartbeats.
//
// The heartbeat interval matters: the client uses it to decide the stream is
// dead. Never sending one makes presence stop working a minute into the session
// with nothing in the log to explain it.
type PresenceService struct {
	names    names.Builder
	hub      *presence.Hub
	accounts *account.Client

	heartbeat    time.Duration
	pollInterval time.Duration
}

// PresenceOptions configures the presence service.
type PresenceOptions struct {
	Names    names.Builder
	Hub      *presence.Hub
	Accounts *account.Client
	// Heartbeat is the interval advertised to the client, and how often we send
	// a heartbeat message on an otherwise idle stream.
	Heartbeat time.Duration
	// PollInterval is how often a subscription re-reads the account server's
	// network-wide presence, so "friend online in another game" also shows up.
	PollInterval time.Duration
}

// NewPresence builds the presence service.
func NewPresence(o PresenceOptions) *PresenceService {
	if o.Heartbeat <= 0 {
		o.Heartbeat = 30 * time.Second
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 15 * time.Second
	}
	return &PresenceService{
		names:        o.Names,
		hub:          o.Hub,
		accounts:     o.Accounts,
		heartbeat:    o.Heartbeat,
		pollInterval: o.PollInterval,
	}
}

// KeepAlive receives the player's own presence and answers with heartbeats.
func (s *PresenceService) KeepAlive(stream friendsv1.PresenceService_KeepAliveServer) error {
	ctx := stream.Context()
	caller, err := s.caller(ctx)
	if err != nil {
		return err
	}

	// Tell the client how often to talk to us, straight away: it will not send
	// its first presence update until it knows.
	if err := stream.Send(&friendsv1.KeepAliveResponse{Heartbeat: &friendsv1.Heartbeat{Interval: durationpb.New(s.heartbeat)}}); err != nil {
		return err
	}

	// A player with an open KeepAlive stream is online, even before their first
	// UpdatePresence — that is what "keep alive" means.
	s.hub.Set(caller.UserID, caller.PID, friendsv1.State_ONLINE, nil)
	// When the stream ends the player left online play (or crashed): mark them
	// offline immediately rather than making their friends wait out the TTL.
	defer func() {
		s.hub.Offline(caller.UserID)
		log.Printf("[presence] KeepAlive closed pid=%d user=%s -> OFFLINE", caller.PID, caller.UserID)
	}()

	// Heartbeats are sent from a second goroutine, because the receive loop
	// below blocks on the client.
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(s.heartbeat)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if err := stream.Send(&friendsv1.KeepAliveResponse{Heartbeat: &friendsv1.Heartbeat{Interval: durationpb.New(s.heartbeat)}}); err != nil {
					return
				}
			}
		}
	}()

	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch body := req.GetRequest().(type) {
		case *friendsv1.KeepAliveRequest_UpdatePresence_:
			p := body.UpdatePresence.GetPresence()
			// The update_mask tells us which fields the client is actually
			// setting. We honour it loosely: an absent attributes field means
			// "unchanged", never "clear" (see presence.Hub.Set).
			state := p.GetState()
			attrs := p.GetAttributes()
			s.hub.Set(caller.UserID, caller.PID, state, attrs)
			log.Printf("[presence] pid=%d user=%s state=%s attrs=%d", caller.PID, caller.UserID, state, len(attrs))
		case *friendsv1.KeepAliveRequest_Ack:
			// The client acknowledging our heartbeat: proof of life, nothing
			// more to store.
			s.hub.Touch(caller.UserID, caller.PID)
		default:
			// An empty keepalive is still proof of life.
			s.hub.Touch(caller.UserID, caller.PID)
		}
	}
}

// SubscribePresences streams the presence of the requested users.
func (s *PresenceService) SubscribePresences(req *friendsv1.SubscribePresencesRequest, stream friendsv1.PresenceService_SubscribePresencesServer) error {
	ctx := stream.Context()
	caller, err := s.caller(ctx)
	if err != nil {
		return err
	}
	if req.GetUser() != "" {
		if userID, err := s.names.UserID(req.GetUser()); err == nil && userID != caller.UserID {
			return nplnerr.UserMismatch("cannot subscribe on behalf of another user")
		}
	}

	// The client names the presences it wants ("…/users/<id>/presence"). An
	// empty list means "my friends", which is how the game asks after it has
	// already subscribed to the friend list.
	watched := make([]string, 0, len(req.GetPresences()))
	for _, name := range req.GetPresences() {
		if userID, err := s.names.UserID(name); err == nil {
			watched = append(watched, userID)
		}
	}
	if len(watched) == 0 {
		if graph, err := s.accounts.NplnFriends(ctx, caller.PID); err == nil {
			for _, f := range graph.Friends {
				if f.UserID != "" {
					watched = append(watched, f.UserID)
				}
			}
		}
	}
	if max := int(req.GetMaxPresenceCount()); max > 0 && len(watched) > max {
		watched = watched[:max]
	}
	log.Printf("[presence] SubscribePresences pid=%d watching %d user(s)", caller.PID, len(watched))

	updates, cancel := s.hub.Subscribe(watched)
	defer cancel()

	// 1. Full enumeration of what we know right now.
	if err := s.sendSnapshot(ctx, stream, caller.PID, watched); err != nil {
		return err
	}
	// 2. The marker that tells the client the initial enumeration is complete —
	//    it waits for this before showing the friend list as loaded.
	if err := stream.Send(&friendsv1.SubscribePresencesResponse{
		Response: &friendsv1.SubscribePresencesResponse_EnumerationDone{
			EnumerationDone: &friendsv1.SubscribePresencesResponse_PresenceEnumerationDone{},
		},
	}); err != nil {
		return err
	}

	// 3. Live updates, network-wide re-reads, and heartbeats.
	poll := time.NewTicker(s.pollInterval)
	defer poll.Stop()
	beat := time.NewTicker(s.heartbeat)
	defer beat.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e, ok := <-updates:
			if !ok {
				return nil
			}
			if err := stream.Send(&friendsv1.SubscribePresencesResponse{
				Response: &friendsv1.SubscribePresencesResponse_Presences_{
					Presences: &friendsv1.SubscribePresencesResponse_Presences{
						Presences:   []*friendsv1.Presence{s.hub.Proto(e)},
						ResumeToken: resumeToken(),
					},
				},
			}); err != nil {
				return err
			}
		case <-poll.C:
			// Re-read the account server so a friend who came online in ANOTHER
			// game (or on the console itself) is reflected here too. Splatoon 3
			// only ever hears about presence through this stream.
			if err := s.sendSnapshot(ctx, stream, caller.PID, watched); err != nil {
				return err
			}
		case <-beat.C:
			if err := stream.Send(&friendsv1.SubscribePresencesResponse{
				Response: &friendsv1.SubscribePresencesResponse_Heartbeat{
					Heartbeat: &friendsv1.Heartbeat{Interval: durationpb.New(s.heartbeat)},
				},
			}); err != nil {
				return err
			}
		}
	}
}

// sendSnapshot sends the current presence of every watched user, merging this
// server's live view with the account server's network-wide view.
func (s *PresenceService) sendSnapshot(ctx context.Context, stream friendsv1.PresenceService_SubscribePresencesServer, pid uint64, watched []string) error {
	byUser := map[string]*account.Friend{}
	if graph, err := s.accounts.NplnFriends(ctx, pid); err == nil {
		for i := range graph.Friends {
			f := graph.Friends[i]
			byUser[f.UserID] = &f
		}
	}
	presences := make([]*friendsv1.Presence, 0, len(watched))
	for _, userID := range watched {
		presences = append(presences, s.hub.PresenceFor(userID, byUser[userID]))
	}
	if len(presences) == 0 {
		return nil
	}
	return stream.Send(&friendsv1.SubscribePresencesResponse{
		Response: &friendsv1.SubscribePresencesResponse_Presences_{
			Presences: &friendsv1.SubscribePresencesResponse_Presences{
				Presences:   presences,
				ResumeToken: resumeToken(),
			},
		},
	})
}

// caller mirrors Service.caller for the presence service.
func (s *PresenceService) caller(ctx context.Context) (*server.Caller, error) {
	c, ok := server.CallerFrom(ctx)
	if !ok {
		return nil, nplnerr.TokenInvalid("no access token")
	}
	if c.Anonymous {
		return nil, nplnerr.PermissionDenied("the anonymous user has no presence")
	}
	if c.PID == 0 {
		return nil, nplnerr.InvalidAccount("this token carries no Nextendo account")
	}
	return c, nil
}

// resumeToken is the cursor a client may send back to resume a presence stream.
//
// Presence is a latest-value-wins signal and every reconnect starts with a full
// enumeration, so there is nothing to resume from: a monotonic timestamp is
// enough to satisfy clients that echo the token back, without pretending to
// support replay we do not implement.
func resumeToken() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
