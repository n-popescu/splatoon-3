<h1 align="center">splatoon-3</h1>

<p align="center">
  <b>Nextendo Network game server for Splatoon 3.</b>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/license-PolyForm%20Shield%201.0.0-orange" alt="License: PolyForm Shield 1.0.0">
  <img src="https://img.shields.io/badge/go-1.23%2B-00ADD8" alt="Go 1.23+">
</p>

---

## What is this?

The game server for **Splatoon 3** on [Nextendo Network](https://nextendo.network). It handles
authentication, the friend list and presence, matchmaking and the peer-to-peer setup, the stage/mode
rotation, splatfests, cloud saves, player records, replays and the other title services the game
needs to bring its online mode up.

It plugs into the same stack as the NEX game servers and is configured the same way:

| It uses | For |
| --- | --- |
| `nextendo-account` | accounts, the one friend graph, presence, and the online gates |
| `sni-router` | shares `:443` with the other games' auth endpoints |
| `nextendo-dashboard` | polls `/api/stats` here exactly as it polls the NEX servers |
| the shared `NEXTENDO_SECRET` | verifies `nx2.` account tokens and derives player identity |

## Important: Splatoon 3 is not a NEX title

**It is not built on [`nextendo-nex`](https://github.com/NextendoNetwork/nextendo-nex), and it cannot
be.** Splatoon 3 does not speak NEX/PRUDP at all — it uses Nintendo's newer **NPLN** platform: gRPC
over HTTP/2 and TLS, protobuf payloads, one tenant per title. Concretely:

- Splatoon 3 has **no NEX game-server id and no NEX access key**. Every NEX title has both (Splatoon
  2 is `2DF33D01` / `4eb18d39`); Splatoon 3 appears only in the NPLN tenant list, as `dce9377b`.
- The console connects to `https://t-dce9377b-lp1.lp1.t.npln.srv.nintendo.net` and immediately speaks
  gRPC. A PRUDP listener there would complete a TLS handshake and then never receive a packet it
  understands.
- NAT traversal is **ICE (STUN/TURN)**, not the Pia NAT-check pair, so `nextendo-nncs` is not
  involved in this title.

So the wire protocol differs because the game dictates it. Everything an operator or a maintainer
touches is deliberately identical to the rest of the fleet: same environment variables, same shared
secret, same `/api/nsa` and `/internal/*` account contract, same `gates.go` / `presence.go` /
`revoked.go` behaviour, same `/api/stats` JSON, same `DASH_PORT` convention, same licence.

The NPLN protocol stack lives under [`npln/`](npln) and plays exactly the role `nextendo-nex` plays
for the NEX titles — it can be split into its own module the day a second NPLN title needs it.
[`docs/INTEGRATION.md`](docs/INTEGRATION.md) is the file to read before adopting this repository.

## Running

```sh
cp example.env .env    # then edit .env
go run .
```

Configuration is entirely through environment variables — see [`example.env`](example.env). No
secrets are baked into the source.

```sh
go build ./... && go test ./...    # 71 tests, all offline
```

## Ports

| Port | What |
| --- | --- |
| `:443` | gRPC/HTTP2 + TLS — the tenant endpoint (via `sni-router`, or directly) |
| `:8088` | `DASH_PORT` — `/api/stats`, `/healthz`, `/ugc/*` (private network only) |
| `3478/udp` | your STUN/TURN server (not this process) |

`:8088` because the fleet already uses 8082 (MK8), 8083 (S2), 8084 (SSBU), 8085 (the dashboard),
8086 (ACNH) and 8087 (Minecraft).

## Documentation

| Document | What is in it |
| --- | --- |
| [docs/INTEGRATION.md](docs/INTEGRATION.md) | **read first**: how this fits the fleet, what differs and why |
| [docs/SETUP-HARDWARE.md](docs/SETUP-HARDWARE.md) | connecting a real Switch, step by step |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | how the pieces fit, and what happens from boot to match |
| [docs/NPLN-PROTOCOL.md](docs/NPLN-PROTOCOL.md) | the protocol: metadata, tokens, resource names, errors |
| [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) | DNS, TLS, ICE, environment, compose |
| [docs/MATCHMAKING.md](docs/MATCHMAKING.md) | how players end up in the same room, and what to tune |
| [docs/SCHEDULE.md](docs/SCHEDULE.md) | the rotation file |
| [docs/FRIENDS.md](docs/FRIENDS.md) | the Switch friends system: root causes and fixes |
| [docs/TESTING.md](docs/TESTING.md) | bring-up and debugging |
| [audit/](audit) | the cross-repository audit done while building this |

## What this is not

This server ships **no** Nintendo code, keys, measured data, or copyrighted assets. It is an
independent reimplementation for use with a community-run replacement service, not affiliated with,
endorsed by, or associated with Nintendo. The NPLN tenant id it uses is a well-known per-title value
derivable from the game itself, not a secret.

## License

Released under the **[PolyForm Shield License 1.0.0](LICENSE.md)** — source-available: read, use,
modify, and self-host, but do not use it to provide a product that competes with Nextendo Network.
