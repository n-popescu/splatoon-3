<h1 align="center">splatoon-3</h1>

<p align="center">
  <b>Nextendo Network online server for Splatoon 3 — an NPLN tenant server, written in Go.</b>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/license-PolyForm%20Shield%201.0.0-orange" alt="License: PolyForm Shield 1.0.0">
  <img src="https://img.shields.io/badge/go-1.23%2B-00ADD8" alt="Go 1.23+">
  <img src="https://img.shields.io/badge/protocol-NPLN%20(gRPC)-6f42c1" alt="Protocol: NPLN (gRPC)">
</p>

---

## Read this first: Splatoon 3 is not a NEX game

Every other game server in the Nextendo Network stack — Mario Kart 8 Deluxe, Splatoon 2, Smash,
Animal Crossing — speaks **NEX/PRUDP** and is built on
[`nextendo-nex`](https://github.com/NextendoNetwork/nextendo-nex).

**Splatoon 3 does not.** It talks to Nintendo's newer **NPLN** platform:

| | NEX titles (Splatoon 2 …) | Splatoon 3 (this server) |
| --- | --- | --- |
| Transport | PRUDP over a WebSocket, TLS | **gRPC over HTTP/2, TLS** |
| Encoding | RMC + NEX structures | **protobuf** |
| Login | Kerberos ticket from a TicketGranting auth server | **`Auth.IssuePrearrangedUserToken`** → a JWT access token |
| Matchmaking | `MatchmakeExtension` gatherings | **`GameSessionService` / `Matchmaker`** |
| Friends | the console's own friend list only | **`Friends` + `PresenceService` streams** |
| NAT traversal | Pia NAT-check (`nextendo-nncs`) + hole punch | **ICE: STUN, and TURN as a relay** |
| Endpoint | game auth host on `:443`, secure on its own port | **`t-dce9377b-lp1.lp1.t.npln.srv.nintendo.net`** |

So this repository is **not** a `nextendo-nex` game server, and no amount of work on the NEX core
would have produced it. It is a from-scratch NPLN tenant server that plugs into the *same* Nextendo
stack: the same accounts, the same friend graph, the same online gates, the same monitoring.

## What it does

A single Go binary serving the NPLN services Splatoon 3 uses:

**Platform services**

- `nn.npln.auth.v1.Auth`, `nn.npln.auth.v1.UserService` — login, tokens, user directory
- `nn.npln.friends.v1.Friends`, `nn.npln.friends.v1.PresenceService` — friends and live presence
- `nn.npln.matchmaking.v1.GameSessionService`, `nn.npln.matchmaking.v1.Matchmaker` — rooms, matchmaking, room codes, ICE
- `nn.npln.messaging.v1.Messaging` — invites and acknowledged messages
- `nn.npln.maintenance.v1.MaintenanceScheduleService` — maintenance windows
- `nn.npln.ugcstore.v1.Ugcstore`, `…Screening` — user content documents and attachments

**Splatoon 3 (`toyohr`) services**

- `Schedule` — stage/rule rotation, Salmon Run, seasons, event battles
- `FestService` — splatfests
- `CloudSave` — the online save record
- `GameRecord` — Anarchy / X / Salmon Run records
- `Replay`, `Locker`, `Canola`, `CoopScenario` — user content and the codes players share
- `LobbyMessaging` — lobby chat and signalling
- `UserScreening` — player reports

Plus the things that make it a *Nextendo* server:

- identities resolved against **`nextendo-account`** — one account, one friend list, everywhere;
- **presence reported both ways**, so a Splatoon 3 player shows as online to every friend, on a
  Switch, in another game or on the website;
- the same **online gates** as the NEX servers (verified account, not banned, one place at a time);
- **`/api/stats`** in the shape `nextendo-dashboard` already polls;
- every unimplemented method **logged with its payload**, so bringing the title up is a matter of
  reading the log rather than guessing.

## Status, honestly

This server is **written but not yet tested against the game**. It compiles, its logic is covered by
unit tests, and every design decision is documented — but nobody has yet put a console in front of
it. Expect to iterate on:

- which matchmaking configs Splatoon 3 asks for, and how many players each needs
  (`data/matchmaking.json`, see [docs/MATCHMAKING.md](docs/MATCHMAKING.md));
- the game's content data — stages, rules, weapons, seasons (`schedule.json`, see
  [docs/SCHEDULE.md](docs/SCHEDULE.md));
- the handful of RPCs whose response shape is undocumented (they answer an empty success and say so
  in the log).

[docs/TESTING.md](docs/TESTING.md) is the bring-up guide: what to run, what to look for in the log,
and how to turn an `UNHANDLED` line into an implementation.

## Quick start

```sh
git clone https://github.com/n-popescu/splatoon-3 && cd splatoon-3
cp example.env .env          # then edit it — nothing works until the secrets match your stack
go build ./cmd/splatoon-3
./splatoon-3
```

Everything is configured through the environment; no address, key or secret is baked into the source.
See [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) for DNS, TLS, STUN/TURN and a compose file.

**Connecting a real console?** [docs/SETUP-HARDWARE.md](docs/SETUP-HARDWARE.md) is the step-by-step
procedure for a retail Switch on Atmosphère + Prelude, including the two settings that differ from the
emulator and a symptom-to-cause table.

## Documentation

| Document | What is in it |
| --- | --- |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | how the pieces fit, and what happens from boot to match |
| [docs/NPLN-PROTOCOL.md](docs/NPLN-PROTOCOL.md) | the protocol itself: metadata, tokens, resource names, errors |
| [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) | running it: DNS, TLS, ICE, environment, compose |
| [docs/SETUP-HARDWARE.md](docs/SETUP-HARDWARE.md) | connecting a **real Switch**: server, router, console, verification, troubleshooting |
| [docs/MATCHMAKING.md](docs/MATCHMAKING.md) | how players end up in the same room, and what to tune |
| [docs/SCHEDULE.md](docs/SCHEDULE.md) | the rotation file |
| [docs/FRIENDS.md](docs/FRIENDS.md) | the Switch friends system: root causes and fixes |
| [docs/TESTING.md](docs/TESTING.md) | bring-up and debugging |
| [docs/HANDOFF.md](docs/HANDOFF.md) | state of the work, what is left, how to continue |

## Credits

- The NPLN protocol is publicly documented by
  [kinnay's NintendoClients wiki](https://github.com/kinnay/NintendoClients/wiki/NPLN-Servers), and the
  protobuf definitions under [`protocol/`](protocol) are the ones published in
  [kinnay/NPLN-Protocols](https://github.com/kinnay/NPLN-Protocols) (decompiled from public game data).
- The surrounding architecture — accounts, friends, presence, gates, monitoring — is
  [Nextendo Network](https://nextendo.network).

## What this is not

This project ships **no** Nintendo code, keys, assets or captured data. It is an independent
reimplementation of a publicly-documented protocol, for use with a community-run replacement service.
It is not affiliated with, endorsed by, or associated with Nintendo.

## License

Released under the **[PolyForm Shield License 1.0.0](LICENSE.md)** — source-available: read, use,
modify and self-host, but do not use it to provide a product that competes with Nextendo Network.
(Same license as the rest of the Nextendo Network stack.)
