package matchmaking

import (
	"context"
	"errors"
	"log"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	matchmakingv1 "github.com/n-popescu/splatoon-3/gen/npln/matchmaking/v1"
	"github.com/n-popescu/splatoon-3/internal/names"
	"github.com/n-popescu/splatoon-3/internal/nplnerr"
	"github.com/n-popescu/splatoon-3/internal/server"
	"github.com/n-popescu/splatoon-3/internal/token"
)

// GameSessionService implements nn.npln.matchmaking.v1.GameSessionService.
type GameSessionService struct {
	names    names.Builder
	registry *Registry
	tokens   *token.Issuer
	ice      *IceAllocator
	// matcher is consulted by TrackGameSessionCreationTicket, which streams the
	// progress of a ticket the same way the matchmaker does.
	tickets *TicketStore
}

// GameSessionOptions configures the service.
type GameSessionOptions struct {
	Names    names.Builder
	Registry *Registry
	Tokens   *token.Issuer
	Ice      *IceAllocator
	Tickets  *TicketStore
}

// NewGameSessionService builds the service.
func NewGameSessionService(o GameSessionOptions) *GameSessionService {
	return &GameSessionService{names: o.Names, registry: o.Registry, tokens: o.Tokens, ice: o.Ice, tickets: o.Tickets}
}

// CreateGameSessionCreationTicket creates a room for the caller (the host).
//
// The request carries the room the client wants and a user definition per local
// player. We create the session immediately and answer with a SUCCEEDED ticket:
// there is nothing asynchronous about creating a room on a server that owns the
// registry, and answering PENDING would only make the client wait for a Track
// stream to tell it what we already know.
func (s *GameSessionService) CreateGameSessionCreationTicket(ctx context.Context, req *matchmakingv1.CreateGameSessionCreationTicketRequest) (*matchmakingv1.GameSessionCreationTicket, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.names.NormalizeTenant(req.GetParent()); err != nil {
		return nil, nplnerr.InvalidArgument(err.Error())
	}
	ticketIn := req.GetGameSessionCreationTicket()
	participants, err := s.participants(caller, ticketIn.GetUserDefinitions(), req.GetUserDelegationTokens())
	if err != nil {
		return nil, err
	}

	session := s.registry.Create(ticketIn.GetGameSession(), ticketIn.GetMatchmakingConfig(), participants)
	matched, err := s.matchedUserSessions(session, participants)
	if err != nil {
		return nil, err
	}
	ticket := &matchmakingv1.GameSessionCreationTicket{
		Name:                s.names.GameSessionCreationTicket(newID("gsct")),
		MatchmakingConfig:   ticketIn.GetMatchmakingConfig(),
		UserDefinitions:     ticketIn.GetUserDefinitions(),
		State:               matchmakingv1.GameSessionCreationTicket_SUCCEEDED,
		MatchedUserSessions: matched,
		GameSession:         s.registry.Proto(session, matchmakingv1.GameSessionView_FULL),
	}
	s.tickets.PutCreation(names.LastSegment(ticket.GetName()), ticket)
	log.Printf("[mm] CreateGameSessionCreationTicket pid=%d -> session=%s players=%d",
		caller.PID, session.ID, len(participants))
	return ticket, nil
}

// TrackGameSessionCreationTicket streams the state of a creation ticket.
//
// Ours are already SUCCEEDED when created, so this sends the final state and
// ends the stream — which is exactly what the client waits for. It still exists
// because the client calls it unconditionally after creating a ticket.
func (s *GameSessionService) TrackGameSessionCreationTicket(req *matchmakingv1.TrackGameSessionCreationTicketRequest, stream matchmakingv1.GameSessionService_TrackGameSessionCreationTicketServer) error {
	if _, err := requireCaller(stream.Context()); err != nil {
		return err
	}
	ticket, ok := s.tickets.GetCreation(names.LastSegment(req.GetName()))
	if !ok {
		return nplnerr.NotFound("no such game session creation ticket: " + req.GetName())
	}
	return stream.Send(ticket)
}

