# Deployment

Everything below is a placeholder you replace. No real address or secret appears anywhere in this
repository.

> You must supply your own legally-dumped game, keys and system files. This project ships none of
> Nintendo's code, keys or data.

## Prerequisites

- **Go 1.23+** to build.
- **A DNS you control for the client** — the emulator's DNS layer or Atmosphère's `dns_mitm` — so the
  NPLN tenant host resolves to this server.
- **A TLS certificate the client trusts** for that host (the same CA the rest of your Nextendo
  deployment uses).
- **A running `nextendo-account`**, reachable on the private network, with the shared secret and the
  internal key.
- **A STUN server, and ideally a TURN server** (coturn does both) for the peer-to-peer match. This
  replaces the `nextendo-nncs` NAT-check pair the NEX titles need — Splatoon 3 does not use it.

## DNS

```
t-dce9377b-lp1.lp1.t.npln.srv.nintendo.net   ->  SERVER_IP
```

That is the only host this server needs. Splatoon 3 also talks to the account layer
(`*.baas.nintendo.com`, `dauth-lp1…`, `aauth-lp1…`), which is already covered by the stack's existing
redirects — `Prelude-Nro` writes a hosts file with `*.nintendo.*` wildcards, so nothing extra is
required on a console.

If you front `:443` with `sni-router`, add the tenant host to its map so it forwards to this process.

## Ports

| Port | What |
| --- | --- |
| `:443` | gRPC/HTTP2 + TLS — the tenant endpoint (via `sni-router`, or directly) |
| `:8087` | plain HTTP — `/api/stats` for the dashboard, `/ugc/*` for attachments (private network only) |
| `3478` | your STUN/TURN server (not this process) |

## Configuration

Copy `example.env` to `.env` and edit. The values that MUST line up with the rest of the stack:

| Variable | Must match |
| --- | --- |
| `NEXTENDO_SECRET` (or `NEXTENDO_SECRET_FILE`) | **`nextendo-account`'s secret, byte for byte.** It signs the `nx2.` tokens *and* derives the NPLN user ids. A mismatch does not fail loudly — it silently gives every player a different identity than the one the account server publishes to friends, so friend lists come up empty. |
| `NEXTENDO_INTERNAL_KEY` | the account server's internal key |
| `NEXTENDO_ACCOUNT_URL` | where the account server listens (private network) |
| `DASH_TOKEN` | the dashboard's token |

Splatoon-3-specific settings worth knowing about:

| Variable | Default | Meaning |
| --- | --- | --- |
| `NPLN_TENANT_ID` | `t-dce9377b-lp1` | the tenant this server serves |
| `NPLN_APP_ID` | `0100c2500fc20000` | Splatoon 3's title id, reported as the game a player is in |
| `NPLN_STUN_HOST` / `NPLN_STUN_PORT` | — | your STUN server (**required** for matches) |
| `NPLN_TURN_HOST` / `NPLN_TURN_PORT` | — | your TURN relay (strongly recommended) |
| `NPLN_TURN_SECRET` | — | coturn REST-API secret; credentials are then time-limited and per-user |
| `NPLN_MATCH_WINDOW` | `20s` | how long a partially-filled room keeps waiting for more players |
| `NPLN_MATCH_TIMEOUT` | `3m` | after which a ticket fails cleanly |
| `NPLN_SESSION_TTL` | `2m` | a room whose host stopped syncing is reaped |
| `NPLN_SCHEDULE_FILE` | `schedule.json` | the rotation ([SCHEDULE.md](SCHEDULE.md)) |
| `NPLN_DATA_DIR` | `data` | cloud saves, records, documents, the signing key, `matchmaking.json` |
| `NPLN_ATTACHMENT_BASE_URL` | — | public prefix of `/ugc/*`; unset disables attachments |
| `NPLN_LOG_BODIES` | `0` | log every request/response as JSON (bring-up only) |
| `NPLN_MAINTENANCE_START` / `_END` | — | RFC3339; while active, every RPC answers "under maintenance" |
| `NEXTENDO_REQUIRE_SIGNED_TOKEN` | `0` | require the cryptographic account binding. Turn it on for an emulator-only deployment; a retail CFW Switch cannot provide it |

## TURN credentials

With `NPLN_TURN_SECRET` set, `AllocateIceServerSet` mints coturn REST-API credentials:

