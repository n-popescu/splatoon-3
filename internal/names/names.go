// Package names builds and parses NPLN resource names.
//
// NPLN identifies everything by a path-like resource name that is unique even
// across tenants, e.g.
//
//	tenants/t-dce9377b-lp1/users/u-qtb6z4jkvrndteijghom
//	tenants/t-dce9377b-lp1/gameSessions/gs-4f1c…/userSessions/us-9ab…
//
// The server always sends the FULL name; the client is allowed to send the
// shorthand "current" for the tenant (and, in some requests, for itself), so
// every parser here accepts "tenants/current/…" and resolves it against the
// configured tenant. Getting that wrong shows up as a flood of NotFound errors
// on an otherwise perfect implementation, which is why it lives in one place.
package names

import (
	"fmt"
	"strings"
)

// Builder mints resource names for one tenant.
type Builder struct {
	// TenantID is the bare tenant id, e.g. "t-dce9377b-lp1".
	TenantID string
}

// Tenant returns "tenants/<id>".
func (b Builder) Tenant() string { return "tenants/" + b.TenantID }

// User returns "tenants/<id>/users/<userID>".
func (b Builder) User(userID string) string { return b.Tenant() + "/users/" + userID }

// Account returns "accounts/<accountID>" (accounts are tenant-independent).
func (b Builder) Account(accountID string) string { return "accounts/" + accountID }

// UserExternalID returns "tenants/<id>/userExternalIds/<externalID>".
func (b Builder) UserExternalID(externalID string) string {
	return b.Tenant() + "/userExternalIds/" + externalID
}

// Presence returns "tenants/<id>/users/<userID>/presence".
func (b Builder) Presence(userID string) string { return b.User(userID) + "/presence" }

// FriendUser returns "tenants/<id>/users/<userID>/friendUsers/<friendUserID>".
func (b Builder) FriendUser(userID, friendUserID string) string {
	return b.User(userID) + "/friendUsers/" + friendUserID
}

// GameSession returns "tenants/<id>/gameSessions/<sessionID>".
func (b Builder) GameSession(sessionID string) string {
	return b.Tenant() + "/gameSessions/" + sessionID
}

// UserSession returns "tenants/<id>/gameSessions/<sessionID>/userSessions/<userSessionID>".
func (b Builder) UserSession(sessionID, userSessionID string) string {
	return b.GameSession(sessionID) + "/userSessions/" + userSessionID
}

// GameSessionShortAlias returns the short-code (room code) resource name.
func (b Builder) GameSessionShortAlias(code string) string {
	return b.Tenant() + "/gameSessionShortAliases/" + code
}

// GameSessionCreationTicket returns the creation-ticket resource name.
func (b Builder) GameSessionCreationTicket(id string) string {
	return b.Tenant() + "/gameSessionCreationTickets/" + id
}

// MatchmakingTicket returns the matchmaking-ticket resource name.
func (b Builder) MatchmakingTicket(id string) string {
	return b.Tenant() + "/matchmakingTickets/" + id
}

// MatchmakingConfig returns the matchmaking-config resource name.
func (b Builder) MatchmakingConfig(name string) string {
	return b.Tenant() + "/matchmakingConfigs/" + name
}

// IceServerSet returns the ICE-server-set resource name.
func (b Builder) IceServerSet(name string) string { return b.Tenant() + "/iceServerSets/" + name }

// LatencyMeasurementServer returns the latency-server resource name.
func (b Builder) LatencyMeasurementServer(name string) string {
	return b.Tenant() + "/latencyMeasurementServers/" + name
}

// Document returns "tenants/<id>/documents/<path>" (UGC: replays, lockers, …).
func (b Builder) Document(path string) string { return b.Tenant() + "/documents/" + path }

// DocumentShortAlias returns the short code of a UGC document.
func (b Builder) DocumentShortAlias(scope, code string) string {
	return b.Tenant() + "/documentShortAliases/" + scope + "-" + code
}

// SaveRecord returns "tenants/<id>/saveRecords/<id>" (Splatoon 3 cloud save).
func (b Builder) SaveRecord(id string) string { return b.Tenant() + "/saveRecords/" + id }

