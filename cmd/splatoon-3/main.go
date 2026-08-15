// Command splatoon-3 runs the Nextendo Network online server for Splatoon 3.
//
// Splatoon 3 does NOT use NEX/PRUDP like Splatoon 2, Mario Kart 8 Deluxe or
// Smash: it talks to Nintendo's newer NPLN platform — gRPC over HTTP/2 and TLS,
// protobuf payloads, one tenant per title. So this is not a nextendo-nex game
// server; it is an NPLN tenant server that plugs into the same Nextendo stack:
//
//	nextendo-account   accounts, the ONE friend graph, presence, the online gates
//	sni-router         shares :443 with the other games' auth endpoints
//	nextendo-dashboard polls /api/stats here like it polls the NEX servers
//	STUN/TURN          NAT traversal for the peer-to-peer match (instead of the
//	                   Pia NAT-check pair the NEX titles use)
//
// See docs/ARCHITECTURE.md for the whole picture and docs/DEPLOYMENT.md for how
// to run it.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/n-popescu/splatoon-3/internal/account"
	"github.com/n-popescu/splatoon-3/internal/config"
	"github.com/n-popescu/splatoon-3/internal/dashboard"
	"github.com/n-popescu/splatoon-3/internal/identity"
	"github.com/n-popescu/splatoon-3/internal/names"
	"github.com/n-popescu/splatoon-3/internal/presence"
	"github.com/n-popescu/splatoon-3/internal/server"
	authsvc "github.com/n-popescu/splatoon-3/internal/services/auth"
	friendssvc "github.com/n-popescu/splatoon-3/internal/services/friends"
	maintenancesvc "github.com/n-popescu/splatoon-3/internal/services/maintenance"
	mmsvc "github.com/n-popescu/splatoon-3/internal/services/matchmaking"
	msgsvc "github.com/n-popescu/splatoon-3/internal/services/messaging"
	toyohrsvc "github.com/n-popescu/splatoon-3/internal/services/toyohr"
	ugcsvc "github.com/n-popescu/splatoon-3/internal/services/ugc"
	"github.com/n-popescu/splatoon-3/internal/store"
	"github.com/n-popescu/splatoon-3/internal/token"
	"github.com/n-popescu/splatoon-3/internal/wire"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	loadEnvFile()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("[npln] configuration: %v", err)
	}
	log.Printf("[npln] Splatoon 3 (NPLN) server starting: tenant=%s app=%s", cfg.TenantID, cfg.AppID)

	nb := names.Builder{TenantID: cfg.TenantID}
	accounts := account.New(cfg.AccountBaseURL, cfg.InternalKey, 3*time.Second)

	// ---- identity ---------------------------------------------------------
	resolver, err := identity.NewResolver(identity.Options{
		Secret:          cfg.Secret,
		AppID:           cfg.AppID,
		RequireProof:    cfg.RequireSignedToken,
		VerifySignature: cfg.VerifyIDTokenSignature,
		SigningKeyFile:  cfg.BaasPublicKeyFile,
		LookupNSA: func(nsaHex string) (uint64, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			return accounts.PIDByNSA(ctx, nsaHex)
		},
	})
	if err != nil {
		log.Fatalf("[npln] identity: %v", err)
	}

	tokens, err := token.NewIssuer(token.Options{
		KeyFile:  filepath.Join(cfg.DataDir, "npln_signing_key.pem"),
		Issuer:   "nextendo-npln/" + cfg.TenantID,
		TenantID: cfg.TenantID,
		AppID:    cfg.AppID,
	})
	if err != nil {
		log.Fatalf("[npln] token issuer: %v", err)
	}

	// ---- persistent stores ------------------------------------------------
	stop := make(chan struct{})
	users := openStore[authsvc.UserRecord](cfg.DataDir, "users.json", stop)
	saves := openStore[toyohrsvc.SaveRecordRecord](cfg.DataDir, "cloud_saves.json", stop)
	records := openStore[toyohrsvc.GameRecord](cfg.DataDir, "game_records.json", stop)
	festEntries := openStore[toyohrsvc.FestEntryRecord](cfg.DataDir, "fest_entries.json", stop)
	reports := openStore[toyohrsvc.ReportRecord](cfg.DataDir, "reports.json", stop)
	documents := openStore[ugcsvc.DocumentRecord](cfg.DataDir, "documents.json", stop)
	aliases := openStore[ugcsvc.AliasRecord](cfg.DataDir, "document_codes.json", stop)

	// ---- services ---------------------------------------------------------
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hub := presence.New(presence.Options{Names: nb, Accounts: accounts, AppID: cfg.AppID, TTL: cfg.PresenceTTL})
	hub.StartReporter(ctx, cfg.PresenceReportInterval)
	hub.StartReaper(ctx)

	registry := mmsvc.NewRegistry(nb, cfg.SessionTTL)
	registry.StartReaper(stop)

	configs := mmsvc.NewConfigSet(cfg.DefaultMinPlayers, cfg.DefaultMaxPlayers)
	if err := configs.LoadFile(filepath.Join(cfg.DataDir, "matchmaking.json")); err != nil {
		log.Printf("[npln] matchmaking config: %v (using defaults)", err)
	}

	ice := mmsvc.NewIceAllocator(mmsvc.IceOptions{
		Names:          nb,
		StunHost:       cfg.StunHost,
		StunPort:       cfg.StunPort,
		TurnHost:       cfg.TurnHost,
		TurnPort:       cfg.TurnPort,
		TurnSecret:     cfg.TurnSecret,
		TurnUser:       os.Getenv("NPLN_TURN_USER"),
		TurnPassword:   os.Getenv("NPLN_TURN_PASSWORD"),
		TurnCredTTL:    cfg.TurnCredTTL,
		TTL:            cfg.IceTTL,
		LatencyServers: latencyServers(cfg),
	})

	tickets := mmsvc.NewTicketStore()
	gameSessions := mmsvc.NewGameSessionService(mmsvc.GameSessionOptions{
		Names: nb, Registry: registry, Tokens: tokens, Ice: ice, Tickets: tickets,
	})
	matchmaker := mmsvc.NewMatchmakerService(mmsvc.MatchmakerOptions{
		Names: nb, Registry: registry, Tickets: tickets, Sessions: gameSessions,
		Configs: configs, Window: cfg.MatchWindow, Timeout: cfg.MatchTimeout,
	})
	matchmaker.StartMatcher(ctx)

	ugcStore, err := ugcsvc.NewStore(ugcsvc.StoreOptions{
		Names:     nb,
		Documents: documents,
		Aliases:   aliases,
		BlobDir:   filepath.Join(cfg.DataDir, "attachments"),
		BaseURL:   cfg.AttachmentBaseURL,
	})
	if err != nil {
		log.Fatalf("[npln] UGC store: %v", err)
	}

	schedule := toyohrsvc.NewScheduleService(nb)
	if err := schedule.LoadFile(cfg.ScheduleFile); err != nil {
		log.Printf("[npln] schedule: %v (serving the placeholder rotation)", err)
	}

	services := wire.Services{
		Auth: authsvc.New(authsvc.Options{
			Names: nb, Tokens: tokens, Resolver: resolver, Accounts: accounts,
			Users: users, RequireAccount: cfg.RequireAccount,
		}),
		Friends: friendssvc.New(friendssvc.Options{
			Names: nb, Accounts: accounts, Hub: hub, PollInterval: cfg.FriendsPollInterval,
		}),
		Presence: friendssvc.NewPresence(friendssvc.PresenceOptions{
			Names: nb, Hub: hub, Accounts: accounts, PollInterval: cfg.FriendsPollInterval,
		}),
		GameSession: gameSessions,
		Matchmaker:  matchmaker,
		Messaging:   msgsvc.New(msgsvc.Options{Names: nb, Registry: registry}),
		Maintenance: maintenancesvc.New(nb),
		Schedule:    schedule,
		Fest:        toyohrsvc.NewFestService(nb, schedule, festEntries),
		CloudSave:   toyohrsvc.NewCloudSaveService(nb, saves),
		GameRecord:  toyohrsvc.NewGameRecordService(nb, records),
		Screening:   toyohrsvc.NewUserScreeningService(nb, reports),
		Documents:   toyohrsvc.NewDocumentServices(ugcStore),
		UGC:         ugcsvc.NewService(ugcStore),
	}

	// ---- monitoring -------------------------------------------------------
	dash := dashboard.New(dashboard.Options{
		Game:     "splatoon3",
		Token:    cfg.DashToken,
		Registry: registry,
		Presence: hub,
		UGC:      ugcStore,
	})
	go func() {
		if err := dash.Serve(cfg.HTTPAddr); err != nil {
			log.Printf("[dashboard] stopped: %v", err)
		}
	}()

	// ---- gRPC -------------------------------------------------------------
	interceptors := &server.Interceptors{
		TenantID:    cfg.TenantID,
		Tokens:      tokens,
		Observer:    dash,
		LogBodies:   cfg.LogBodies,
		Maintenance: maintenancesvc.ActiveReason,
	}
	grpcServer, err := server.New(server.Options{
		ListenAddr:   cfg.ListenAddr,
		CertFile:     cfg.CertFile,
		KeyFile:      cfg.KeyFile,
		DisableTLS:   cfg.DisableTLS,
		Interceptors: interceptors,
		LogUnknown:   cfg.LogUnknown,
	})
	if err != nil {
		log.Fatalf("[npln] gRPC server: %v", err)
	}
	wire.Register(grpcServer, services)

	// Graceful shutdown: flush the stores and let open streams end, so a restart
	// does not lose a cloud save or leave consoles hanging on a dead stream.
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		s := <-sig
		log.Printf("[npln] %s received, shutting down", s)
		close(stop)
		cancel()
		grpcServer.GracefulStop()
	}()

	if err := server.Listen(grpcServer, cfg.ListenAddr, !cfg.DisableTLS); err != nil {
		log.Fatalf("[npln] gRPC stopped: %v", err)
	}
	log.Printf("[npln] stopped")
}

