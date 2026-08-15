# HANDOFF

Everything an operator or the next agent needs to continue. Written to be read top to bottom once, then
used as a checklist.

---

## 1. What exists now

### `n-popescu/splatoon-3` — a complete NPLN server for Splatoon 3

Merged as six reviewed pull requests:

| PR | Contents |
| --- | --- |
| [#1](https://github.com/n-popescu/splatoon-3/pull/1) | NPLN `.proto` tree (vendored verbatim from kinnay/NPLN-Protocols `e55caa5`) + generated Go bindings + `scripts/generate.sh` |
| [#2](https://github.com/n-popescu/splatoon-3/pull/2) | config, identity chain, ES256 tokens, `nextendo-account` client, gRPC plumbing |
| [#3](https://github.com/n-popescu/splatoon-3/pull/3) | `Auth`, `UserService`, `Friends`, `PresenceService`, the presence hub, `docs/FRIENDS.md` |
| [#4](https://github.com/n-popescu/splatoon-3/pull/4) | `GameSessionService`, `Matchmaker`, ICE, `Messaging`, `LobbyMessaging`, `MaintenanceScheduleService` |
| [#5](https://github.com/n-popescu/splatoon-3/pull/5) | `Schedule`, `FestService`, `CloudSave`, `GameRecord`, `Replay`/`Locker`/`Canola`/`CoopScenario`, `UserScreening`, `Ugcstore` |
| [#6](https://github.com/n-popescu/splatoon-3/pull/6) | the binary, service wiring, `/api/stats`, `example.env`, deployment + testing docs |

`go build ./...`, `go vet ./...` and `go test ./...` all pass. **It has never seen the game.**

### The friends fix for `nextendo-account`

[`contrib/nextendo-account/0001-friends-console-identity-and-presence.patch`](../contrib/nextendo-account/)
— a `git am`-able patch adding per-request console identity resolution that fails closed, plus the
missing presence writer. See that directory's README for how to apply it, and
[`FRIENDS.md`](FRIENDS.md) for the analysis.

---

## 2. The single most important finding

**Splatoon 3 is not a NEX title.** It speaks NPLN: gRPC over HTTP/2, protobuf, tenant
`t-dce9377b-lp1`, endpoint `t-dce9377b-lp1.lp1.t.npln.srv.nintendo.net`. `nextendo-nex` is not
involved at any level — no PRUDP, no RMC, no Kerberos ticket, no `nextendo-nncs` NAT check (it uses ICE
instead).

If someone starts "porting splatoon-2" to Splatoon 3, stop them: that is a dead end, and it is why this
repository is standalone.

Corollary: `nextendo-account` already anticipated this. Its `/internal/npln-friends` endpoint mentions
`npln-s3` and `SubscribeFriendUsersResponse` — this server is the consumer that endpoint was written
for, and the NPLN user ids on both sides are derived identically (there is a test for it).

---

## 3. Bring-up checklist (in order)

1. **Deploy `nextendo-account`** (patched or not) and note its `NEXTENDO_SECRET` and
   `NEXTENDO_INTERNAL_KEY`.
2. **Set up STUN/TURN** (coturn). Splatoon 3 needs it; the Pia NAT-check servers are *not* used.
3. **Configure this server** from `example.env`. The one that silently ruins everything if wrong is
   `NEXTENDO_SECRET` — it must be byte-identical to the account server's, or friend lists come up empty
   while every log line looks healthy.
4. **DNS + TLS**: point `t-dce9377b-lp1.lp1.t.npln.srv.nintendo.net` at the server, with a certificate
   the client trusts. Add it to `sni-router` if `:443` is shared.
5. **Run with `NPLN_LOG_BODIES=1`** for the first session. See [`TESTING.md`](TESTING.md).
6. **Collect two things from the log** and write them down:
   - every `[mm] matchmaking config "…" is not described in the config file` line → these are the real
     config names for `data/matchmaking.json` ([`MATCHMAKING.md`](MATCHMAKING.md));
   - every `[npln] UNHANDLED /nn.npln.…` line → the methods still to implement.
7. **Replace the placeholder schedule** with real stage/rule numbers
   ([`SCHEDULE.md`](SCHEDULE.md)) and bump `schedule_set_id`.
8. **Wire the monitoring**: add this server to `nextendo-dashboard`'s poll list and to
   `nextendo-account`'s `gameStatsURLs()` ([`DEPLOYMENT.md`](DEPLOYMENT.md)).

---

## 4. What is known to be incomplete

Ordered by how likely it is to block a match.

| # | Item | Why it is open | Where |
| --- | --- | --- | --- |
| 1 | **Matchmaking config names and player counts** | Nintendo configures them server-side; the game never says. Defaults (min 2 / max 8) let two people match but are wrong for 4v4. | `data/matchmaking.json`, `internal/services/matchmaking/config.go` |
| 2 | **Schedule content** | Stage/rule/weapon ids are game data. A structurally valid placeholder ships; the numbers are guesses. | `schedule.json`, `internal/services/toyohr/schedule.go` |
| 3 | **The `host`/`port` contract** | The host publishes its address through `SyncGameSession` and joiners read it back. That is the only defensible design without a capture, but the *actual* field the game fills (address vs ICE candidate) needs confirming. | `internal/services/matchmaking/registry.go` |
| 4 | **Undocumented RPCs** | `InitializeTag`, `GetTag`, `SelectTags`, `GetViolation`, `ValidateSaveRecord` have `[UNKNOWN]` response bodies upstream. They answer an empty success **and log it**. | `internal/services/toyohr/record.go` |
| 5 | **Splatfest results** | Served `is_valid: false` on purpose. Real results need a fest-power pipeline nobody has specified. | `internal/services/toyohr/fest.go` |
| 6 | **Rank-aware matchmaking** | Property *equality* filtering works; rank lives in an opaque property. Once you know which one, a band filter is a few lines. | `QueryFilter` in `registry.go` |
| 7 | **Query cursors** | `page_size` is honoured; `page_token` / UGC cursors are not, and say so in the log. | `ugcstore.go`, `gamesession.go` |
| 8 | **`nx-account` changes** | Not a public repository, so it could not be edited. Six steps, two of which actually kill the friend bug. | [`FRIENDS.md`](FRIENDS.md) §B |
| 9 | **Latency-based matching** | Latency data arrives and is stored on the user session; nothing uses it. Fine for one region. | `internal/services/matchmaking` |

---

## 5. Copy-paste handoff message

> **Handoff — Splatoon 3 (NPLN) server + Switch friends fix**
>
> **Repos.** The Splatoon 3 server is `n-popescu/splatoon-3`, merged as PRs #1–#6, `main` is green
> (`go build`, `go vet`, `go test` all pass). The `nextendo-account` friends fix is a `git am`-able
> patch at `contrib/nextendo-account/0001-friends-console-identity-and-presence.patch` in that same
> repo (the fork `n-popescu/nextendo-account` exists but the branch could not be pushed — git
> credentials were scoped to `splatoon-3`).
>
> **The key fact.** Splatoon 3 does **not** use NEX/PRUDP. It uses NPLN: gRPC over HTTP/2 + TLS,
> protobuf, tenant `t-dce9377b-lp1`, host `t-dce9377b-lp1.lp1.t.npln.srv.nintendo.net`. Do not try to
> build it on `nextendo-nex`. NAT traversal is ICE (STUN/TURN), not the Pia NAT check, so
> `nextendo-nncs` is not involved.
>
> **What is implemented.** Auth (+UserService), Friends, PresenceService, GameSessionService,
> Matchmaker, Messaging, LobbyMessaging, MaintenanceScheduleService, Ugcstore/Screening, and the
> Splatoon-3-specific `toyohr` services: Schedule, FestService, CloudSave, GameRecord, Replay, Locker,
> Canola, CoopScenario, UserScreening. Identity resolves against `nextendo-account` (the NPLN user id is
> derived with the same HMAC, guarded by a test), presence is reported both ways, the online gates match
> the NEX servers, and `/api/stats` uses the same JSON keys as the NEX game servers so
> `nextendo-dashboard` renders it unchanged.
>
> **Nothing has been tested against the game.** `docs/TESTING.md` is the bring-up guide. Run with
> `NPLN_LOG_BODIES=1`; every unimplemented method is logged with a hexdump of its payload, and every
> unknown matchmaking config is logged with the fallback it used. Those two log streams are the to-do
> list.
>
> **The two things you must configure before it can work:**
> 1. `NEXTENDO_SECRET` byte-identical to `nextendo-account`'s — a mismatch silently empties every friend
>    list while the logs look fine;
> 2. `data/matchmaking.json` with the real per-mode player counts, and `schedule.json` with real
>    stage/rule ids (both ship as documented placeholders).
> Also set `NPLN_STUN_HOST` (and ideally TURN, with `NPLN_TURN_SECRET` matching coturn), or matches will
> not connect — `AllocateIceServerSet` fails loudly in that case, by design.
>
> **Friends bug A (nobody appears online).** Root cause: nothing reported a console that is merely
> online — only a NEX game server (for players inside that game) and the emulator fork ever wrote
> presence, and `nx-account` had no server-to-server way to write it. Fixed here by reporting Splatoon 3
> players to `/internal/presence-batch` every 30 s, publishing OFFLINE immediately when a stream closes,
> merging the account server's network-wide view into `SubscribePresences`, and setting
> `presence_deliverable`/`presence_receivable` on every friend (with them false the client shows no
> presence at all). The patch adds `POST /internal/presence` and makes every console identity lookup
> count as a liveness signal with its own longer TTL.
>
> **Friends bug B (every console adds friends as the same person).** Root cause: identity was resolved
> from `bs:did` compared against a value *derived from the account PID*, which only matches if the
> console adopts the id we minted. A console caches its device account in system save
> `8000000000000010` and presents its own id forever, so the lookup 404s — and `nx-account` then falls
> back to a process-wide "last authenticated account" variable (its own comment in `main.go` says so).
> Every console converged on whoever set the server up; reissuing a friend code changed the code, not
> the binding. The patch adds a real binding table (`/internal/bind`), one resolution entry point
> (`/internal/whoami`) that returns **404 and nothing else** when it cannot resolve, exclusive bindings
> (409 on a conflict), and `/internal/unbind` as the recovery path.
> **`nx-account` still has to do two things, and the bug is not gone until it does:** resolve via
> `/internal/whoami` with the ids from the request's own `id_token`, and **delete the global fallback**
> (answer an auth error on 404). Also call `/internal/bind` at link time so a console that already has a
> device account works without a factory reset. `docs/FRIENDS.md` §B has all six steps.
>
> **Where to start reading.** `docs/ARCHITECTURE.md` (10 minutes, covers the whole flow), then
> `docs/NPLN-PROTOCOL.md` for the wire details, then `docs/TESTING.md` when you have a console in front
> of you. `docs/HANDOFF.md` §4 is the open-items table, roughly in the order they will block you.

---

## 6. Notes for the next agent specifically

- **Vendored protocol.** `protocol/` is a byte-for-byte copy of upstream in its upstream layout; package
  mapping happens in `scripts/generate.sh` via protoc `M` flags. Keep it that way so it stays diffable
  when kinnay's decompilation is updated. `gen/` is committed; regenerate with `make generate`.
- **Answer, don't guess.** The pattern throughout: when a response shape is unknown, return an *empty
  success* and log it. An empty success leaves a feature inert; a guessed body corrupts a player's
  record or shows them fabricated data. Two places deliberately return an error instead of a plausible
  answer — `AllocateIceServerSet` with no STUN/TURN configured, and `ImportAttachment` (SSRF) — both
  documented in place.
- **Tests encode decisions, not coverage.** That a joiner cannot rewrite the host's address, that a full
  room returns the *specific* full code, that a heartbeat does not clear presence attributes, that an
  unresolvable console fails closed, that the NPLN user id matches `nextendo-account`'s derivation. If
  you change behaviour, one of these will tell you which decision you just reversed.
- **The identity derivation is a two-body problem.** `internal/identity` and `nextendo-account`'s
  `npln_friends.go` must agree forever. `internal/identity/identity_test.go` re-implements the account
  server's version rather than calling ours twice, on purpose.
- **Streams are the silent failure mode.** Every long-lived stream here sends something on a timer. If a
  feature works for 60 seconds and then stops, look at the stream's heartbeat before anything else.