// CancelGameSessionCreationTicket forgets a creation ticket.
func (s *GameSessionService) CancelGameSessionCreationTicket(ctx context.Context, req *matchmakingv1.CancelGameSessionCreationTicketRequest) (*emptypb.Empty, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	s.tickets.DeleteCreation(names.LastSegment(req.GetName()))
	return &emptypb.Empty{}, nil
}

// GetGameSession returns one room.
func (s *GameSessionService) GetGameSession(ctx context.Context, req *matchmakingv1.GetGameSessionRequest) (*matchmakingv1.GameSession, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	id, err := s.names.GameSessionID(req.GetName())
	if err != nil {
		return nil, nplnerr.InvalidArgument(err.Error())
	}
	session, ok := s.registry.Get(id)
	if !ok {
		// A room that was reaped (its host vanished) gets the specific "expired"
		// code, so the client leaves the lobby instead of retrying forever.
		return nil, nplnerr.SessionExpired("this game session no longer exists")
	}
	return s.registry.Proto(session, viewOrFull(req.GetView())), nil
}

// BatchGetGameSessions returns several rooms at once.
func (s *GameSessionService) BatchGetGameSessions(ctx context.Context, req *matchmakingv1.BatchGetGameSessionsRequest) (*matchmakingv1.BatchGetGameSessionsResponse, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	out := &matchmakingv1.BatchGetGameSessionsResponse{}
	for _, name := range req.GetNames() {
		id, err := s.names.GameSessionID(name)
		if err != nil {
			continue
		}
		if session, ok := s.registry.Get(id); ok {
			out.GameSessions = append(out.GameSessions, s.registry.Proto(session, viewOrFull(req.GetView())))
		}
	}
	return out, nil
}

// QueryGameSessions is the room browser: "find me rooms like this".
func (s *GameSessionService) QueryGameSessions(ctx context.Context, req *matchmakingv1.QueryGameSessionsRequest) (*matchmakingv1.QueryGameSessionsResponse, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.names.NormalizeTenant(req.GetTenant()); err != nil {
		return nil, nplnerr.InvalidArgument(err.Error())
	}
	users := make([]string, 0, len(req.GetUsers()))
	for _, u := range req.GetUsers() {
		if id, err := s.names.UserID(u); err == nil {
			users = append(users, id)
		}
	}
	limit := int(req.GetPageSize())
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	sessions := s.registry.Query(QueryFilter{
		Config:      req.GetGameSessionSearchConfig(),
		MinVacancy:  req.GetMinVacancyCount(),
		Properties:  req.GetProperties(),
		Users:       users,
		Limit:       limit,
		RequireOpen: true,
		// A query for specific users is "where are my friends playing" and must
		// find their private room too; a plain browse only returns public rooms.
		RequirePublic: len(users) == 0,
	})
	resp := &matchmakingv1.QueryGameSessionsResponse{}
	for _, session := range sessions {
		resp.GameSessions = append(resp.GameSessions, s.registry.Proto(session, viewOrBasic(req.GetView())))
	}
	log.Printf("[mm] QueryGameSessions pid=%d users=%d -> %d room(s)", caller.PID, len(users), len(resp.GameSessions))
	return resp, nil
}

// JoinGameSession puts the caller (and any local co-players) into a room.
func (s *GameSessionService) JoinGameSession(ctx context.Context, req *matchmakingv1.JoinGameSessionRequest) (*matchmakingv1.JoinGameSessionResponse, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	id, err := s.names.GameSessionID(req.GetName())
	if err != nil {
		return nil, nplnerr.InvalidArgument(err.Error())
	}
	participants, err := s.participants(caller, req.GetUserDefinitions(), req.GetUserDelegationTokens())
	if err != nil {
		return nil, err
	}
	session, joined, err := s.registry.Join(id, req.GetPassword(), participants)
	switch {
	case errors.Is(err, ErrNoSuchSession):
		return nil, nplnerr.SessionExpired("this game session no longer exists")
	case errors.Is(err, ErrSessionFull):
		return nil, nplnerr.SessionFull("this game session is full")
	case errors.Is(err, ErrWrongPassword):
		return nil, nplnerr.WrongPassword("wrong game session password")
	case errors.Is(err, ErrClosed):
		return nil, nplnerr.FailedPrecondition("this game session is not accepting participants")
	case err != nil:
		return nil, nplnerr.Internal(err.Error())
	}
	matched, err := s.matchedFromUserSessions(session, joined)
	if err != nil {
		return nil, err
	}
	log.Printf("[mm] JoinGameSession pid=%d session=%s -> %d/%d players",
		caller.PID, session.ID, len(session.Users), session.MaxParticipants)
	return &matchmakingv1.JoinGameSessionResponse{
		MatchedUserSessions: matched,
		GameSession:         s.registry.Proto(session, matchmakingv1.GameSessionView_FULL),
	}, nil
}

