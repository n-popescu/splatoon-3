// Package config holds every tunable of the Splatoon 3 (NPLN) server.
//
// Design rule inherited from the rest of the Nextendo Network stack: NOTHING is
// hardcoded. Addresses, ports, secrets, TLS material, tenant ids and the game's
// schedule all come from the environment (or from files pointed at by the
// environment). The defaults below are safe for a local run and contain no real
// address and no secret.
package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	// ---- Identity of the tenant -------------------------------------------
	//
	// A retail Switch resolves the NPLN endpoint from the tenant id baked into
	// the game: Splatoon 3 talks to
	//
	//	https://t-dce9377b-lp1.lp1.t.npln.srv.nintendo.net
	//
	// DNS-MITM (Atmosphère hosts file / the emulator's DNS layer) points that
	// name at this server, so the tenant id is what the client sends in the
	// mandatory `npln-tenant-id` metadata field and what every resource name we
	// mint must be prefixed with.
	TenantID string
	// AppID is the Splatoon 3 title id. Reported to nextendo-account as the
	// game a player is currently in, so friends see "playing Splatoon 3".
	AppID string

	// ---- Listeners ---------------------------------------------------------
	// gRPC (HTTP/2 + TLS). Shares :443 with the other Nextendo auth endpoints
	// through sni-router, which forwards the still-encrypted stream by SNI.
	ListenAddr string
	CertFile   string
	KeyFile    string
	// TLS is only disabled for local testing against a plaintext h2c client;
	// a console always requires TLS.
	DisableTLS bool
	// HTTPAddr serves the attachment upload/download endpoints (UGC blobs) and
	// the monitoring /api/stats. Kept OFF :443 because it is plain HTTP meant
	// for the private network / the dashboard aggregator.
	HTTPAddr  string
	DashToken string

	// ---- nextendo-account (identity, friends, presence, online gates) ------
	AccountBaseURL string
	InternalKey    string
	// Secret shared with nextendo-account: signs the "nx2." NEX login tokens
	// and derives the NPLN user ids. MUST be byte-identical to the account
	// server's secret or identities will not line up.
	Secret []byte
	// RequireAccount rejects any login that cannot be tied to a Nextendo
	// account (fail-closed identity, same rule as the NEX game servers).
	RequireAccount bool
	// RequireSignedToken additionally demands the cryptographic account binding
	// (the "nnex" nx2 claim the emulator embeds in the BAAS id_token). A real
	// CFW Switch does not carry it, so it stays off by default.
	RequireSignedToken bool
	// VerifyIDTokenSignature checks the RS256 signature of the BAAS id_token
	// against BaasPublicKeyFile. Off by default: on a private deployment the
	// token arrives over TLS from a client we already gate by account, and the
	// public key is not always deployed next to this service.
	VerifyIDTokenSignature bool
	BaasPublicKeyFile      string

	// ---- Storage ----------------------------------------------------------
	DataDir string

	// ---- Matchmaking ------------------------------------------------------
	// MatchWindow is how long a matchmaking ticket keeps collecting players
	// into the same game session before the session is handed to the players.
	MatchWindow time.Duration
	// MatchTimeout is how long a ticket may stay SEARCHING before it fails.
	MatchTimeout time.Duration
	// DefaultMinPlayers / DefaultMaxPlayers apply to a matchmaking config the
	// operator has not described in matchmaking.json.
	DefaultMinPlayers int
	DefaultMaxPlayers int
	// SessionTTL evicts a game session whose host stopped calling
	// SyncGameSession (crash, closed emulator, pulled network cable). Without
	// it the session list fills with lobbies nobody is in — the exact "phantom
	// lobby" problem the NEX servers hit before they grew a reaper.
	SessionTTL time.Duration

	// ---- ICE (NAT traversal for the P2P mesh) -----------------------------
	// Splatoon 3 does not use the old Pia NAT-check + hole-punch pair; it asks
	// NPLN for an ICE server set (STUN, and TURN when a relay is needed).
	StunHost string
	StunPort int
	TurnHost string
	TurnPort int
	// TurnSecret enables coturn's REST-API credentials: the username is
	// "<expiry>:<npln user id>" and the password is
	// base64(HMAC-SHA1(secret, username)), so no static credential is shipped.
	TurnSecret     string
	TurnCredTTL    time.Duration
	IceTTL         time.Duration
	LatencyServers []LatencyServer

	// ---- Presence ---------------------------------------------------------
	// PresenceReportInterval is how often the set of players currently in
	// Splatoon 3 is pushed to nextendo-account so the Switch friend list and
	// the other games see them online.
	PresenceReportInterval time.Duration
	// PresenceTTL drops a player from that set when their client stops sending
	// PresenceService.KeepAlive.
	PresenceTTL time.Duration
	// FriendsPollInterval is how often an open SubscribeFriendUsers /
	// SubscribePresences stream re-reads the friend graph from
	// nextendo-account and pushes the diff to the console.
	FriendsPollInterval time.Duration

	// ---- Content ----------------------------------------------------------
	// ScheduleFile describes the rotation (stages, rules, Salmon Run, season,
	// splatfest). See docs/SCHEDULE.md and schedule.example.json.
	ScheduleFile string
	// AttachmentBaseURL is the public prefix handed to the client for UGC
	// attachment upload/download URIs (must reach HTTPAddr).
	AttachmentBaseURL string

	// ---- Diagnostics ------------------------------------------------------
	// LogBodies dumps every request and response as protojson. Invaluable when
	// bringing a title up (it is how you find the field the game actually
	// wants), far too chatty for a live deployment.
	LogBodies bool
	// LogUnknown logs the full path and a hexdump of the payload of any RPC we
	// do not implement, instead of silently answering Unimplemented.
	LogUnknown bool
}

