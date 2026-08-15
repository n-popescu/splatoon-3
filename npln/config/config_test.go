package config

import (
	"testing"
	"time"
)

// TestGhostIdleMatchesTheAccountServer: nextendo-account polls /api/stats and
// ignores a player idle for longer than its own ghostIdleSeconds (default 900).
// Both ends of the "one place at a time" gate must use the same threshold, so this
// server reads the same variable — and accepts the fleet's bare-seconds spelling.
func TestGhostIdleMatchesTheAccountServer(t *testing.T) {
	t.Setenv("NEXTENDO_SECRET", "x")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GhostIdle != 15*time.Minute {
		t.Errorf("default = %s, want 15m (nextendo-account's default)", cfg.GhostIdle)
	}

	// The NEX servers and the account server express it in bare seconds.
	t.Setenv("NEXTENDO_GHOST_IDLE_SECONDS", "900")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GhostIdle != 15*time.Minute {
		t.Errorf("900 seconds became %s, want 15m", cfg.GhostIdle)
	}

	// A Go duration is accepted too, for consistency with the NPLN_* variables.
	t.Setenv("NEXTENDO_GHOST_IDLE_SECONDS", "5m")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GhostIdle != 5*time.Minute {
		t.Errorf(`"5m" became %s, want 5m`, cfg.GhostIdle)
	}
}

// TestNextendoHostSuppliesTheStunTurnDefault: NEXTENDO_HOST is the one variable
// that names this deployment's public address across the fleet. Splatoon 3 hands
// STUN/TURN addresses to the console, and an unset STUN host means matches never
// connect — so setting the fleet-wide variable has to be enough.
func TestNextendoHostSuppliesTheStunTurnDefault(t *testing.T) {
	t.Setenv("NEXTENDO_SECRET", "x")
	t.Setenv("NEXTENDO_HOST", "nextendo.example")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StunHost != "nextendo.example" {
		t.Errorf("STUN host = %q, want the NEXTENDO_HOST value", cfg.StunHost)
	}
	if cfg.TurnHost != "nextendo.example" {
		t.Errorf("TURN host = %q, want the NEXTENDO_HOST value", cfg.TurnHost)
	}

	// A title-specific value still wins: STUN and TURN can live elsewhere.
	t.Setenv("NPLN_TURN_HOST", "turn.example")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StunHost != "nextendo.example" {
		t.Errorf("STUN host = %q, want the NEXTENDO_HOST value", cfg.StunHost)
	}
	if cfg.TurnHost != "turn.example" {
		t.Errorf("TURN host = %q, want the explicit NPLN_TURN_HOST", cfg.TurnHost)
	}
}

// TestDashPortFollowsTheFleetConvention: the aggregator addresses each game by
// DASH_PORT, and 8087 is already Minecraft's.
func TestDashPortFollowsTheFleetConvention(t *testing.T) {
	t.Setenv("NEXTENDO_SECRET", "x")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8088" {
		t.Errorf("default monitoring address = %q, want :8088", cfg.HTTPAddr)
	}

	t.Setenv("DASH_PORT", "9099")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":9099" {
		t.Errorf("DASH_PORT was ignored: %q", cfg.HTTPAddr)
	}

	// A full address still wins, for binding one interface.
	t.Setenv("NPLN_HTTP_ADDR", "127.0.0.1:9100")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != "127.0.0.1:9100" {
		t.Errorf("NPLN_HTTP_ADDR was ignored: %q", cfg.HTTPAddr)
	}
}