// SyncGameSession is the room heartbeat.
//
// The host publishes its peer address and the room's current shape; everybody
// gets back the current list of user sessions, which is how a host learns that a
// player joined or left. It is also what keeps the room alive: a room nobody
// syncs is reaped.
func (s *GameSessionService) SyncGameSession(ctx context.Context, req *matchmakingv1.SyncGameSessionRequest) (*matchmakingv1.SyncGameSessionResponse, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	in := req.GetGameSession()
	if in.GetName() == "" {
		return nil, nplnerr.InvalidArgument("SyncGameSession without a game session name")
	}
	id, err := s.names.GameSessionID(in.GetName())
	if err != nil {
		return nil, nplnerr.InvalidArgument(err.Error())
	}
	session, err := s.registry.Sync(id, caller.UserID, in, req.GetKeepAliveOnly())
	if errors.Is(err, ErrNoSuchSession) {
		return nil, nplnerr.SessionExpired("this game session no longer exists")
	}
	if err != nil {
		return nil, nplnerr.Internal(err.Error())
	}
	resp := &matchmakingv1.SyncGameSessionResponse{}
	for _, us := range session.Users {
		resp.UserSessions = append(resp.UserSessions, s.registry.UserProto(session, us))
	}
	return resp, nil
}

// ListUserSessions lists the players of a room.
func (s *GameSessionService) ListUserSessions(ctx context.Context, req *matchmakingv1.ListUserSessionsRequest) (*matchmakingv1.ListUserSessionsResponse, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	id, err := s.names.GameSessionID(req.GetParent())
	if err != nil {
		return nil, nplnerr.InvalidArgument(err.Error())
	}
	session, ok := s.registry.Get(id)
	if !ok {
		return nil, nplnerr.SessionExpired("this game session no longer exists")
	}
	resp := &matchmakingv1.ListUserSessionsResponse{}
	for _, us := range session.Users {
		resp.UserSessions = append(resp.UserSessions, s.registry.UserProto(session, us))
	}
	return resp, nil
}

// GetUserSession returns one player of a room.
func (s *GameSessionService) GetUserSession(ctx context.Context, req *matchmakingv1.GetUserSessionRequest) (*matchmakingv1.UserSession, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	sessionID, userSessionID, err := s.names.UserSessionID(req.GetName())
	if err != nil {
		return nil, nplnerr.InvalidArgument(err.Error())
	}
	session, ok := s.registry.Get(sessionID)
	if !ok {
		return nil, nplnerr.SessionExpired("this game session no longer exists")
	}
	for _, us := range session.Users {
		if us.ID == userSessionID {
			return s.registry.UserProto(session, us), nil
		}
	}
	return nil, nplnerr.NotFound("no such user session")
}

