package main

// Small helpers shared by the root package, named as they are in every other
// Nextendo game server (envOr, envOrInt, loadNextendoSecret) so the wiring reads
// the same across the fleet.

import (
	"encoding/hex"
	"log"
	"os"
	"strconv"
	"strings"
)

// envOr returns the environment variable or a default.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envOrInt returns the environment variable as an int, or a default.
func envOrInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// accountBaseURL is where nextendo-account listens. Same variable, same default
// as the NEX servers.
func accountBaseURL() string { return envOr("NEXTENDO_ACCOUNT_URL", "http://nextendo-account:8080") }

// internalKey lets this server call the account server's /internal/* control plane.
func internalKey() string { return os.Getenv("NEXTENDO_INTERNAL_KEY") }

// loadNextendoSecret reads the shared Nextendo secret EXACTLY as
// nextendo-account's loadSecret and each NEX game server's loadNextendoSecret do:
// the raw bytes of NEXTENDO_SECRET when set, otherwise the hex-decoded contents of
// the shared key file.
//
// Getting this wrong does not fail loudly — it silently derives a different
// identity for every player, so friend lists come up empty while everything looks
// healthy. Hence the loud log line naming which source was used.
func loadNextendoSecret() []byte {
	if v := os.Getenv("NEXTENDO_SECRET"); v != "" {
		return []byte(v)
	}
	path := envOr("NEXTENDO_SECRET_FILE", "nextendo_secret.key")
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("[auth] WARNING: no NEXTENDO_SECRET and %s unreadable (%v): identities will NOT match the account server", path, err)
		return nil
	}
	secret, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(secret) < 16 {
		log.Printf("[auth] WARNING: %s is not a valid hex secret: identities will NOT match the account server", path)
		return nil
	}
	return secret
}
