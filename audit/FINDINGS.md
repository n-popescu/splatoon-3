# Findings

Each finding: what it is, why it matters, the evidence, and what the patch does. Line numbers are from
the `main` of each repository at the time of the audit.

---

## F1 — `nextendo-nncs`: the NAT files are rewritten non-atomically, and the join path reads them

**Severity: High (breaks matches, intermittently).** Fixed by `patches/02`.

`nextendo-nncs` is the only component that ever observes a console's real external UDP endpoint. It
publishes what it sees to two files:

```go
// nextendo-nncs/main.go:63, :97
_ = os.WriteFile(natFile, []byte(b.String()), 0644)
_ = os.WriteFile(typeFile, []byte(b.String()), 0644)
```

`nextendo-nex` reads the first one **on the join path** — `natBridgeStations` inside
`GetSessionURLs` (`natbridge.go:113`) — to hand a joiner the host's reachable address instead of the
useless WebSocket TCP port.

`os.WriteFile` truncates the file and then writes it. A reader that lands in that window sees an empty
or half-written file, so the host's endpoint looks **missing**: the joiner is handed the TCP port, its
probe never lands, and the console gives up with a communication error. And the window was open almost
continuously, because both files were rewritten **in full on every single probe**.

**Evidence.** The test added by the patch, run against the original code:

```
nat_files_test.go:82: 239 read(s) saw a truncated/incomplete file
                      — the join path would hand out the wrong endpoint
```

239 of 500 reads (48 %) while a single writer was active. With the fix: 0.

This is the shape of failure that is impossible to debug from a bug report: it is timing-dependent, it
affects the *joiner* while the host looks fine, and it leaves nothing in either log.

Two more problems in the same file:

- **Write amplification.** Every datagram on an unauthenticated public UDP port caused two full-file
  rewrites. Cheap packets in, sustained disk writes out.
- **Unbounded tables.** `natMap` and `natSeen` never expired an entry, so both the tables and the files
  grew for the life of the process — one entry per source IP that ever sent a datagram.

**The fix:** write to a temporary file in the same directory and rename (atomic swap: a reader sees the
old contents or the new ones, never a truncation); debounce writes to 250 ms and only write when the
content actually changed; expire entries after 30 minutes.

---

## F2 — A leaked login token was revoked in 3 components out of 9

**Severity: High (account takeover with a known-public credential).** Fixed by `patches/10`, `12`–`18`.

A `nx2.` token is an HMAC over `pid.username.expiry` with a 30-day life. One leaked: the 1.6.5 Windows
release was packaged from a folder containing a live session file, so it shipped to every downloader.
Since the secret cannot be rotated without logging out every player and changing every account's
derived ids, each server carries a denylist checked even when the signature verifies —
`revokedNexPayloads`, with a comment in `nextendo-account/main.go:854`:

> *Keep this in sync with the identical list in each NEXtendo game server.*

They did not stay in sync:

| Component | Leaked payload revoked? |
| --- | --- |
| `nextendo-account` | yes |
| `mario-kart-8-deluxe` | yes |
| `luigis-mansion-3` | yes |
| `splatoon-2` | **no** — empty map |
| `super-smash-bros-ultimate` | **no** |
| `animal-crossing-new-horizons` | **no** |
| `super-mario-maker-2` | **no** |
| `mario-strikers` | **no** |
| `minecraft` | **no** |

So a credential that is public, and believed dead, still played online as PID 1800000006 on six of the
eight game servers. Worse than the impersonation: through the *one place at a time* gate, whoever holds
it also keeps the real owner **out** of online play, because the account looks like it is already
playing.

**The fix** does both halves. The known payload is added where it was missing, and `revoked.go` makes
the list configurable — `NEXTENDO_REVOKED_TOKENS` and `NEXTENDO_REVOKED_TOKENS_FILE`, merged at
start-up before the listener accepts anything — so the next revocation is one deployment instead of
nine source edits that can drift again. A denylist file that fails to load is reported loudly, because
silently accepting a revoked token is worse than having no denylist.

Each patch also adds a regression test asserting the known payload is refused.

---

## F3 — Five game servers never reported presence, so their players were invisible to friends