// IssueMatchmakingIdToken re-issues the per-player tokens of a room, which peers
// exchange to prove who they are to each other.
func (s *GameSessionService) IssueMatchmakingIdToken(ctx context.Context, req *matchmakingv1.IssueMatchmakingIdTokenRequest) (*matchmakingv1.IssueMatchmakingIdTokenResponse, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	id, err := s.names.GameSessionID(req.GetGameSession())
	if err != nil {
		return nil, nplnerr.InvalidArgument(err.Error())
	}
	session, ok := s.registry.Get(id)
	if !ok {
		return nil, nplnerr.SessionExpired("this game session no longer exists")
	}
	// Only a member of the room may mint its tokens.
	if !s.memberOf(session, caller.UserID) {
		return nil, nplnerr.PermissionDenied("you are not in this game session")
	}
	want := map[string]bool{}
	for _, u := range req.GetUsers() {
		if uid, err := s.names.UserID(u); err == nil {
			want[uid] = true
		}
	}
	var matched []*matchmakingv1.MatchedUserSession
	for _, us := range session.Users {
		if len(want) > 0 && !want[us.UserID] {
			continue
		}
		m, err := s.matchedFor(session, us)
		if err != nil {
			return nil, err
		}
		matched = append(matched, m)
	}
	return &matchmakingv1.IssueMatchmakingIdTokenResponse{
		MatchedUserSessions: matched,
		GameSession:         s.registry.Proto(session, matchmakingv1.GameSessionView_FULL),
	}, nil
}

// IssueUserDelegationToken lets one console act for a second local player.
//
// Splatoon 3 supports two players on one console in some modes; the console
// holds one access token but must place two users. The delegation token is the
// delegator's consent, and every request that adds a user other than the token
// owner must carry one — see participants below.
func (s *GameSessionService) IssueUserDelegationToken(ctx context.Context, req *matchmakingv1.IssueUserDelegationTokenRequest) (*matchmakingv1.IssueUserDelegationTokenResponse, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.names.NormalizeTenant(req.GetParent()); err != nil {
		return nil, nplnerr.InvalidArgument(err.Error())
	}
	delegator, err := s.names.UserID(req.GetDelegatorUser())
	if err != nil {
		return nil, nplnerr.InvalidArgument("delegator_user: " + err.Error())
	}
	mandatary, err := s.names.UserID(req.GetMandataryUser())
	if err != nil {
		return nil, nplnerr.InvalidArgument("mandatary_user: " + err.Error())
	}
	// The caller must be one of the two parties: you can consent for yourself,
	// or ask for a token naming you as the mandatary of a local player whose
	// user you also hold (both are the same console).
	if caller.UserID != delegator && caller.UserID != mandatary {
		return nil, nplnerr.PermissionDenied("a delegation token must involve the calling user")
	}
	actions := make([]int32, 0, len(req.GetDelegationActions()))
	for _, a := range req.GetDelegationActions() {
		actions = append(actions, int32(a))
	}
	tok, ttl, err := s.tokens.IssueDelegation(delegator, mandatary, actions, caller.PID)
	if err != nil {
		return nil, nplnerr.Internal("cannot sign delegation token: " + err.Error())
	}
	return &matchmakingv1.IssueUserDelegationTokenResponse{
		UserDelegationDetail: &matchmakingv1.UserDelegationDetail{
			DelegatorUser:       s.names.User(delegator),
			MandataryUser:       s.names.User(mandatary),
			Attributes:          req.GetAttributes(),
			DelegationActions:   req.GetDelegationActions(),
			UserDelegationToken: tok,
			Ttl:                 durationOf(ttl),
		},
	}, nil
}

// IssuePublicKey publishes the key that verifies our matchmaking id tokens, so
// peers can check each other's tokens without asking the server.
func (s *GameSessionService) IssuePublicKey(ctx context.Context, req *matchmakingv1.IssuePublicKeyRequest) (*matchmakingv1.IssuePublicKeyResponse, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	pem, err := s.tokens.PublicKeyPEM()
	if err != nil {
		return nil, nplnerr.Internal("cannot export the public key: " + err.Error())
	}
	return &matchmakingv1.IssuePublicKeyResponse{Key: pem}, nil
}

