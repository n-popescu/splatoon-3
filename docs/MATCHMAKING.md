# Matchmaking

## The model

A **game session** is a room: a host, a participant limit, an opaque property map the game fills in,
and a **user session** per player. Rooms live in memory and are reaped when their host stops syncing.

Two ways in:

| Flow | Used for | RPCs |
| --- | --- | --- |
| **Host creates** | private battles, room codes, LAN-style play | `CreateGameSessionCreationTicket` → `SyncGameSession` → (`CreateGameSessionShortAlias`) |
| **Matchmaker places** | turf war, Anarchy, X, Salmon Run | `CreateMatchmakingTicket` → `TrackMatchmakingTicket` |

## How the matchmaker decides

On every tick (1 s), per matchmaking config:

1. **Time out** tickets older than `NPLN_MATCH_TIMEOUT` → `TIMED_OUT`. The game then says "could not
   find players" instead of spinning forever.
2. **Backfill first.** A ticket naming a room in `backfill.game_session` is joined into that room —
   that is a lobby asking for a replacement, and it outranks any search.
3. **Place into an existing room.** Query for open, public rooms of the same config with enough
   vacancy, fullest first (filling a nearly-complete room beats spreading players across empty ones),
   and join.
4. **Group waiting tickets.** When the pool holds at least `min_players`:
   - if it reaches `max_players`, form the room immediately;
   - otherwise keep waiting until `NPLN_MATCH_WINDOW` has elapsed since the oldest ticket, then form
     the room with whoever is there.
5. The **oldest ticket hosts** the new room. Each placed player gets a matchmaking id token, which is
   how peers identify each other inside the match.

Resolved tickets are pushed to their `TrackMatchmakingTicket` stream, which the game is sitting on.

## `data/matchmaking.json`

Splatoon 3 asks for a **named** matchmaking config
(`tenants/…/matchmakingConfigs/<name>`) and the *server* decides what that name means — retail
configures this server-side, so the game never tells us how many players a mode needs. That is
configuration, not something to guess in code:

```json
[
  { "name": "regular",  "min_players": 8, "max_players": 8, "comment": "Turf War, 4v4" },
  { "name": "bankara",  "min_players": 8, "max_players": 8, "comment": "Anarchy" },
  { "name": "coop",     "min_players": 4, "max_players": 4, "comment": "Salmon Run" },
  { "name": "private",  "min_players": 1, "max_players": 10 }
]
```

Matching is on the **last path segment**, so `regular` matches
`tenants/t-dce9377b-lp1/matchmakingConfigs/regular`.

**You will not know the real names until you test.** An unknown config is logged once, with the
defaults it fell back to:

```
[mm] matchmaking config "tenants/…/matchmakingConfigs/vs_regular_jp" is not described in the
     config file; using min=2 max=8
```

Collect those lines from a play session and they *are* the file to write.

Defaults, when a config is unknown: `NPLN_MATCH_MIN_PLAYERS` (2) and `NPLN_MATCH_MAX_PLAYERS` (8). A
low minimum is deliberate for testing — two people can get into a match — and wrong for a real
deployment of a 4v4 game, where a match that starts with 3 players will not play.

## Who publishes the peer address

`GameSession` carries `host` and `port`. **This server never invents them.** The host publishes its
own reachable address through `SyncGameSession`, and joiners read it back off the session:

```
host                                  server                               joiner
  │  SyncGameSession{host,port} ────────>│                                     │
  │                                      │<──── GetGameSession / Join ─────────│
  │                                      │───── GameSession{host,port} ───────>│
  │<══════════════════ peer-to-peer ═════════════════════════════════════════>│
```

Only the host may change the room's shape (address, limit, joinability, properties); a joiner calling
`SyncGameSession` only refreshes its own liveness. There is a test for that — a joiner able to rewrite
the peer address could redirect everybody's traffic.

This mirrors how the NEX side of the stack handles station URLs: the server relays what the host
reported, it does not guess.

## NAT traversal

Splatoon 3 asks for an **ICE server set** (`AllocateIceServerSet`) rather than running the Pia NAT
check the NEX titles use. So:

- `nextendo-nncs` is **not** involved, and its two-distinct-IP requirement does not apply here;
- you need STUN (address discovery) and ideally TURN (relay for the NAT combinations that cannot
  hole-punch);
- with no STUN/TURN configured, `AllocateIceServerSet` **fails loudly** instead of returning an empty
  set — an empty set fails much later, inside the game's P2P layer, with nothing in the log.

`ListLatencyMeasurementServers` returns whatever `NPLN_LATENCY_SERVERS` describes
(`name@region=host:port/proto,…`). The client pings them and puts the result in its ticket. Nothing
here uses latency to group players yet — a single-region community server has nothing to optimise —
but the data arrives and is stored on the user session, so region-aware matching is a small change.

## Room codes

`CreateGameSessionShortAlias` (host only) mints a 6-character code from an alphabet with no `0/O` or
`1/I`, because these get read aloud from a friend's screen. `GetGameSessionShortAlias` resolves it. A
code dies with its room.

## Rooms and the reaper

`NPLN_SESSION_TTL` (2 min) bounds how long a room survives without a `SyncGameSession`. A crashed host
otherwise leaves a room that still looks joinable: players get matched into it, wait for a peer that no
longer exists, and see a communication error. The NEX servers in this stack learned that the hard way,
which is why the reaper is here from the start.

A host leaving removes the room (nothing migrates a peer-to-peer host); a joiner leaving frees its slot.

## What is deliberately not implemented

- **`Acceptance`** is echoed rather than enforced: Splatoon 3's flow does not require the accept step,
  but a title that asks and gets `Unimplemented` aborts the whole match, so it answers.
- **Skill / rank-based matching.** The game's rank data is in the room properties and the user
  attributes, opaque to us. Property *equality* filtering already works, so once you know which
  property holds the rank, restricting a query to a band is a few lines in `QueryFilter`.
- **Query cursors** (`page_token`) — the page size is honoured, but there is no cursor. Room lists are
  short enough that it has never mattered.
