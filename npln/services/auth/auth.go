// Package auth implements nn.npln.auth.v1.Auth and nn.npln.auth.v1.UserService.
//
// This is the front door: every other service refuses a request that does not
// carry an access token minted here.
//
// # The flow a console goes through
//
//  1. The console authenticates against the account layer (dauth → BAAS) and
//     ends up holding a BAAS `id_token` for the user playing Splatoon 3.
//  2. It calls Auth.IssuePrearrangedUserToken with that id_token. NPLN gives
//     every account 16 "prearranged" users per tenant; slot 0 is the one a
//     single-player console uses, slots 1..15 exist for extra local players.
//  3. We resolve the id_token to a Nextendo account (see internal/identity),
//     apply the same online gates as the NEX game servers, and mint an access
//     token + a refresh token.
//  4. Every later RPC carries `authorization: bearer <access token>`.
//
// # Identity rules
//
// The user id we return for slot 0 is byte-identical to the id
// nextendo-account publishes for that account (nplnUserID). That is not a
// detail: the account server is what tells this service who a player's friends
// are, in NPLN ids. If the two derivations disagreed, every friend list would
// be empty while looking perfectly healthy in the logs.
//
// A token that cannot be tied to an account is REFUSED. It is never mapped to a
// default or "most recent" account — that is precisely the failure mode that
// made every Switch add friends as one particular person (docs/FRIENDS.md).
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	authv1 "github.com/NextendoNetwork/splatoon-3/gen/npln/auth/v1"
	"github.com/NextendoNetwork/splatoon-3/npln/account"
	"github.com/NextendoNetwork/splatoon-3/npln/identity"
	"github.com/NextendoNetwork/splatoon-3/npln/names"
	"github.com/NextendoNetwork/splatoon-3/npln/nplnerr"
	"github.com/NextendoNetwork/splatoon-3/npln/server"
	"github.com/NextendoNetwork/splatoon-3/npln/store"
	"github.com/NextendoNetwork/splatoon-3/npln/token"
)

// prearrangedUserCount is how many prearranged users a tenant offers per
// account, per the protocol documentation.
const prearrangedUserCount = 16

// anonymousUserID is the id retail NPLN uses for the anonymous user.
const anonymousUserID = "u-anonymous"

// UserRecord is what we persist about a user of this tenant.
type UserRecord struct {
	UserID    string    `json:"user_id"`
	AccountID string    `json:"account_id"`
	PID       uint64    `json:"pid"`
	NsaID     string    `json:"nsa_id"`
	Index     int32     `json:"index"` // prearranged slot, -1 for CreateUser users
	CreatedAt time.Time `json:"created_at"`
	LastLogin time.Time `json:"last_login"`
	ShortID   int64     `json:"short_id"`
}

// Service implements the Auth and UserService gRPC services.
type Service struct {
	names    names.Builder
	tokens   *token.Issuer
	resolver *identity.Resolver
	accounts *account.Client
	users    *store.JSONMap[UserRecord]

	// requireAccount refuses any login that cannot be tied to a Nextendo
	// account. Off only for local testing.
	requireAccount bool

	// refresh holds the live refresh tokens. Single-use, in memory: a refresh
	// token that survived a restart would be a credential nobody can revoke,
	// and losing them only costs the console one extra id_token round-trip.
	refreshMu sync.Mutex
	refresh   map[string]refreshEntry
}

type refreshEntry struct {
	userID  string
	pid     uint64
	nsaID   string
	index   int32
	anon    bool
	expires time.Time
}

// refreshTTL is how long a refresh token stays usable. Generous, because the
// console may sit in a menu for hours before refreshing.
const refreshTTL = 30 * 24 * time.Hour

// Options configures the service.
type Options struct {
	Names          names.Builder
	Tokens         *token.Issuer
	Resolver       *identity.Resolver
	Accounts       *account.Client
	Users          *store.JSONMap[UserRecord]
	RequireAccount bool
}