**Severity: High (this is the "friends are never online" report).** Fixed by `patches/11`–`15`.

`nextendo-account` keeps one presence table for the whole network — the Switch home-menu friend list,
the website and every other game read it — with a 90 s TTL and exactly two writers: the emulator fork,
and a game server reporting its players.

| Game server | Reports presence? |
| --- | --- |
| `splatoon-2` | yes (`presence.go`) |
| `animal-crossing-new-horizons` | yes |
| `luigis-mansion-3` | yes (`friends.go`) |
| **`mario-kart-8-deluxe`** | **no** |
| **`super-smash-bros-ultimate`** | **no** |
| **`super-mario-maker-2`** | **no** |
| **`mario-strikers`** | **no** |
| **`minecraft`** | **no** |

A player in Mario Kart 8 Deluxe — the most played title on the network — was **offline to all of their
friends for the entire session**, on a Switch, in another game and on the website. Presence is also how
a friend decides whether it is worth trying to join, so this is not cosmetic.

**The fix** adds the reporter that `splatoon-2` has been running to each of the five servers: every PID
that sends a PRUDP packet is playing right now, and the active set is pushed to
`/internal/presence-batch` every 30 s (comfortably inside the 90 s TTL), dropping PIDs that stop
sending so a player who quits or crashes falls offline by themselves.

Two details that matter and were easy to get wrong:

- the report carries `X-Internal-Key`; without it the account server's control-plane guard drops the
  request and presence silently never appears;
- the title id comes from `nextendo-account`'s own game table (`games.go`), so the friend list names the
  right game.

This is the other half of the friends work in [`../docs/FRIENDS.md`](../docs/FRIENDS.md), which covers
the console side.

---

## F4 — `nx-scsi`: cloud-save URLs were unsigned and never expired, and nothing checked ownership