```
username = "<unix expiry>:<npln user id>"
password = base64(HMAC-SHA1(secret, username))
```

Configure coturn with the same secret (`use-auth-secret`, `static-auth-secret=…`). Nothing static is
ever shipped to a client, and a leaked credential expires on its own. Without the secret, the static
`NPLN_TURN_USER` / `NPLN_TURN_PASSWORD` pair is used instead.

## Data directory

```
data/
  npln_signing_key.pem   ES256 key for the tokens (generated on first start — keep it, or every
                         in-flight session is invalidated on restart)
  matchmaking.json       per-config player counts (see MATCHMAKING.md)
  users.json             the NPLN users that have logged in
  cloud_saves.json       Splatoon 3 online save records
  game_records.json      Anarchy / X / Salmon Run records
  fest_entries.json      splatfest team choices
  documents.json         UGC documents
  document_codes.json    the codes players share
  reports.json           player reports
  attachments/           UGC blobs (replays …)
```

All of it is JSON, on purpose: `cp -r data data.bak` is a valid backup, and a support question can be
answered with a text editor. Back it up before testing anything destructive.

## Running

```sh
cp example.env .env      # edit it
mkdir -p data
cp schedule.example.json schedule.json          # then edit with real content data
cp matchmaking.example.json data/matchmaking.json
go build -o splatoon-3 ./cmd/splatoon-3
./splatoon-3
```

## docker-compose (placeholders only)

```yaml
services:
  router:
    image: nextendo/sni-router
    ports: ["443:443"]
    environment:
      # add the Splatoon 3 tenant host next to the NEX auth hosts
      NPLN_S3: "splatoon3:443"

  splatoon3:
    build: .
    environment:
      NPLN_LISTEN_ADDR: ":443"
      CERT_FILE: "/certs/cert.pem"
      KEY_FILE: "/certs/key.pem"
      NEXTENDO_ACCOUNT_URL: "http://account:8080"
      NEXTENDO_SECRET: "change-me"           # SAME as the account server
      NEXTENDO_INTERNAL_KEY: "change-me"     # SAME as the account server
      NPLN_STUN_HOST: "SERVER_IP"
      NPLN_TURN_HOST: "SERVER_IP"
      NPLN_TURN_SECRET: "change-me"          # SAME as coturn
      NPLN_ATTACHMENT_BASE_URL: "http://splatoon3:8087"
      DASH_TOKEN: "change-me"
    volumes:
      - ./certs:/certs:ro
      - s3data:/app/data

  coturn:
    image: coturn/coturn
    network_mode: host
    command: >
      -n --realm=nextendo --use-auth-secret --static-auth-secret=change-me
      --no-cli --no-tls --no-dtls

volumes:
  s3data:
```

## Wiring it into the rest of the stack

Two integrations are worth doing, and both are one line each on the other side:

**`nextendo-dashboard`** — add this server's `/api/stats` to the list it polls, so Splatoon 3 shows up
next to the NEX games.

**`nextendo-account`** — add this server to `gameStatsURLs()` in `online_presence.go`:

```go
env("DASH_S3_URL", "http://splatoon3:8087"),
```

That is what makes the *one place at a time* gate see a Splatoon 3 player as playing, instead of
letting the same account play here and in Splatoon 2 simultaneously. (Presence itself already works
without it — this server pushes to `/internal/presence-batch`.)

## Verifying it works

1. Start `nextendo-account`, then this server. The log prints the tenant and whether TLS is on.
2. `curl "http://127.0.0.1:8087/api/stats?key=$DASH_TOKEN"` → JSON with zero players.
3. Point a client at it and enter online play. In order, the log should show:
   `IssuePrearrangedUserToken` (with a `pid=`), `SubscribeMaintenanceSchedules`, the `Schedule` calls,
   `Friends.*`, `PresenceService.KeepAlive`, then the matchmaking calls.
4. Anything missing shows up as `UNHANDLED /nn.npln.…` with a hexdump. That is your to-do list.

## Security checklist

- `/api/stats` is behind `DASH_TOKEN` and not reachable from the internet.
- `:8087` (UGC + stats) stays on the private network; only `:443` faces clients.
- The data directory is not world-readable (`npln_signing_key.pem` signs every identity).
- Secrets come from the environment or files, never from the repository.
- `NEXTENDO_REQUIRE_SIGNED_TOKEN=1` if your deployment is emulator-only — it makes identity
  cryptographic rather than directory-based.