// LatencyServer is one entry of ListLatencyMeasurementServers: the client pings
// these to fill the LatencyData it sends with its matchmaking ticket.
type LatencyServer struct {
	Name     string `json:"name"`
	Region   string `json:"region"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"` // udp | tcp | http
}

// Load resolves the configuration from the environment.
func Load() (*Config, error) {
	c := &Config{
		TenantID: env("NPLN_TENANT_ID", "t-dce9377b-lp1"),
		AppID:    env("NPLN_APP_ID", "0100c2500fc20000"),

		ListenAddr: env("NPLN_LISTEN_ADDR", ":443"),
		CertFile:   env("CERT_FILE", "cert.pem"),
		KeyFile:    env("KEY_FILE", "key.pem"),
		DisableTLS: envBool("NPLN_DISABLE_TLS", false),
		HTTPAddr:   httpAddr(),
		DashToken:  os.Getenv("DASH_TOKEN"),

		AccountBaseURL:         env("NEXTENDO_ACCOUNT_URL", "http://nextendo-account:8080"),
		InternalKey:            os.Getenv("NEXTENDO_INTERNAL_KEY"),
		RequireAccount:         envBool("NEXTENDO_REQUIRE_ACCOUNT", true),
		RequireSignedToken:     envBool("NEXTENDO_REQUIRE_SIGNED_TOKEN", false),
		VerifyIDTokenSignature: envBool("NPLN_VERIFY_ID_TOKEN", false),
		BaasPublicKeyFile:      env("BAAS_SIGNING_KEY", ""),

		DataDir: env("NPLN_DATA_DIR", "data"),

		MatchWindow:       envDuration("NPLN_MATCH_WINDOW", 20*time.Second),
		MatchTimeout:      envDuration("NPLN_MATCH_TIMEOUT", 3*time.Minute),
		DefaultMinPlayers: envInt("NPLN_MATCH_MIN_PLAYERS", 2),
		DefaultMaxPlayers: envInt("NPLN_MATCH_MAX_PLAYERS", 8),
		SessionTTL:        envDuration("NPLN_SESSION_TTL", 2*time.Minute),

		StunHost:    env("NPLN_STUN_HOST", ""),
		StunPort:    envInt("NPLN_STUN_PORT", 3478),
		TurnHost:    env("NPLN_TURN_HOST", ""),
		TurnPort:    envInt("NPLN_TURN_PORT", 3478),
		TurnSecret:  os.Getenv("NPLN_TURN_SECRET"),
		TurnCredTTL: envDuration("NPLN_TURN_CRED_TTL", time.Hour),
		IceTTL:      envDuration("NPLN_ICE_TTL", 30*time.Minute),

		PresenceReportInterval: envDuration("NPLN_PRESENCE_INTERVAL", 30*time.Second),
		PresenceTTL:            envDuration("NPLN_PRESENCE_TTL", 90*time.Second),
		FriendsPollInterval:    envDuration("NPLN_FRIENDS_POLL", 15*time.Second),

		ScheduleFile:      env("NPLN_SCHEDULE_FILE", "schedule.json"),
		AttachmentBaseURL: env("NPLN_ATTACHMENT_BASE_URL", ""),

		LogBodies:  envBool("NPLN_LOG_BODIES", false),
		LogUnknown: envBool("NPLN_LOG_UNKNOWN", true),
	}

	secret, err := loadSecret()
	if err != nil {
		return nil, err
	}
	c.Secret = secret

	if s := os.Getenv("NPLN_LATENCY_SERVERS"); s != "" {
		servers, err := parseLatencyServers(s)
		if err != nil {
			return nil, fmt.Errorf("NPLN_LATENCY_SERVERS: %w", err)
		}
		c.LatencyServers = servers
	}
	if !c.DisableTLS {
		if c.CertFile == "" || c.KeyFile == "" {
			return nil, fmt.Errorf("CERT_FILE and KEY_FILE are required unless NPLN_DISABLE_TLS=1")
		}
	}
	return c, nil
}