// openStore opens a persisted JSON store under the data directory and starts its
// flusher. A store that cannot be read is fatal for exactly one reason: silently
// starting with an empty cloud-save store would look like every player lost their
// progression.
func openStore[T any](dir, file string, stop <-chan struct{}) *store.JSONMap[T] {
	path := filepath.Join(dir, file)
	m, err := store.OpenJSONMap[T](path)
	if err != nil {
		log.Printf("[store] %v", err)
	}
	if m == nil {
		log.Fatalf("[store] cannot open %s", path)
	}
	m.StartFlusher(5*time.Second, stop, func(err error) {
		log.Printf("[store] cannot persist %s: %v", path, err)
	})
	return m
}

// latencyServers converts the configured latency servers for the ICE allocator.
func latencyServers(cfg *config.Config) []mmsvc.LatencyServerConfig {
	out := make([]mmsvc.LatencyServerConfig, 0, len(cfg.LatencyServers))
	for _, ls := range cfg.LatencyServers {
		out = append(out, mmsvc.LatencyServerConfig{
			Name: ls.Name, Region: ls.Region, Host: ls.Host, Port: ls.Port, Protocol: ls.Protocol,
		})
	}
	return out
}

// loadEnvFile loads KEY=VALUE lines from the file named by NPLN_ENV_FILE (or
// ".env") into the environment before the configuration is read.
//
// Same convention as nextendo-account: an operator edits one file and restarts,
// without recreating a container. A real environment variable always wins.
func loadEnvFile() {
	path := os.Getenv("NPLN_ENV_FILE")
	if path == "" {
		path = ".env"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range splitLines(string(b)) {
		line = trimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		key, value, ok := cut(line, "=")
		if !ok {
			continue
		}
		key, value = trimSpace(key), trimQuotes(trimSpace(value))
		if key == "" || os.Getenv(key) != "" {
			continue
		}
		_ = os.Setenv(key, value)
	}
	log.Printf("[npln] loaded environment from %s", path)
}

// The tiny string helpers below keep loadEnvFile free of a strings import in a
// file that is otherwise all wiring; they behave like their strings equivalents.
func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func trimQuotes(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

func cut(s, sep string) (string, string, bool) {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}