// CreateGameSessionShortAlias gives a room a short code players can type.
func (s *GameSessionService) CreateGameSessionShortAlias(ctx context.Context, req *matchmakingv1.CreateGameSessionShortAliasRequest) (*matchmakingv1.GameSessionShortAlias, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	alias := req.GetGameSessionShortAlias()
	sessionID, err := s.names.GameSessionID(alias.GetGameSession())
	if err != nil {
		return nil, nplnerr.InvalidArgument(err.Error())
	}
	session, ok := s.registry.Get(sessionID)
	if !ok {
		return nil, nplnerr.SessionExpired("this game session no longer exists")
	}
	if host := session.HostUser(); host == nil || host.UserID != caller.UserID {
		return nil, nplnerr.PermissionDenied("only the host may create a room code")
	}
	code, err := s.registry.SetAlias(sessionID, names.LastSegment(alias.GetName()))
	if err != nil {
		return nil, nplnerr.FailedPrecondition(err.Error())
	}
	log.Printf("[mm] room code %s -> session %s", code, sessionID)
	return &matchmakingv1.GameSessionShortAlias{
		Name:        s.names.GameSessionShortAlias(code),
		GameSession: s.names.GameSession(sessionID),
		// The code lives exactly as long as the room does; the reaper is what
		// actually frees it.
		ExpireTime: timestamppb.New(time.Now().Add(s.registry.ttl)),
	}, nil
}

// GetGameSessionShortAlias resolves a room code.
func (s *GameSessionService) GetGameSessionShortAlias(ctx context.Context, req *matchmakingv1.GetGameSessionShortAliasRequest) (*matchmakingv1.GameSessionShortAlias, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	code := names.LastSegment(req.GetName())
	session, ok := s.registry.ResolveAlias(code)
	if !ok {
		return nil, nplnerr.NotFound("no room has the code " + code)
	}
	return &matchmakingv1.GameSessionShortAlias{
		Name:        s.names.GameSessionShortAlias(code),
		GameSession: s.names.GameSession(session.ID),
		ExpireTime:  timestamppb.New(session.UpdatedAt.Add(s.registry.ttl)),
	}, nil
}

// AllocateIceServerSet hands the client the STUN/TURN servers to hole-punch with.
func (s *GameSessionService) AllocateIceServerSet(ctx context.Context, req *matchmakingv1.AllocateIceServerSetRequest) (*matchmakingv1.IceServerSet, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.names.NormalizeTenant(req.GetTenant()); err != nil {
		return nil, nplnerr.InvalidArgument(err.Error())
	}
	set, err := s.ice.Allocate(caller.UserID)
	if err != nil {
		return nil, nplnerr.FailedPrecondition(err.Error())
	}
	return set, nil
}