// httpAddr resolves the monitoring/UGC listener.
//
// The fleet convention is DASH_PORT (mk8 8082, s2 8083, ssbu 8084, dashboard
// 8085, acnh 8086, minecraft 8087), so Splatoon 3 takes :8088 — 8087 is already
// Minecraft's, and two servers on one host would have silently fought over it.
// NPLN_HTTP_ADDR still wins when set, for a deployment that wants a full address.
func httpAddr() string {
	if v := os.Getenv("NPLN_HTTP_ADDR"); v != "" {
		return v
	}
	return ":" + env("DASH_PORT", "8088")
}

// Tenant returns the tenant resource name ("tenants/t-dce9377b-lp1").
func (c *Config) Tenant() string { return "tenants/" + c.TenantID }

// loadSecret reads the shared Nextendo secret EXACTLY the way nextendo-account
// (loadSecret) and the NEX game servers (loadNextendoSecret) do: the raw bytes
// of NEXTENDO_SECRET when set, otherwise the hex-decoded contents of the shared
// key file. Getting this wrong does not fail loudly — it silently derives a
// different NPLN user id for every player, so friends never line up. Hence the
// hard error when neither source is available.
func loadSecret() ([]byte, error) {
	if v := os.Getenv("NEXTENDO_SECRET"); v != "" {
		return []byte(v), nil
	}
	path := env("NEXTENDO_SECRET_FILE", "nextendo_secret.key")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("no NEXTENDO_SECRET and cannot read %s: %w", path, err)
	}
	dec, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(dec) < 16 {
		return nil, fmt.Errorf("%s does not contain a hex secret of at least 16 bytes", path)
	}
	return dec, nil
}

// parseLatencyServers reads "name@region=host:port/proto,..." into the list the
// ListLatencyMeasurementServers RPC answers with.
func parseLatencyServers(s string) ([]LatencyServer, error) {
	var out []LatencyServer
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, rest, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("entry %q is not name@region=host:port/proto", entry)
		}
		region := ""
		if n, r, ok := strings.Cut(name, "@"); ok {
			name, region = n, r
		}
		proto := "udp"
		if r, p, ok := strings.Cut(rest, "/"); ok {
			rest, proto = r, p
		}
		host, portStr, ok := strings.Cut(rest, ":")
		if !ok {
			return nil, fmt.Errorf("entry %q has no port", entry)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("entry %q: bad port: %w", entry, err)
		}
		out = append(out, LatencyServer{Name: name, Region: region, Host: host, Port: port, Protocol: proto})
	}
	return out, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		// Bare number = seconds, which is how the rest of the stack spells it.
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Second
		}
	}
	return def
}