// VsSchedule / CoopSchedule / SeasonSchedule / LeagueSchedule name the rotation
// entries the Schedule service serves. They hang off a "target" (the game's
// name for a schedule set / region group).
func (b Builder) VsSchedule(target, id string) string {
	return b.Target(target) + "/vsSchedules/" + id
}

// CoopSchedule names a Salmon Run rotation entry.
func (b Builder) CoopSchedule(target, id string) string {
	return b.Target(target) + "/coopSchedules/" + id
}

// VsParams names a versus-parameter set.
func (b Builder) VsParams(target, id string) string { return b.Target(target) + "/vsParams/" + id }

// SeasonSchedule names a season entry.
func (b Builder) SeasonSchedule(id string) string { return b.Tenant() + "/seasonSchedules/" + id }

// LeagueSchedule names a league (event battle) entry.
func (b Builder) LeagueSchedule(id string) string { return b.Tenant() + "/leagueSchedules/" + id }

// Target returns "tenants/<id>/targets/<name>".
func (b Builder) Target(target string) string { return b.Tenant() + "/targets/" + target }

// Fest names a splatfest and its satellites.
func (b Builder) Fest(id string) string             { return b.Tenant() + "/fests/" + id }
func (b Builder) FestSchedule(id string) string     { return b.Tenant() + "/festSchedules/" + id }
func (b Builder) FestResult(id string) string       { return b.Tenant() + "/festResults/" + id }
func (b Builder) FestDecryptionKey(id string) string {
	return b.Fest(id) + "/decryptionKey"
}

// FestEntry names a user's splatfest team choice.
func (b Builder) FestEntry(userID string) string { return b.User(userID) + "/festEntry" }

// Violation names a moderation violation record for a user.
func (b Builder) Violation(userID, kind string) string {
	return b.User(userID) + "/violations/" + kind
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

// UserID extracts the user id from a name like
// "tenants/<t|current>/users/<userID>" (optionally with sub-resources such as
// "/presence" or "/friendUsers/<id>"), verifying the tenant matches.
func (b Builder) UserID(name string) (string, error) {
	rest, err := b.stripTenant(name)
	if err != nil {
		return "", err
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[0] != "users" || parts[1] == "" {
		return "", fmt.Errorf("names: %q is not a user resource", name)
	}
	return parts[1], nil
}

// GameSessionID extracts the session id from a game-session name (or from a
// user-session name below it).
func (b Builder) GameSessionID(name string) (string, error) {
	rest, err := b.stripTenant(name)
	if err != nil {
		return "", err
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[0] != "gameSessions" || parts[1] == "" {
		return "", fmt.Errorf("names: %q is not a game session resource", name)
	}
	return parts[1], nil
}

// UserSessionID extracts (gameSessionID, userSessionID) from a user-session name.
func (b Builder) UserSessionID(name string) (string, string, error) {
	rest, err := b.stripTenant(name)
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 4 || parts[0] != "gameSessions" || parts[2] != "userSessions" {
		return "", "", fmt.Errorf("names: %q is not a user session resource", name)
	}
	return parts[1], parts[3], nil
}

// LastSegment returns the final path element of a resource name, which is the
// id for every single-level resource ("…/matchmakingTickets/<id>").
func LastSegment(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// stripTenant removes the "tenants/<id>/" prefix, accepting "current" as the
// tenant the client is connected to. A name for a DIFFERENT tenant is rejected:
// resource names are globally unique on purpose, and silently serving another
// tenant's resource would be a cross-game data leak.
func (b Builder) stripTenant(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("names: empty resource name")
	}
	parts := strings.SplitN(name, "/", 3)
	if len(parts) < 3 || parts[0] != "tenants" {
		return "", fmt.Errorf("names: %q does not start with tenants/<id>/", name)
	}
	if parts[1] != b.TenantID && parts[1] != "current" {
		return "", fmt.Errorf("names: %q belongs to another tenant (%s)", name, parts[1])
	}
	return parts[2], nil
}

// NormalizeTenant expands "tenants/current" (or "current", or "") to this
// tenant's full name; any other tenant is an error.
func (b Builder) NormalizeTenant(name string) (string, error) {
	switch strings.TrimSpace(name) {
	case "", "current", "tenants/current", b.Tenant(), "tenants/" + b.TenantID:
		return b.Tenant(), nil
	}
	return "", fmt.Errorf("names: %q is not this tenant", name)
}