**Severity: High (read and overwrite another player's save).** Fixed by `patches/04`.

Three problems compounding each other.

**1. The "signed URLs" were not signed.**

```go
// nx-scsi/main.go:291
func getURL(a *Archive, c *ComponentFile) string {
	return "https://scsi-download…/" + a.NsaID + "/" + a.ApplicationID +
		"/" + u64s(c.ID) + "_" + itoa(c.Index) + ".bin?Expires=" + itoa(…+3600) + "&nx=1"
}
```

`Expires` was decorative: `downloadBlob` and `uploadBlob` (`handlers.go:459`, `:479`) never looked at
it, never checked a signature, and never required a token. **The URL was the credential, it never
expired, and `PUT` to it overwrote the save.** These URLs are also printed to the log
(`START_UPLOAD resp=…`, `COMPONENT_CREATE resp=…`), so a shared log, a paste or a screenshot handed out
permanent authority over a player's save data.

**2. No ownership check.** `GET /save_data_archives/<id>`, `start_download`, `start_upload`,
`finish_upload`, `generate_key_seed_package` and every `component_files/<id>/…` action took only the
numeric id. `nsaFromToken(r)` was used to *stamp* a new archive and to filter the *list*, and nowhere
else. The ids come from `crypto/rand`, which makes them hard to guess — but that is not an
authorisation check, and the ids travel in URLs and logs.

**3. Unbounded upload.** `io.Copy(f, r.Body)` straight into the file: one client could fill the
filesystem, which takes down every player's saves rather than only its own. A client that vanished
mid-upload also left a **truncated** blob in place of the previous complete one — silent corruption the
player only discovers when they restore it.

Plus a data race: component fields (`Status`, `ArchiveSize`, `EncodedArchiveDigest`) were mutated and
read outside `store.mu`, and `persist()` marshalled the live object while another request could be
changing it, which can also emit half-updated JSON.

**The fix:** HMAC-signed URLs over (operation, owner, title, component, index, expiry), verified before
any I/O — so a download link cannot be replayed as an upload, and an expired one stops working; an
ownership check that refuses a request carrying a *verified* identity different from the archive's owner
(a request with no verifiable token keeps the previous behaviour, because the console does not always
present one); `MaxBytesReader` on uploads (`SCSI_MAX_BLOB_BYTES`, default 64 MiB) and on metadata
bodies; temp-file-and-rename for both blobs and `archive.json`; and mutations moved under the lock.

`SCSI_ALLOW_UNSIGNED_BLOBS=1` exists to finish migrating consoles holding old URLs, and logs a warning
every time it is used.

---

## F5 — `nextendo-nex`: 16-bit sequence wraparound turns into a permanent retransmit storm

**Severity: Medium (degrades every long session; affects all games).** Fixed by `patches/01`.

```go
// nextendo-nex/endpoint.go:697
func (c *Connection) ackPackets(base uint16, extra []uint16) {
	for pid := range c.pending {
		if pid <= base {          // ← not wrap-safe
```

Reliable sequence ids are 16 bits and `outReliable` wraps. After 65535 packets, everything still
pending from *before* the wrap compares greater than every id after it, so it is never acknowledged:

```
pending = [65530, 65531, 65534, 65535, 0, 1, 2],  ack base = 2
original comparison leaves pending: [65530, 65531, 65534, 65535]   ← forever
wrap-safe comparison leaves:        []
```

`retransmitLoop` then re-sends those packets **every 2 seconds for the rest of the connection**, the
`pending` map never shrinks, and the console keeps receiving duplicates of messages it already
processed. 65 000 reliable packets is unremarkable for a lobby that stays up — each large DataStore
payload is 13 of them.

**The fix:** `int16(pid-base) <= 0`, the standard wrap-safe sequence comparison, with tests that also
assert an ack does **not** clear packets the console has not seen (a genuinely lost fragment must still
be retransmitted).

---

## F6 — `nextendo-nex`: `Connection.state` is raced, and `OnDisconnect` could run twice

**Severity: Medium (writes to closed connections; lost lobbies).** Fixed by `patches/01`.

`state` was a plain `int`, written by the receive goroutine (`processCONNECT`) and by the transport
(`Close`), and read by `retransmitLoop` and by the paced multi-fragment sender — both of which test it
to decide whether to stop. That is a data race by the memory model, and its practical effect is
continuing to write to a connection that is gone.

`close()` also did a test-then-set:

```go
if c.state == stateClosed { return }
c.state = stateClosed
```

so two concurrent closes (the transport closing the socket while the receive goroutine handles a
`DISCONNECT`) could both pass and call `OnDisconnect` twice. Every game server wires `OnDisconnect` to
`mm.RemovePlayer`, which deletes the player's gathering — running it twice removes a gathering a
*reconnecting* player has just created.

**The fix:** `atomic.Int32` plus `CompareAndSwap`, with a test that eight concurrent `Close` calls run
`OnDisconnect` exactly once.

---

## F7 — `sni-router` never sends PROXY protocol, so the auth server sees the router's IP

**Severity: Medium (breaks the ticketless secure path; blinds the monitoring).** Fixed by `patches/03`.

The backend terminates TLS itself, so the only thing it learns about the client is the TCP peer —
behind this router, that is the router. The consequences are specific:

- the auth server remembers the login PID keyed by the client IP (`RememberAuthPID`), and the console's
  secure connection arrives **directly** on the game's own port with its real IP, so `RecallAuthPID`
  misses. A console reaching the secure server without a decryptable ticket is refused (and in older
  builds inherited a placeholder PID, which is the 2618-562 failure the code comments describe);
- `/api/stats` shows the router's address, geolocation and ISP for every player;
- any per-IP reasoning on the auth port is blinded.

`nextendo-nex` has parsed PROXY v1 for exactly this reason since the deployment sat behind Traefik
(`proxyproto.go`, `ListenSecureProxy`, enabled per game with `NEXTENDO_PROXY_PROTOCOL=1`) — but the
project's own recommended front for sharing `:443` never sent the header, so that support had nothing
to parse.

**The fix:** emit a PROXY v1 line before the replayed ClientHello, gated behind
`SNI_SEND_PROXY_PROTOCOL=1` (off by default: sending it to a backend that is not expecting it would
corrupt the TLS stream). Tests assert the exact shape `nextendo-nex` parses, and that it precedes the
ClientHello.

---

## F8 — `sni-router`: accept-error spin loop and a file-descriptor leak

**Severity: Medium (self-inflicted outage on a shared box).** Fixed by `patches/03`.

```go
for {
	c, err := ln.Accept()
	if err != nil {
		continue          // ← a permanent error spins at 100% CPU
	}
	go handle(…)
}
```

and in `handle`:

```go
go func() { _, _ = io.Copy(up, c) }()
_, _ = io.Copy(c, up)     // ← only one direction is waited on
```

A half-open connection (a crash, a closed lid, a dropped Wi-Fi link) leaves the other `io.Copy` blocked
forever: one goroutine and two file descriptors per abandoned connection, for the life of the process.
That ends as `accept: too many open files` — which the loop above then spins on, on a box that is also
running the game servers.

**The fix:** exponential back-off on accept errors, and tearing down both directions when either
finishes. Also a `parseSNI` test over every truncation of a ClientHello, because that parser is exposed
to anything that finds a public `:443`.

---

## F9 — `nextendo-account`: the default trusted subnet is an example CIDR

**Severity: Medium. Not fixed — it is a deployment decision.**

`internal_guard.go:57`:

```go
rules := "10.0.0.0/24 10.0.0.1"     // overridable via internal_net.conf
```

Authorisation for `/internal/*` is "correct key **or** source inside a trusted subnet (but not its
gateway)". With no `internal_net.conf` and no `NEXTENDO_INTERNAL_KEY`, the only gate is that default
subnet — and `10.0.0.0/24` is a very common LAN/VPN range. On such a deployment, any host on the LAN can
call:

