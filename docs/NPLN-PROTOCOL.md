# The NPLN protocol, as this server implements it

Everything here is either from [kinnay's NPLN documentation](https://github.com/kinnay/NintendoClients/wiki/NPLN-Servers)
and the [decompiled protobuf definitions](https://github.com/kinnay/NPLN-Protocols), or a decision this
server makes where the protocol leaves it open. The second kind is always called out.

## Endpoint and tenant

```
https://<tenant id>.lp1.t.npln.srv.nintendo.net      tenant id = t-<server id>-lp1
Splatoon 3:  t-dce9377b-lp1
```

One NPLN *tenant* per title. Users and most resources are scoped to a tenant; *accounts* are shared
across tenants (an account is the console user, a user is that account's identity inside one game).

DNS-MITM points the tenant host at this server. It is plain gRPC over HTTP/2 with TLS, so it can share
`:443` with the NEX auth endpoints through `sni-router`.

## Metadata

Every request carries gRPC metadata:

| Field | Required | Value |
| --- | --- | --- |
| `npln-tenant-id` | always | `t-dce9377b-lp1` |
| `authorization` | when authenticated | `bearer <access token>` |
| `uid` | when authenticated | the NPLN user id the token was issued for |

Retail NPLN answers **`Unimplemented` (12)** when the metadata is invalid — not `Unauthenticated`,
which is surprising but documented, and this server does the same so a misconfigured client behaves
identically against both. `uid` is verified against the token; a mismatch is `NOT_FOUND_USER_MISMATCH`.

## Tokens

`Auth` hands out an access token (a JWT) and a single-use refresh token. The documented retail shape,
which this server reproduces:

```
header  {"alg":"ES256","jku":"jwkSets/nplnAccessToken","kid":"<uuid>"}
claims  {"exp","iat","iss","sub":"<user id>",
         "npln":{"aid":"<account id>","app_id":"<title id>","tid":"<tenant id>",
                 "ext_id":"<nsa id>","ext_id_type":1,
                 "authorization":{"allow":["**"],"deny":[],"nso_restricted":false}}}
```

**Nextendo additions** (extra claims, ignored by anything that does not know them): `nx_pid` (the
Nextendo account), `nx_anon`, `nx_idx` (the prearranged slot). They save every service a round-trip to
the account server, and they are inside a signed token, so they are as trustworthy as the rest.

Three token kinds, all ES256 with the same key:

| Token | Purpose | Published verifier |
| --- | --- | --- |
| access | authenticates every RPC | — |
| matchmaking id | peers prove who they are to each other inside a match | `GameSessionService.IssuePublicKey` |
| user delegation | one console acting for a second local player | verified here |

ES256 (rather than an HMAC) matters for two reasons: it is what retail uses, and `IssuePublicKey` is
expected to publish a *public* key — with a symmetric key there would be nothing safe to publish. The
signature is the JWS 64-byte `R‖S` pair, **not** the ASN.1 encoding `crypto/ecdsa` produces by
default; a spec-following verifier rejects the latter.

## Resource names

NPLN addresses everything by a path-like name that is unique even across tenants. The ones this server
mints:

```
accounts/<a-…>
tenants/<t-…>
tenants/<t-…>/users/<u-…>
tenants/<t-…>/users/<u-…>/presence
tenants/<t-…>/users/<u-…>/friendUsers/<u-…>
tenants/<t-…>/userExternalIds/<nsa id>
tenants/<t-…>/gameSessions/<gs-…>
tenants/<t-…>/gameSessions/<gs-…>/userSessions/<us-…>
tenants/<t-…>/gameSessionShortAliases/<CODE>
tenants/<t-…>/matchmakingTickets/<mt-…>
tenants/<t-…>/matchmakingConfigs/<name>
tenants/<t-…>/iceServerSets/<name>
tenants/<t-…>/saveRecords/<u-…>
tenants/<t-…>/documents/<collection>/<id>
tenants/<t-…>/targets/<target>/vsSchedules/<id>
tenants/<t-…>/targets/<target>/coopSchedules/<id>
tenants/<t-…>/seasonSchedules/<id>
tenants/<t-…>/fests/<id>, /festSchedules/<id>, /festResults/<id>
```

The server always sends the **full** name; the client may send `tenants/current`. Every parser here
accepts `current` and **rejects another tenant's name** — resource names are globally unique on
purpose, and serving another tenant's resource would be a cross-game data leak.

## Errors

An NPLN error is a gRPC status **plus** an `nn.npln.errdetails.NError` detail carrying a trace id and a
fine-grained code. Some of those codes change the game's behaviour, which is why this server never
returns a bare status:

| Situation | Status | Detail code | Why it matters |
| --- | --- | --- | --- |
| bad/absent token | `Unauthenticated` | `TOKEN_INVALID` | the client re-authenticates |
| expired token | `Unauthenticated` | `TOKEN_EXPIRED` | the client **refreshes** instead of erroring |
| console not linked to an account | `Unauthenticated` | `INVALID_ACCOUNT` | the player is told to link, not shown a network error |
| room is full | `FailedPrecondition` | `GAME_SESSION_IS_FULL` | the client looks for **another room** |
| wrong room password | `PermissionDenied` | `GAME_SESSION_WRONG_PASSWORD` | "wrong code", not "communication error" |
| room was reaped | `Aborted` | `GAME_SESSION_EXPIRED` | the client leaves the lobby instead of waiting |
| maintenance window | `Unavailable` | `UNDER_MAINTENANCE` | the game shows its own maintenance screen |
| method not implemented | `Unimplemented` | `UNIMPLEMENTED_GENERIC` | the operation aborts cleanly, and we log it |

## Streams

NPLN leans on long-lived streams, and they are where a naive implementation quietly fails: the client
treats a stream that goes quiet as dead and stops using that feature, with nothing in the log.

| Stream | Direction | This server sends |
| --- | --- | --- |
| `Friends.SubscribeFriendUsers` | server | full snapshot, then on change, plus a forced snapshot every 60 s |
| `PresenceService.SubscribePresences` | server | snapshot → `PresenceEnumerationDone` → updates + heartbeats |
| `PresenceService.KeepAlive` | bidi | `Heartbeat` immediately, then every 30 s; receives the player's presence |
| `Messaging.RecvMessage` | server | `KeepAlive` immediately, then messages/acks, plus periodic keep-alives |
| `LobbyMessaging.RecvMessage` | server | same, scoped to one lobby |
| `Matchmaker.TrackMatchmakingTicket` | server | current state, then each transition, ends on a terminal state |
| `GameSessionService.TrackGameSessionCreationTicket` | server | the (already resolved) ticket |
| `MaintenanceScheduleService.Subscribe…` | server | the window if any, then keep-alives |

The gRPC keepalive policy is loosened accordingly (`MinTime` 10 s, `PermitWithoutStream`), because the
default would kill idle streams and complain about the console's pings.

## Diagnostics

Two switches exist for bring-up, and both are how you turn "the game shows an error" into a fix:

- `NPLN_LOG_BODIES=1` logs every request and response as protojson.
- `NPLN_LOG_UNKNOWN=1` (default) logs the full method path **and a hexdump of the payload** for any RPC
  this server does not implement, then answers `Unimplemented`.

```
[npln] UNHANDLED /nn.npln.toyohr.v1.Locker/SomeNewMethod from 10.0.0.5:52344 payload=37 bytes
00000000  0a 22 74 65 6e 61 6e 74  73 2f 74 2d 64 63 65 39  |."tenants/t-dce9|
…
```

Reading those lines in order is the entire bring-up loop.