// New builds the Auth service.
func New(o Options) *Service {
	return &Service{
		names:          o.Names,
		tokens:         o.Tokens,
		resolver:       o.Resolver,
		accounts:       o.Accounts,
		users:          o.Users,
		requireAccount: o.RequireAccount,
		refresh:        map[string]refreshEntry{},
	}
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

// CreateUser registers a user on this tenant for the account behind the id
// token. NPLN allows an arbitrary number of them; Splatoon 3 uses the
// prearranged slots instead, so this exists for completeness and for tools.
func (s *Service) CreateUser(ctx context.Context, req *authv1.CreateUserRequest) (*authv1.User, error) {
	if _, err := s.names.NormalizeTenant(req.GetParent()); err != nil {
		return nil, nplnerr.InvalidArgument(err.Error())
	}
	id, err := s.authenticate(ctx, req.GetExternalIdToken(), 0)
	if err != nil {
		return nil, err
	}
	rec := s.upsertUser(id, -1)
	log.Printf("[auth] CreateUser pid=%d user=%s", id.PID, rec.UserID)
	return s.userProto(rec), nil
}

// IssueToken issues an access token for a user created with CreateUser.
func (s *Service) IssueToken(ctx context.Context, req *authv1.IssueTokenRequest) (*authv1.IssueTokenResponse, error) {
	userID, err := s.names.UserID(req.GetUser())
	if err != nil {
		return nil, nplnerr.InvalidArgument(err.Error())
	}
	id, err := s.authenticate(ctx, req.GetExternalIdToken(), 0)
	if err != nil {
		return nil, err
	}
	rec, ok := s.users.Get(userID)
	if !ok {
		return nil, nplnerr.UserNotFound("no such user on this tenant: " + userID)
	}
	// The token proves an account; the request names a user. The two must agree,
	// or anybody with a valid id token could take over another player's user.
	if rec.PID != id.PID {
		return nil, nplnerr.UserMismatch("this user belongs to another account")
	}
	id.UserID = rec.UserID
	id.UserIndex = rec.Index
	return s.issue(id, false)
}

// IssuePrearrangedUserToken issues an access token for one of the 16 prearranged
// users of the account. This is the path Splatoon 3 uses.
func (s *Service) IssuePrearrangedUserToken(ctx context.Context, req *authv1.IssuePrearrangedUserTokenRequest) (*authv1.IssuePrearrangedUserTokenResponse, error) {
	if _, err := s.names.NormalizeTenant(req.GetTenant()); err != nil {
		return nil, nplnerr.InvalidArgument(err.Error())
	}
	idx := req.GetUserIndex()
	if idx < 0 || idx >= prearrangedUserCount {
		return nil, nplnerr.InvalidArgument(fmt.Sprintf("user_index %d is outside 0..%d", idx, prearrangedUserCount-1))
	}
	id, err := s.authenticate(ctx, req.GetExternalIdToken(), idx)
	if err != nil {
		return nil, err
	}
	rec := s.upsertUser(id, idx)
	resp, err := s.issue(id, false)
	if err != nil {
		return nil, err
	}
	log.Printf("[auth] IssuePrearrangedUserToken pid=%d slot=%d user=%s nsa=%s proven=%v",
		id.PID, idx, id.UserID, id.NsaID, id.Proven)
	return &authv1.IssuePrearrangedUserTokenResponse{User: s.userProto(rec), Token: resp.GetToken()}, nil
}

// IssueAnonymousUserToken issues the tenant's anonymous token. Most services
// refuse it, exactly like retail NPLN — it exists so a client can reach the
// maintenance/appconfig services before it has an identity.
func (s *Service) IssueAnonymousUserToken(ctx context.Context, req *authv1.IssueAnonymousUserTokenRequest) (*authv1.IssueAnonymousUserTokenResponse, error) {
	if _, err := s.names.NormalizeTenant(req.GetTenant()); err != nil {
		return nil, nplnerr.InvalidArgument(err.Error())
	}
	// Anonymous still needs a syntactically valid id token: it proves the caller
	// owns the title. It does NOT need to resolve to an account.
	tok := externalToken(req.GetExternalIdToken())
	if tok == "" {
		return nil, nplnerr.TokenInvalid("anonymous token request without an external id token")
	}
	access, err := s.tokens.IssueAccess(anonymousUserID, "", "", 0, 0, true)
	if err != nil {
		return nil, nplnerr.Internal("cannot sign anonymous token: " + err.Error())
	}
	refresh := s.newRefresh(refreshEntry{userID: anonymousUserID, anon: true, expires: time.Now().Add(refreshTTL)})
	log.Printf("[auth] IssueAnonymousUserToken")
	return &authv1.IssueAnonymousUserTokenResponse{Token: &authv1.Token{
		User:         s.names.User(anonymousUserID),
		AccessToken:  access,
		RefreshToken: refresh,
		Ttl:          durationpb.New(s.tokens.AccessTTL()),
	}}, nil
}

// RefreshToken exchanges a refresh token for a fresh access token. Each refresh
// token is single-use, as documented; a replay gets Unauthenticated.
func (s *Service) RefreshToken(ctx context.Context, req *authv1.RefreshTokenRequest) (*authv1.RefreshTokenResponse, error) {
	entry, ok := s.consumeRefresh(req.GetRefreshToken())
	if !ok {
		return nil, nplnerr.TokenInvalid("unknown or already-used refresh token")
	}
	if req.GetUser() != "" {
		if userID, err := s.names.UserID(req.GetUser()); err == nil && userID != entry.userID {
			return nil, nplnerr.UserMismatch("refresh token does not belong to that user")
		}
	}
	tok, err := s.reissue(entry)
	if err != nil {
		return nil, err
	}
	return &authv1.RefreshTokenResponse{Token: tok}, nil
}

// RefreshAnonymousUserToken is RefreshToken for the anonymous user.
func (s *Service) RefreshAnonymousUserToken(ctx context.Context, req *authv1.RefreshAnonymousTokenRequest) (*authv1.RefreshAnonymousTokenResponse, error) {
	if _, err := s.names.NormalizeTenant(req.GetTenant()); err != nil {
		return nil, nplnerr.InvalidArgument(err.Error())
	}
	entry, ok := s.consumeRefresh(req.GetRefreshToken())
	if !ok || !entry.anon {
		return nil, nplnerr.TokenInvalid("unknown or already-used anonymous refresh token")
	}
	tok, err := s.reissue(entry)
	if err != nil {
		return nil, err
	}
	return &authv1.RefreshAnonymousTokenResponse{Token: tok}, nil
}

// ValidateToken succeeds when the request carried a valid access token. The
// interceptor has already verified it, so reaching here with a caller in the
// context IS the answer.
func (s *Service) ValidateToken(ctx context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	if _, ok := server.CallerFrom(ctx); !ok {
		return nil, nplnerr.TokenInvalid("no valid access token on this request")
	}
	return &emptypb.Empty{}, nil
}

// ListUsers lists the users of the calling account that were created with
// CreateUser (prearranged users are excluded, per the documentation).
func (s *Service) ListUsers(ctx context.Context, req *authv1.ListUsersRequest) (*authv1.ListUsersResponse, error) {
	caller, ok := server.CallerFrom(ctx)
	if !ok || caller.Anonymous {
		return nil, nplnerr.TokenInvalid("ListUsers requires a non-anonymous access token")
	}
	if _, err := s.names.NormalizeTenant(req.GetParent()); err != nil {
		return nil, nplnerr.InvalidArgument(err.Error())
	}
	var out []*authv1.User
	s.users.Range(func(_ string, rec UserRecord) bool {
		if rec.PID == caller.PID && rec.Index < 0 {
			out = append(out, s.userProto(rec))
		}
		return true
	})
	return &authv1.ListUsersResponse{Users: out}, nil
}

// DeleteUser deletes a user created with CreateUser. Prearranged users cannot be
// deleted (they are implicit), which the documentation also states.
func (s *Service) DeleteUser(ctx context.Context, req *authv1.DeleteUserRequest) (*emptypb.Empty, error) {
	caller, ok := server.CallerFrom(ctx)
	if !ok || caller.Anonymous {
		return nil, nplnerr.TokenInvalid("DeleteUser requires a non-anonymous access token")
	}
	userID, err := s.names.UserID(req.GetName())
	if err != nil {
		return nil, nplnerr.InvalidArgument(err.Error())
	}
	rec, found := s.users.Get(userID)
	if !found {
		return nil, nplnerr.UserNotFound("no such user: " + userID)
	}
	if rec.PID != caller.PID {
		return nil, nplnerr.PermissionDenied("that user belongs to another account")
	}
	if rec.Index >= 0 {
		return nil, nplnerr.FailedPrecondition("prearranged users cannot be deleted")
	}
	s.users.Delete(userID)
	log.Printf("[auth] DeleteUser pid=%d user=%s", caller.PID, userID)
	return &emptypb.Empty{}, nil
}

// ---------------------------------------------------------------------------
// UserService
// ---------------------------------------------------------------------------

// GetUser returns a user of this tenant.
func (s *Service) GetUser(ctx context.Context, req *authv1.GetUserRequest) (*authv1.User, error) {
	userID, err := s.names.UserID(req.GetName())
	if err != nil {
		return nil, nplnerr.InvalidArgument(err.Error())
	}
	rec, ok := s.users.Get(userID)
	if !ok {
		return nil, nplnerr.UserNotFound("no such user: " + userID)
	}
	return s.userProto(rec), nil
}

// QueryUserExternalIds maps users to their external (NSA) ids and back. The
// game uses it to turn the NSA ids it knows from the Switch friend list into
// NPLN users, and vice versa.
func (s *Service) QueryUserExternalIds(ctx context.Context, req *authv1.QueryUserExternalIdsRequest) (*authv1.QueryUserExternalIdsResponse, error) {
	if _, err := s.names.NormalizeTenant(req.GetTenant()); err != nil {
		return nil, nplnerr.InvalidArgument(err.Error())
	}
	var out []*authv1.UserExternalId

	switch cond := req.GetCondition().(type) {
	case *authv1.QueryUserExternalIdsRequest_ExternalIdCondition_:
		// Given users, return their external ids.
		for _, userName := range cond.ExternalIdCondition.GetUsers() {
			userID, err := s.names.UserID(userName)
			if err != nil {
				continue
			}
			if rec, ok := s.users.Get(userID); ok {
				out = append(out, s.externalIDProto(rec))
			}
		}
	case *authv1.QueryUserExternalIdsRequest_UserCondition_:
		// Given external ids, return the matching users. An NSA id we have
		// never seen is simply absent from the answer (not an error): the
		// console asks about every friend, including those who never played
		// Splatoon 3.
		want := map[string]bool{}
		for _, ext := range cond.UserCondition.GetExternalIds() {
			if ext.GetId() != "" {
				want[strings.ToLower(ext.GetId())] = true
			}
		}
		s.users.Range(func(_ string, rec UserRecord) bool {
			if want[strings.ToLower(rec.NsaID)] {
				out = append(out, s.externalIDProto(rec))
			}
			return true
		})
	default:
		return nil, nplnerr.InvalidArgument("QueryUserExternalIds needs a user_condition or an external_id_condition")
	}
	return &authv1.QueryUserExternalIdsResponse{UserExternalIds: out}, nil
}

// QueryUserShortIds maps user ids to short ids and back.
func (s *Service) QueryUserShortIds(ctx context.Context, req *authv1.QueryUserShortIdsRequest) (*authv1.QueryUserShortIdsResponse, error) {
	if _, err := s.names.NormalizeTenant(req.GetTenant()); err != nil {
		return nil, nplnerr.InvalidArgument(err.Error())
	}
	var out []*authv1.QueryUserShortIdsResponse_UserShortId
	switch cond := req.GetCondition().(type) {
	case *authv1.QueryUserShortIdsRequest_UserIds_:
		for _, name := range cond.UserIds.GetIds() {
			userID, err := s.names.UserID(name)
			if err != nil {
				userID = names.LastSegment(name)
			}
			if rec, ok := s.users.Get(userID); ok {
				out = append(out, &authv1.QueryUserShortIdsResponse_UserShortId{UserId: s.names.User(rec.UserID), ShortId: rec.ShortID})
			}
		}
	case *authv1.QueryUserShortIdsRequest_ShortIds_:
		want := map[int64]bool{}
		for _, id := range cond.ShortIds.GetIds() {
			want[id] = true
		}
		s.users.Range(func(_ string, rec UserRecord) bool {
			if want[rec.ShortID] {
				out = append(out, &authv1.QueryUserShortIdsResponse_UserShortId{UserId: s.names.User(rec.UserID), ShortId: rec.ShortID})
			}
			return true
		})
	default:
		return nil, nplnerr.InvalidArgument("QueryUserShortIds needs user_ids or short_ids")
	}
	return &authv1.QueryUserShortIdsResponse{UserShortIds: out}, nil
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

// authenticate resolves an external id token to a Nextendo identity and applies
// the online gates. Every refusal is explicit; there is no default identity.
func (s *Service) authenticate(ctx context.Context, ext *authv1.ExternalIdToken, userIndex int32) (*identity.Identity, error) {
	raw := externalToken(ext)
	if raw == "" {
		return nil, nplnerr.TokenInvalid("no external id token in the request")
	}
	id, err := s.resolver.Resolve(raw, userIndex)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrExpiredToken):
			return nil, nplnerr.TokenExpired("the console's id token has expired")
		case errors.Is(err, identity.ErrUnknownAccount):
			// FAIL-CLOSED. This is the message an operator wants to see: the
			// console is fine, it simply is not linked to a Nextendo account.
			log.Printf("[auth] REFUSED: %v", err)
			return nil, nplnerr.InvalidAccount("this console is not linked to a Nextendo account")
		case errors.Is(err, identity.ErrUnproven):
			log.Printf("[auth] REFUSED: %v", err)
			return nil, nplnerr.InvalidAccount("this client did not prove which Nextendo account it is using")
		default:
			log.Printf("[auth] REFUSED: %v", err)
			return nil, nplnerr.TokenInvalid("id token rejected: " + err.Error())
		}
	}

	// Online gates, owned by nextendo-account and shared with every game:
	// verified e-mail, not disabled/banned, not already playing elsewhere.
	if allow, reason := s.accounts.OnlineCheck(ctx, id.PID, "switch"); !allow {
		log.Printf("[auth] pid=%d REFUSED by the online gate (%s)", id.PID, reason)
		return nil, nplnerr.InvalidAccount("this account may not go online right now: " + reason)
	}
	return id, nil
}