- `/internal/identity?pid=` — nickname, friend code, friend list, play history;
- `/internal/npln-friends?pid=` — the friend graph **and** `account_hex`, the NSA id that acts as an
  online identity credential;
- `/internal/login` — credential validation, i.e. an unthrottled oracle.

The guard itself is well built (real TCP source, never `X-Forwarded-For`, constant-time key compare).
The problem is only the *default*.

**Recommendation:** default to **no** trusted subnet, so the key is required unless a subnet is
explicitly configured; keep loopback allowed for local diagnostics. One line, and it makes the safe
deployment the default one.

---

## F10 — Dashboards: `DASH_TOKEN` unset means fully open

**Severity: Low-Medium. Not fixed — it changes an operator-visible behaviour.**

Every game server:

```go
if token != "" && subtle.ConstantTimeCompare(…) == 1 { /* serve */ }
```

and the aggregator, `nextendo-dashboard/main.go:431`:

```go
if token == "" || r.URL.Query().Get("key") == token {
```

So with `DASH_TOKEN` unset, `/api/stats` — player names, PIDs, IP addresses, NAT types, lobby contents
— is served to anyone who can reach the port. The aggregator additionally compares the token with `==`
rather than `subtle.ConstantTimeCompare`, unlike the game servers.

**Recommendation:** refuse to serve when no token is configured (fail closed, log the reason at
start-up), and use a constant-time comparison in the aggregator too. `nextendo-account`'s
`online-check` gate depends on `/api/stats`, so the token must be *set*, not the check removed.

---

## F11 — All game servers: `NEXTENDO_REQUIRE_ACCOUNT` defaults to off

**Severity: Low-Medium. Not fixed — an intentional default worth revisiting.**

```go
requireAccount = os.Getenv("NEXTENDO_REQUIRE_ACCOUNT") == "1"
```

Identical in all eight servers. Forget the variable and `resolveUser` falls through to
`anonymousPID(username)`: a login with no Nextendo identity is accepted, with a PID derived from a
string the client chooses. The docs describe online as account-only, and every other gate (verified
e-mail, one place at a time, bans) is bypassed for such a session.

**Recommendation:** default to **on** and let a deployment opt out (`=0`) for local testing — which is
what the Splatoon 3 server in this repository does. Fail-closed identity should not depend on
remembering an environment variable.

---

## F12 — Smaller robustness issues

**Severity: Low. Not fixed.**

- **`nx-dauth/main.go:248`, `:265`** — `io.ReadAll(r.Body)` with no cap on a public `:443` handler. A
  trivial memory-exhaustion primitive; `nextendo-account` caps every body it reads, and this service
  should too.
