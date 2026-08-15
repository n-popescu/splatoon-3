// Package messaging implements nn.npln.messaging.v1.Messaging and Splatoon 3's
// own nn.npln.toyohr.v1.LobbyMessaging.
//
// Both are the same idea: a mailbox per user, drained by a long-lived
// server-stream the client keeps open, and a send RPC that drops a message into
// other users' mailboxes. This is how the game invites a friend to a lobby, how
// it tells a squad the match is starting, and how Splatoon 3's lobby chat works.
//
// # Why it is not fire-and-forget
//
// Messages carry a `message_request_id` and may ask for an acknowledgement. A
// sender that never receives an ack will keep re-sending (which is what makes a
// friend invite arrive four times), so acks are relayed back to the sender
// instead of being swallowed.
//
// # What is kept
//
// Nothing, beyond delivery. Messages live in memory, are dropped when the
// recipient's stream is not open (with a small per-user backlog for the seconds
// between a game state change and the stream reopening), and never touch disk:
// they are ephemeral game traffic, and one of them is player chat.
package messaging

import (
	"context"
	"log"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	messagingv1 "github.com/n-popescu/splatoon-3/gen/npln/messaging/v1"
	toyohrv1 "github.com/n-popescu/splatoon-3/gen/npln/toyohr/v1"
	"github.com/n-popescu/splatoon-3/internal/names"
	"github.com/n-popescu/splatoon-3/internal/nplnerr"
	"github.com/n-popescu/splatoon-3/internal/server"
	"github.com/n-popescu/splatoon-3/internal/services/matchmaking"
)

// backlogPerUser bounds the messages held for a user whose stream is closed.
// Small on purpose: a stale invite is worse than a missing one.
const backlogPerUser = 32

// item is one queued delivery: either a message or an ack.
type item struct {
	msg   *messagingv1.RecvMessage
	ack   *messagingv1.RecvAck
	lobby *toyohrv1.ReceivedMessage
	// lobbyID scopes a lobby message, so a subscriber only gets its own lobby's.
	lobbyID string
}

// hub is the mailbox set.
type hub struct {
	mu      sync.Mutex
	boxes   map[string]*mailbox
	counter uint64
}

type mailbox struct {
	// ch is the live stream, when one is open.
	ch chan item
	// backlog holds items delivered while no stream was open.
	backlog []item
}

func newHub() *hub { return &hub{boxes: map[string]*mailbox{}} }

// open attaches a stream to a user's mailbox and drains its backlog into it.
func (h *hub) open(userID string) (<-chan item, func()) {
	ch := make(chan item, backlogPerUser)
	h.mu.Lock()
	box := h.boxes[userID]
	if box == nil {
		box = &mailbox{}
		h.boxes[userID] = box
	}
	// One stream per user: a reconnecting client replaces the old one, whose
	// channel is closed by its own defer.
	box.ch = ch
	backlog := box.backlog
	box.backlog = nil
	h.mu.Unlock()

	for _, it := range backlog {
		select {
		case ch <- it:
		default:
		}
	}
	return ch, func() {
		h.mu.Lock()
		if cur := h.boxes[userID]; cur != nil && cur.ch == ch {
			cur.ch = nil
		}
		h.mu.Unlock()
		close(ch)
	}
}

// deliver puts an item in a user's mailbox.
func (h *hub) deliver(userID string, it item) {
	h.mu.Lock()
	box := h.boxes[userID]
	if box == nil {
		box = &mailbox{}
		h.boxes[userID] = box
	}
	ch := box.ch
	if ch == nil {
		if len(box.backlog) >= backlogPerUser {
			box.backlog = box.backlog[1:]
		}
		box.backlog = append(box.backlog, it)
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()
	select {
	case ch <- it:
	default:
		log.Printf("[msg] mailbox of %s is full; dropping a message", userID)
	}
}

// Service implements the Messaging service.
type Service struct {
	names names.Builder
	hub   *hub
	// registry lets SendLobbyMessage expand a lobby into its members.
	registry *matchmaking.Registry
	idle     time.Duration
}

// Options configures the messaging service.
type Options struct {
	Names    names.Builder
	Registry *matchmaking.Registry
	// IdleTimeout is the keep-alive interval advertised on a receive stream.
	IdleTimeout time.Duration
}

// New builds the service (both Messaging and LobbyMessaging are served by it).
func New(o Options) *Service {
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = 30 * time.Second
	}
	return &Service{names: o.Names, hub: newHub(), registry: o.Registry, idle: o.IdleTimeout}
}

// ---------------------------------------------------------------------------
// nn.npln.messaging.v1.Messaging
// ---------------------------------------------------------------------------