// issue mints the token pair for a resolved identity.
func (s *Service) issue(id *identity.Identity, anon bool) (*authv1.IssueTokenResponse, error) {
	access, err := s.tokens.IssueAccess(id.UserID, id.AccountID, id.NsaID, id.PID, id.UserIndex, anon)
	if err != nil {
		return nil, nplnerr.Internal("cannot sign access token: " + err.Error())
	}
	refresh := s.newRefresh(refreshEntry{
		userID:  id.UserID,
		pid:     id.PID,
		nsaID:   id.NsaID,
		index:   id.UserIndex,
		anon:    anon,
		expires: time.Now().Add(refreshTTL),
	})
	return &authv1.IssueTokenResponse{Token: &authv1.Token{
		User:         s.names.User(id.UserID),
		AccessToken:  access,
		RefreshToken: refresh,
		Ttl:          durationpb.New(s.tokens.AccessTTL()),
	}}, nil
}

// reissue mints a new token pair from a consumed refresh entry.
func (s *Service) reissue(entry refreshEntry) (*authv1.Token, error) {
	accountID := ""
	if !entry.anon {
		accountID = s.resolver.AccountID(entry.pid)
	}
	access, err := s.tokens.IssueAccess(entry.userID, accountID, entry.nsaID, entry.pid, entry.index, entry.anon)
	if err != nil {
		return nil, nplnerr.Internal("cannot sign access token: " + err.Error())
	}
	next := s.newRefresh(refreshEntry{
		userID:  entry.userID,
		pid:     entry.pid,
		nsaID:   entry.nsaID,
		index:   entry.index,
		anon:    entry.anon,
		expires: time.Now().Add(refreshTTL),
	})
	return &authv1.Token{
		User:         s.names.User(entry.userID),
		AccessToken:  access,
		RefreshToken: next,
		Ttl:          durationpb.New(s.tokens.AccessTTL()),
	}, nil
}

