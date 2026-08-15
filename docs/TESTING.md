# Testing and bring-up

Nobody has put a console in front of this server yet. This is the order to do it in, and how to read
what comes back.

## 0. Before a console

```sh
go build ./cmd/splatoon-3 && go test ./...
```

Then run it locally without TLS and without a real account server, just to see it come up:

```sh
NPLN_DISABLE_TLS=1 NPLN_LISTEN_ADDR=127.0.0.1:50051 \
NEXTENDO_SECRET=dev-secret-not-for-production \
NPLN_DATA_DIR=/tmp/s3data \
NPLN_LOG_BODIES=1 \
./splatoon-3
```

You should see the tenant line, the schedule line, and the two listeners. Then:

```sh
curl -s "http://127.0.0.1:8087/api/health"
curl -s "http://127.0.0.1:8087/api/stats"        # DASH_TOKEN unset -> open locally
```

`grpcurl` is the fastest way to poke at it without a console. The server has no reflection service (the
console never asks for one), so pass the vendored protos:

```sh
grpcurl -plaintext -import-path protocol -proto proto/auth/v1/auth.proto \
  -H 'npln-tenant-id: t-dce9377b-lp1' \
  -d '{"tenant":"tenants/current","external_id_token":{"dummy_ext_id_token":"<a JWT>"}}' \
  127.0.0.1:50051 nn.npln.auth.v1.Auth/IssueAnonymousUserToken
```

## 1. Point the game at it

- **Emulator**: it already redirects the Nintendo hostnames; add the Splatoon 3 tenant host
  (`t-dce9377b-lp1.lp1.t.npln.srv.nintendo.net`) to your server's address.
- **Console (Atmosphère)**: `Prelude-Nro`'s hosts file already covers `*.nintendo.*` wildcards, which
  includes the tenant host, so a console configured for Nextendo needs nothing extra.
- **TLS**: the game verifies the certificate. On a console this needs the usual `nn::ssl` bypass /
  custom CA that the rest of the stack already relies on. `kinnay/NPLN-Protocols` also ships
  `generate_patch.py`, which builds an IPS patch that disables certificate verification in the main NSO
  — useful when you are debugging TLS itself rather than the protocol.

## 2. The first login

What a healthy start looks like, in order:

```
[rpc] /nn.npln.auth.v1.Auth/IssuePrearrangedUserToken [anonymous] ok (12ms)
[auth] IssuePrearrangedUserToken pid=1800000042 slot=0 user=u-qoahvkaf4bclq6uqu6in nsa=8ca8… proven=true
[rpc] /nn.npln.maintenance.v1.MaintenanceScheduleService/SubscribeMaintenanceSchedules [pid=1800000042 …] stream opened
[rpc] /nn.npln.toyohr.v1.Schedule/SelectVsSchedules [pid=1800000042 …] ok (1ms)
```

Common failures, and what they mean:

| Log line | Cause | Fix |
| --- | --- | --- |
| `invalid npln-tenant-id ("t-…")` | the client is a different game, or `NPLN_TENANT_ID` is wrong | set the tenant id the game sends |
| `[auth] REFUSED: identity: no Nextendo account for this console` | the console's NSA id is not linked | link the account (see [FRIENDS.md](FRIENDS.md)) |
| `[auth] pid=… REFUSED by the online gate (unverified)` | account e-mail not verified | verify it, or relax the gate on the account server |
| `[auth] pid=… REFUSED by the online gate (elsewhere)` | the account is playing somewhere else | close the other session, or wait out the ghost timeout |
| `id token rejected: identity: malformed id token` | not a JWT, or a different token entirely | dump it with `NPLN_LOG_BODIES=1` and compare with the shape in [NPLN-PROTOCOL.md](NPLN-PROTOCOL.md) |
| nothing at all in the log | DNS or TLS: the game never reached us | check the redirect, then the certificate |

## 3. Turning `UNHANDLED` into an implementation

Every unimplemented method is logged with its path and a hexdump:

```
[npln] UNHANDLED /nn.npln.toyohr.v1.GameRecord/PrepareBankaraPowerMeasurement from 10.0.0.5:52344 payload=52 bytes
00000000  0a 30 74 65 6e 61 6e 74  73 2f 74 2d 64 63 65 39  |.0tenants/t-dce9|
```

1. Look the method up in `protocol/proto/…` — the request and response messages are usually there even
   when the wiki has no page for them.
2. Decode the payload against the request message to confirm which fields the game fills.
3. Implement it in the matching `internal/services/…` file, register it if the whole service was
   missing, and add a test.
4. If the response message is `[UNKNOWN]` in the definitions, answer an **empty success** and log it —
   that is what the existing undocumented methods do (`InitializeTag`, `GetViolation`,
   `ValidateSaveRecord`). An empty success leaves a feature inert; a guessed body can corrupt a player's
   record.

## 4. Matchmaking with two clients

1. Both enter the same mode. Watch for:

   ```
   [mm] ticket mt-… pid=1800000042 config="tenants/…/matchmakingConfigs/…" queued (1 waiting)
   [mm] matchmaking config "…" is not described in the config file; using min=2 max=8
   [mm] formed room gs-… for config="…" with 2 ticket(s) / 2 player(s)
   [mm] session gs-… host published its address: 203.0.113.7:30000
   ```

2. **Write down the config names** from those lines — they are what `data/matchmaking.json` needs
   ([MATCHMAKING.md](MATCHMAKING.md)).
3. If the room forms but the match never starts, the problem is P2P, not matchmaking: check
   `AllocateIceServerSet` answered, that STUN/TURN are reachable **from the consoles**, and that the
   host's published address is its public one.
4. If players each get their own room, they are in different configs, or one of them is not public /
   not accepting participants. `/api/stats` shows every room with its flags.

## 5. Friends and presence

See the checklist at the end of [FRIENDS.md](FRIENDS.md). The single most common failure is a mismatched
`NEXTENDO_SECRET`, which produces an *empty but healthy-looking* friend list:

```
[friends] ListFriendUsers pid=1800000042 -> 0 friend(s)
```

with a friend graph that is fine on the website. `go test ./internal/identity/` proves the derivation
matches the account server's, so the difference can only be the secret.

## 6. Things worth watching over a long session

| Symptom | Where to look |
| --- | --- |
| rooms accumulate in `/api/stats` | the reaper: hosts are not syncing, or `NPLN_SESSION_TTL` is too long |
| friends flicker offline | presence TTL vs the client's keepalive interval; check `[presence]` lines |
| a player cannot rejoin ("playing elsewhere") | the account server's ghost timeout — add this server to its `gameStatsURLs()`, see [DEPLOYMENT.md](DEPLOYMENT.md) |
| memory grows | tickets or presences not being swept; both have reapers, but a leaked *stream* keeps its subscription |
| `stream closed after 0s` immediately after opening | the client rejected our first message — turn on `NPLN_LOG_BODIES=1` and compare the shape |

## 7. Regression tests

```sh
go test ./...                     # everything
go test ./internal/identity/      # identity + the account-server derivation guard
go test ./internal/services/matchmaking/   # rooms, tickets, ICE, room codes
go test ./internal/presence/      # presence hub and its fallbacks
go test ./internal/services/toyohr/        # schedule slots, etags, fest
```

Add a test for every behaviour you discover from a capture. The ones that exist encode decisions that
are easy to break by accident — that a joiner cannot rewrite the host's address, that a full room
reports the specific "full" code, that a heartbeat does not clear presence attributes.