- **`mario-strikers/strikers_club.go:204`** — `os.WriteFile(clubStorePath(), data, 0o644)`: persistent
  player state (clubs) written non-atomically, so a crash mid-write truncates it permanently. Same
  temp-and-rename treatment as F1; `nextendo-account` already does this everywhere.
- **File permissions.** `nextendo-nncs` writes player IP addresses with `0644`, and `nx-scsi` wrote
  `archive.json` `0644` (the patch tightens the latter to `0600`). Minor, but these are player data.

---

## F13 — Tooling and repository hygiene

**Severity: Low. Not fixed.**

- **`super-mario-maker-2` cannot pass its own tests in a clean checkout:**

  ```
  smm2_profile_test.go:25: read m49: open measured/a response: no such file or directory
  ```

  The fixture is not in the repository (verified on the pristine upstream commit), so `go test ./...`
  is red for everyone — which means nobody runs it, and the other tests in that repo are not protecting
  anything.
- **`go vet` warning in `nextendo-account`:** `main.go:1497: unreachable code` — the retired guest
  handler returns before its body. Harmless, but it makes `go vet ./...` non-clean, so a real warning
  will be missed.
- **Go version drift:** `nextendo-dashboard` requires `go >= 1.26` and `baas-jwks` `go >= 1.26.4`,
  while the other modules ask for 1.21–1.23 and `nextendo-docs/DEPLOYMENT.md` says "Go 1.23+". A
  contributor following the documentation cannot build those two.

---

## F14 — Cross-checks against the open GitHub issues

Not findings in the code, but worth recording next to it.

- **`Ryujinx-Nextendo` #1 — "Splatoon 3 hangs on boot when Guest Internet Access is enabled".**
  Consistent with the NPLN endpoint being redirected to something that does not answer: with internet
  access on, Splatoon 3 reaches for its NPLN tenant at boot and waits. The server in this repository is
  the missing listener; point `t-dce9377b-lp1.lp1.t.npln.srv.nintendo.net` at it and that path has
  something to talk to. Independently, the emulator should not let a network wait block boot — the
  report also mentions the emulator failing to exit cleanly afterwards, which is a client-side issue.
- **`Ryujinx-Nextendo` #3 — MK8D cannot connect; TLS completes, then the socket closes with no HTTP
  request.** The handshake succeeds and ALPN negotiates `http/1.1`, so DNS and certificates are fine
  and the failure is after the handshake. Two things in this audit are worth eliminating first: F7 (if
  the deployment sets `NEXTENDO_PROXY_PROTOCOL=1` on the game while the router does not send the
  header, the auth sees the router's IP and the ticketless secure path breaks) and F2/F11 (an identity
  that resolves to nothing gets refused at `LoginEx` — the server log line says which).
- **`Prelude-Nro` #9 — 2123-0308 when linking an account.** Account-linking, i.e. the same area as the
  console-identity work in [`../contrib/nextendo-account/`](../contrib/nextendo-account): with
  `/internal/whoami` and `/internal/bind`, a console's *own* device account becomes a recognised
  binding instead of having to match an id derived from the PID.
- **`Prelude-Nro` #3 — "why does PRODINFO need to be exposed?"** A legitimate question that deserves a
  documented answer in the repository rather than an issue thread: which services need the client
  certificate, and what a deployment does and does not send.

---

## The pattern behind F2 and F3

Both are the same mechanism: **the same file copied into eight repositories**. The token validator
exists nine times, `dashboard.go` eight times, and presence in three variants. Each copy drifts, and the
drift is invisible until somebody diffs them — which is exactly how these two were found.

The patches fix the instances *and* remove the need to remember (the revocation list is now
configuration, so the next leak is one deployment). The durable version of that fix is to move the two
shared concerns into `nextendo-nex`, which every game server already imports:

- `nex.ValidateNextendoToken(...)` — one implementation of the `nx2.` check, including revocation;
- `nex.StartPresenceReporter(endpoint, appID)` — one reporter, wired from `OnRMC`.

That would delete roughly 200 duplicated lines per game server and make this class of finding
impossible rather than merely fixed.