// newRefresh stores and returns a fresh refresh token (a UUID-shaped random
// string, like the retail one).
func (s *Service) newRefresh(entry refreshEntry) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	h := hex.EncodeToString(b[:])
	tok := fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
	s.refreshMu.Lock()
	// Opportunistic sweep: refresh tokens are single-use, so the only entries
	// that linger are those from consoles that never came back.
	now := time.Now()
	for k, v := range s.refresh {
		if now.After(v.expires) {
			delete(s.refresh, k)
		}
	}
	s.refresh[tok] = entry
	s.refreshMu.Unlock()
	return tok
}

// consumeRefresh looks a refresh token up and removes it (single use).
func (s *Service) consumeRefresh(tok string) (refreshEntry, bool) {
	if tok == "" {
		return refreshEntry{}, false
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	entry, ok := s.refresh[tok]
	if !ok {
		return refreshEntry{}, false
	}
	delete(s.refresh, tok)
	if time.Now().After(entry.expires) {
		return refreshEntry{}, false
	}
	return entry, true
}

// upsertUser records (or refreshes) the user row for a resolved identity.
func (s *Service) upsertUser(id *identity.Identity, index int32) UserRecord {
	return s.users.Update(id.UserID, func(cur UserRecord, found bool) UserRecord {
		if !found {
			cur = UserRecord{
				UserID:    id.UserID,
				CreatedAt: time.Now().UTC(),
				Index:     index,
			}
		}
		cur.AccountID = id.AccountID
		cur.PID = id.PID
		cur.NsaID = id.NsaID
		cur.ShortID = identity.ShortID(id.PID)
		cur.LastLogin = time.Now().UTC()
		return cur
	})
}

// userProto renders a stored user as the protobuf resource.
func (s *Service) userProto(rec UserRecord) *authv1.User {
	u := &authv1.User{
		Name:    s.names.User(rec.UserID),
		Account: s.names.Account(rec.AccountID),
		ShortId: rec.ShortID,
	}
	if !rec.CreatedAt.IsZero() {
		u.CreateTime = timestamppb.New(rec.CreatedAt)
	}
	if !rec.LastLogin.IsZero() {
		u.LastLoginTime = timestamppb.New(rec.LastLogin)
	}
	return u
}

// externalIDProto renders the user↔NSA-id mapping.
func (s *Service) externalIDProto(rec UserRecord) *authv1.UserExternalId {
	return &authv1.UserExternalId{
		Name: s.names.UserExternalID(rec.NsaID),
		User: s.names.User(rec.UserID),
		ExternalId: &authv1.ExternalId{
			Type: authv1.ExternalIdType_NSA_ID,
			Id:   rec.NsaID,
		},
		TokenLastIssueTime: timestamppb.New(rec.LastLogin),
	}
}

// PIDForUser returns the Nextendo account behind an NPLN user id, for the other
// services (and the dashboard) to resolve a name they were handed.
func (s *Service) PIDForUser(userID string) (uint64, bool) {
	rec, ok := s.users.Get(userID)
	if !ok {
		return 0, false
	}
	return rec.PID, true
}

// UserForPID returns the slot-0 NPLN user id of an account, if it ever logged in.
func (s *Service) UserForPID(pid uint64) (string, bool) {
	var found string
	s.users.Range(func(_ string, rec UserRecord) bool {
		if rec.PID == pid && rec.Index <= 0 {
			found = rec.UserID
			return false
		}
		return true
	})
	return found, found != ""
}

// externalToken reads the id token out of the ExternalIdToken oneof.
//
// Two variants exist: nsa_id_token (a real BAAS id token, what a console sends)
// and dummy_ext_id_token (Nintendo's development stand-in). We accept both —
// the dummy variant is what a self-built test client is most likely to send,
// and the resolver applies the same rules to either.
func externalToken(ext *authv1.ExternalIdToken) string {
	if ext == nil {
		return ""
	}
	if t := ext.GetNsaIdToken(); t != "" {
		return t
	}
	return ext.GetDummyExtIdToken()
}