// RecvMessage streams messages and acks addressed to the caller.
func (s *Service) RecvMessage(req *messagingv1.RecvMessageRequest, stream messagingv1.Messaging_RecvMessageServer) error {
	caller, err := s.caller(stream.Context())
	if err != nil {
		return err
	}
	if req.GetUser() != "" {
		if userID, err := s.names.UserID(req.GetUser()); err == nil && userID != caller.UserID {
			return nplnerr.UserMismatch("cannot receive another user's messages")
		}
	}
	items, cancel := s.hub.open(caller.UserID)
	defer cancel()

	// The keep-alive tells the client the stream is healthy; without it the game
	// tears it down and stops receiving invites a minute into the session.
	if err := stream.Send(&messagingv1.RecvMessageResponse{
		Reply: &messagingv1.RecvMessageResponse_KeepAlive{
			KeepAlive: &messagingv1.KeepAlive{IdleTimeout: durationpb.New(s.idle)},
		},
	}); err != nil {
		return err
	}
	beat := time.NewTicker(s.idle)
	defer beat.Stop()
	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-beat.C:
			if err := stream.Send(&messagingv1.RecvMessageResponse{
				Reply: &messagingv1.RecvMessageResponse_KeepAlive{
					KeepAlive: &messagingv1.KeepAlive{IdleTimeout: durationpb.New(s.idle)},
				},
			}); err != nil {
				return err
			}
		case it, ok := <-items:
			if !ok {
				return nil
			}
			switch {
			case it.msg != nil:
				if err := stream.Send(&messagingv1.RecvMessageResponse{
					Reply: &messagingv1.RecvMessageResponse_Message{Message: it.msg},
				}); err != nil {
					return err
				}
			case it.ack != nil:
				if err := stream.Send(&messagingv1.RecvMessageResponse{
					Reply: &messagingv1.RecvMessageResponse_Ack{Ack: it.ack},
				}); err != nil {
					return err
				}
			}
		}
	}
}

// SendMessage delivers a message to the named users.
func (s *Service) SendMessage(ctx context.Context, req *messagingv1.SendMessageRequest) (*messagingv1.SendMessageResponse, error) {
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetUser() != "" {
		if userID, err := s.names.UserID(req.GetUser()); err == nil && userID != caller.UserID {
			return nil, nplnerr.UserMismatch("cannot send as another user")
		}
	}
	body := req.GetMessageBody()
	now := timestamppb.New(time.Now())
	delivered := 0
	for _, target := range req.GetReceiverUsers() {
		userID, err := s.names.UserID(target)
		if err != nil {
			continue
		}
		s.hub.deliver(userID, item{msg: &messagingv1.RecvMessage{
			MessageBody:        body,
			SenderUser:         s.names.User(caller.UserID),
			MessageResumeToken: s.nextToken(),
			SendTime:           now,
		}})
		delivered++
	}
	log.Printf("[msg] pid=%d sent %q to %d/%d user(s)", caller.PID, body.GetMessageType(), delivered, len(req.GetReceiverUsers()))
	return &messagingv1.SendMessageResponse{}, nil
}

// SendAck relays acknowledgements back to the original senders, so they stop
// re-sending.
func (s *Service) SendAck(ctx context.Context, req *messagingv1.SendAckRequest) (*messagingv1.SendAckResponse, error) {
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	now := timestamppb.New(time.Now())
	for _, ack := range req.GetAcks() {
		senderID, err := s.names.UserID(ack.GetSenderUser())
		if err != nil {
			continue
		}
		s.hub.deliver(senderID, item{ack: &messagingv1.RecvAck{
			Ack:            ack,
			ReceiverUser:   s.names.User(caller.UserID),
			AckResumeToken: s.nextToken(),
			AckSendTime:    now,
		}})
	}
	return &messagingv1.SendAckResponse{}, nil
}

// ---------------------------------------------------------------------------
// nn.npln.toyohr.v1.LobbyMessaging (Splatoon 3)
// ---------------------------------------------------------------------------

