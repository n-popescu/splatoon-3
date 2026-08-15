# Integration with the Nextendo stack

This is the document to read before adopting this repository. It answers the two questions a
maintainer asks: *why is this one not built on `nextendo-nex`*, and *what do I have to change
anywhere else to run it*.

---

## 1. Why this title cannot be a NEX server

Splatoon 3 does not use NEX/PRUDP. It uses **NPLN** — Nintendo's newer platform: gRPC over HTTP/2
and TLS, protobuf messages, one tenant per title. The evidence, all of it checkable:

| | Splatoon 2 (NEX) | Splatoon 3 (NPLN) |
| --- | --- | --- |
| Game server id | `24E30D00` / `2C4BFF00` / `2DF33D01` | **none — it has no NEX server** |
| NEX access key | `f25e0f69` / `f73d3ebe` / `4eb18d39` | **none** |
| Host the console reaches | `g2*.s.n.srv.nintendo.net` | `t-dce9377b-lp1.lp1.t.npln.srv.nintendo.net` |
| Transport | PRUDP over UDP, RMC messages | gRPC over HTTP/2 + TLS, protobuf |
| Auth | `LoginEx` → Kerberos ticket | `IssuePrearrangedUserToken` → a signed NPLN token |
| Matchmaking | `MatchmakeExtension` gatherings | `Matchmaker` tickets + `GameSession` rooms |
| NAT traversal | Pia NAT-check pair (`nextendo-nncs`) | **ICE**: STUN/TURN |
| Presence | inferred from PRUDP traffic | an explicit `PresenceService` with keep-alives |

A PRUDP listener on the NPLN host would complete the TLS handshake and then sit there: the client
sends HTTP/2 frames, which a NEX server cannot parse, and the game fails with nothing useful in
either log. That failure mode is real and worth knowing about — it is exactly what happens today if
`sni-router` has no NPLN route and the connection falls through to a NEX auth server
(`audit/FINDINGS.md`, F19).

So the protocol layer is dictated by the game. **Everything else conforms to the fleet**, and that is
what the rest of this document is about.

## 2. What is identical to the NEX servers, on purpose

| | Here | Same as |
| --- | --- | --- |
| Module path | `github.com/NextendoNetwork/splatoon-3` | every repo in the org |
| Build & run | `go build ./...`, `go run .`, `.env` + environment | every game server |
| Shared secret | `NEXTENDO_SECRET` / `NEXTENDO_SECRET_FILE`, read byte-for-byte the same way (`utility.go`) | `loadNextendoSecret` in each server |
| Identity | `GET /api/nsa?id=<decimal>` with a positive cache, a **negative** cache (60 s), a 16-call in-flight cap, and fail-**closed** on unreachable (`gates.go`) | `splatoon-2/gates.go` |
| Online gates | `POST /internal/online-check`, fail-**open** on a transport error | `splatoon-2/gates.go` |
| Token revocation | `revoked.go` + `NEXTENDO_REVOKED_TOKENS[_FILE]` | every server after audit patches `10`–`18` |
| Presence | `POST /internal/presence-batch` with `{appId,status,pids}` every 30 s | `splatoon-2/presence.go` |
| Monitoring | `/api/stats` with the same JSON keys, gated by `DASH_TOKEN` with a constant-time compare; `/healthz` | `splatoon-2/dashboard.go` |
| Port convention | `DASH_PORT`, default `:8088` | 8082 mk8 · 8083 s2 · 8084 ssbu · 8085 dash · 8086 acnh · 8087 mc |
| TLS | `CERT_FILE` / `KEY_FILE` | every server |
| Licence | PolyForm Shield 1.0.0 | every repo |

The `nnex` claim a console presents inside its BAAS `id_token` **is** an `nx2.` token, verified with
the same HMAC construction as `LoginEx` uses (`nex:` + `pid.username.expiry`). That is why this
server participates in the same revocation list: without `revoked.go` a credential revoked across the
fleet would still be accepted here.

## 3. What you have to change elsewhere

Three one-line changes, and one patch:

**`sni-router`** — required, or the console never reaches this server. Apply
[`audit/patches/08-sni-router-npln-route.patch`](../audit/patches/08-sni-router-npln-route.patch)
(after `03`), then set:

```
BACKEND_NPLN=<splatoon-3 host>:443
```

**`nextendo-dashboard`** — apply
[`contrib/nextendo-dashboard/0001-show-splatoon-3.patch`](../contrib/nextendo-dashboard/0001-show-splatoon-3.patch),
which adds the source entry, then set:

```
DASH_S3_URL=http://splatoon3:8088
```

**`nextendo-account`** — one line in `gameStatsURLs()` (`online_presence.go`), so the "one place at a
time" gate sees a Splatoon 3 player as playing:

```go
env("DASH_S3_URL", "http://splatoon3:8088"),
```

Presence itself already works without it, because this server pushes to
`/internal/presence-batch`.

**Prelude / DNS** — nothing to do, as long as this server sits behind the same IP as the rest of the
stack: the hosts file Prelude writes already covers the NPLN host with its `*srv.nintendo.net`
wildcard. Details, and what to do if it is on its own IP, in
[SETUP-HARDWARE.md](SETUP-HARDWARE.md).

**`nextendo-nncs`** — explicitly *not* used by this title. Do not add a NAT-check host for it; give it
STUN/TURN instead.

## 4. Repository layout

```
main.go            the command, like every other game server
gates.go           identity + the online gates      (same contract as splatoon-2/gates.go)
revoked.go         the nx2 denylist                 (same as the fleet, + config loading)
utility.go         envOr / envOrInt / loadNextendoSecret
npln/              the NPLN protocol stack — this title's nextendo-nex
  identity/        id_token -> Nextendo account, fail-closed
  services/        auth, friends, matchmaking, messaging, toyohr (schedule/fest/records), ugc
  server/          gRPC plumbing, interceptors, tenant metadata
  presence/        the presence hub and its reporter
  ...
gen/               generated protobuf bindings (committed, so no protoc is needed to build)
protocol/          the .proto files they come from
cmd/npln-smoke/    an on-the-wire smoke test: a real gRPC login against a running server
```

The protocol stack is under `npln/` rather than in one flat package because it is 12 000 lines of
title-specific protocol — the NEX servers keep the equivalent in `nextendo-nex` and stay flat as a
result. Splitting `npln/` into its own module is a mechanical change if a second NPLN title (Mario
Kart World, Pokémon S/V, …) ever wants it; the import path is the only thing that moves.

## 5. Verifying an integration

```sh
go build ./... && go test ./...                     # 71 tests, all offline
curl "http://127.0.0.1:8088/api/stats?key=$DASH_TOKEN"
go run ./cmd/npln-smoke -addr 127.0.0.1:50051       # a real gRPC login on the wire
```

Then a console: the log should reach `IssuePrearrangedUserToken pid=<a real PID>`. The full sequence,
and a symptom-to-cause table, are in [SETUP-HARDWARE.md](SETUP-HARDWARE.md).