// ListLatencyMeasurementServers returns the endpoints the client pings to build
// the latency data it sends with a matchmaking ticket.
func (s *GameSessionService) ListLatencyMeasurementServers(ctx context.Context, req *matchmakingv1.ListLatencyMeasurementServersRequest) (*matchmakingv1.ListLatencyMeasurementServersResponse, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	return &matchmakingv1.ListLatencyMeasurementServersResponse{
		LatencyMeasurementServers: s.ice.LatencyServers(),
	}, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// participants turns the user definitions of a request into participants,
// enforcing that the caller may act for each of them.
//
// This is the check that keeps one console from stuffing other players' users
// into a room: the token's own user is always allowed, any OTHER user needs a
// delegation token signed by us that names it as the delegator and the caller as
// the mandatary.
func (s *GameSessionService) participants(caller *server.Caller, defs []*matchmakingv1.UserDefinition, delegations []string) ([]Participant, error) {
	allowed := map[string]bool{caller.UserID: true}
	for _, raw := range delegations {
		claims, err := s.tokens.VerifyDelegation(raw)
		if err != nil {
			log.Printf("[mm] pid=%d sent an unusable delegation token: %v", caller.PID, err)
			continue
		}
		if claims.Mandatary == caller.UserID || claims.Mandatary == "" {
			allowed[claims.Delegator] = true
		}
	}

	if len(defs) == 0 {
		// The common case: one player, implicit. The client does not have to
		// spell out a user definition for itself.
		return []Participant{{UserID: caller.UserID, PID: caller.PID, NsaID: caller.NsaID}}, nil
	}
	out := make([]Participant, 0, len(defs))
	for _, d := range defs {
		userID, err := s.names.UserID(d.GetUser())
		if err != nil {
			// An empty user means "me", which some requests use.
			if d.GetUser() == "" {
				userID = caller.UserID
			} else {
				return nil, nplnerr.InvalidArgument("user_definition.user: " + err.Error())
			}
		}
		if !allowed[userID] {
			return nil, nplnerr.PermissionDenied("no delegation token for user " + userID)
		}
		p := Participant{
			UserID:     userID,
			PID:        caller.PID,
			NsaID:      caller.NsaID,
			Team:       d.GetTeam(),
			Attributes: d.GetAttributes(),
			Latency:    d.GetLatencyData(),
		}
		if userID != caller.UserID {
			// A delegated local player shares the console's account, so the PID
			// is the same; only the NPLN user differs.
			p.NsaID = caller.NsaID
		}
		out = append(out, p)
	}
	return out, nil
}

// matchedUserSessions builds the MatchedUserSession list for freshly placed
// participants.
func (s *GameSessionService) matchedUserSessions(session *Session, participants []Participant) ([]*matchmakingv1.MatchedUserSession, error) {
	var out []*matchmakingv1.MatchedUserSession
	for _, p := range participants {
		var us *UserSession
		for _, candidate := range session.Users {
			if candidate.UserID == p.UserID {
				us = candidate
				break
			}
		}
		if us == nil {
			continue
		}
		m, err := s.matchedFor(session, us)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// matchedFromUserSessions is matchedUserSessions for already-created sessions.
func (s *GameSessionService) matchedFromUserSessions(session *Session, users []*UserSession) ([]*matchmakingv1.MatchedUserSession, error) {
	var out []*matchmakingv1.MatchedUserSession
	for _, us := range users {
		m, err := s.matchedFor(session, us)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// matchedFor mints the token for one placed player.
func (s *GameSessionService) matchedFor(session *Session, us *UserSession) (*matchmakingv1.MatchedUserSession, error) {
	tok, err := s.tokens.IssueMatchmaking(us.UserID, s.names.GameSession(session.ID), s.names.UserSession(session.ID, us.ID), us.NsaID, us.PID)
	if err != nil {
		return nil, nplnerr.Internal("cannot sign matchmaking id token: " + err.Error())
	}
	return &matchmakingv1.MatchedUserSession{
		UserDefinition: &matchmakingv1.UserDefinition{
			User:        s.names.User(us.UserID),
			Attributes:  us.Attributes,
			LatencyData: us.Latency,
			Team:        us.Team,
		},
		UserSession:         s.names.UserSession(session.ID, us.ID),
		MatchmakingIdToken:  tok,
	}, nil
}

// memberOf reports whether a user is in a session.
func (s *GameSessionService) memberOf(session *Session, userID string) bool {
	for _, us := range session.Users {
		if us.UserID == userID {
			return true
		}
	}
	return false
}

// requireCaller returns the authenticated, non-anonymous caller.
func requireCaller(ctx context.Context) (*server.Caller, error) {
	c, ok := server.CallerFrom(ctx)
	if !ok {
		return nil, nplnerr.TokenInvalid("no access token")
	}
	if c.Anonymous {
		return nil, nplnerr.PermissionDenied("the anonymous user cannot take part in matchmaking")
	}
	if c.PID == 0 {
		return nil, nplnerr.InvalidAccount("this token carries no Nextendo account")
	}
	return c, nil
}

// viewOrFull defaults an unspecified view to FULL (what a member of the room
// needs), viewOrBasic defaults it to BASIC (what a browser needs).
func viewOrFull(v matchmakingv1.GameSessionView) matchmakingv1.GameSessionView {
	if v == matchmakingv1.GameSessionView_GAME_SESSION_VIEW_UNSPECIFIED {
		return matchmakingv1.GameSessionView_FULL
	}
	return v
}

func viewOrBasic(v matchmakingv1.GameSessionView) matchmakingv1.GameSessionView {
	if v == matchmakingv1.GameSessionView_GAME_SESSION_VIEW_UNSPECIFIED {
		return matchmakingv1.GameSessionView_BASIC
	}
	return v
}