// RecvLobbyMessage streams lobby messages addressed to the caller.
//
// Note the shape difference from the generic service: a Splatoon 3 lobby message
// stream is scoped to ONE lobby (the game session the player is in), so a message
// for another lobby is not delivered here.
func (s *Service) RecvLobbyMessage(req *toyohrv1.RecvMessageRequest, stream toyohrv1.LobbyMessaging_RecvMessageServer) error {
	caller, err := s.caller(stream.Context())
	if err != nil {
		return err
	}
	lobby := req.GetLobby()
	items, cancel := s.hub.open(caller.UserID + "|lobby")
	defer cancel()

	send := func(it item) error {
		if it.lobby == nil {
			return nil
		}
		if lobby != "" && it.lobbyID != "" && it.lobbyID != lobby {
			return nil
		}
		return stream.Send(&toyohrv1.RecvMessageResponse{
			Payload: &toyohrv1.RecvMessageResponse_Message{Message: it.lobby},
		})
	}
	keepAlive := func() error {
		return stream.Send(&toyohrv1.RecvMessageResponse{
			Payload: &toyohrv1.RecvMessageResponse_KeepAlive{
				KeepAlive: &toyohrv1.KeepAlive{
					IdleTimeout:        durationpb.New(s.idle),
					MessageResumeToken: s.nextToken(),
				},
			},
		})
	}
	if err := keepAlive(); err != nil {
		return err
	}
	beat := time.NewTicker(s.idle)
	defer beat.Stop()
	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-beat.C:
			if err := keepAlive(); err != nil {
				return err
			}
		case it, ok := <-items:
			if !ok {
				return nil
			}
			if err := send(it); err != nil {
				return err
			}
		}
	}
}

// SendLobbyDirect delivers a Splatoon 3 message to explicit recipients.
func (s *Service) SendLobbyDirect(ctx context.Context, req *toyohrv1.SendMessageRequest) (*emptypb.Empty, error) {
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	now := timestamppb.New(time.Now())
	for _, target := range req.GetReceiverUsers() {
		userID, err := s.names.UserID(target)
		if err != nil {
			continue
		}
		s.hub.deliver(userID+"|lobby", item{lobby: &toyohrv1.ReceivedMessage{
			MessageBody:        req.GetMessageBody(),
			SenderUser:         s.names.User(caller.UserID),
			MessageResumeToken: s.nextToken(),
			SendTime:           now,
		}})
	}
	return &emptypb.Empty{}, nil
}

// SendLobbyMessage broadcasts to every player of a lobby.
//
// The "lobby" a Splatoon 3 message names is the game session the players share,
// so the recipient list comes from the matchmaking registry rather than from the
// sender: a client cannot broadcast into a lobby it is not in, and it cannot pick
// who inside the lobby hears it.
func (s *Service) SendLobbyMessage(ctx context.Context, req *toyohrv1.SendLobbyMessageRequest) (*emptypb.Empty, error) {
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	lobby := req.GetLobby()
	session, ok := s.sessionFor(lobby, caller.UserID)
	if !ok {
		return nil, nplnerr.NotFound("no such lobby, or you are not in it: " + lobby)
	}
	now := timestamppb.New(time.Now())
	sent := 0
	for _, us := range session.Users {
		if us.UserID == caller.UserID {
			continue // the sender does not need its own message back
		}
		s.hub.deliver(us.UserID+"|lobby", item{
			lobbyID: lobby,
			lobby: &toyohrv1.ReceivedMessage{
				MessageBody:        req.GetMessageBody(),
				SenderUser:         s.names.User(caller.UserID),
				MessageResumeToken: s.nextToken(),
				SendTime:           now,
			},
		})
		sent++
	}
	log.Printf("[msg] lobby %s: pid=%d broadcast %q to %d player(s)", session.ID, caller.PID, req.GetMessageBody().GetMessageType(), sent)
	return &emptypb.Empty{}, nil
}

// sessionFor resolves the lobby a message names, verifying membership. An empty
// lobby name means "the session I am in", which is how the game usually sends.
func (s *Service) sessionFor(lobby, userID string) (*matchmaking.Session, bool) {
	if lobby != "" {
		if id, err := s.names.GameSessionID(lobby); err == nil {
			if session, ok := s.registry.Get(id); ok {
				for _, us := range session.Users {
					if us.UserID == userID {
						return session, true
					}
				}
				return nil, false
			}
		}
	}
	return s.registry.FindByUser(userID)
}

// caller returns the authenticated, non-anonymous caller.
func (s *Service) caller(ctx context.Context) (*server.Caller, error) {
	c, ok := server.CallerFrom(ctx)
	if !ok {
		return nil, nplnerr.TokenInvalid("no access token")
	}
	if c.Anonymous {
		return nil, nplnerr.PermissionDenied("the anonymous user cannot exchange messages")
	}
	return c, nil
}

// nextToken mints a resume token. Messages are not replayable (they are live
// game traffic), so the token is a monotonic marker for clients that echo it.
func (s *Service) nextToken() string {
	s.hub.mu.Lock()
	s.hub.counter++
	n := s.hub.counter
	s.hub.mu.Unlock()
	return time.Now().UTC().Format("20060102T150405.000000000") + "-" + itoa(n)
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
