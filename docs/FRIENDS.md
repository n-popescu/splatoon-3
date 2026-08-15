# The friends system

Two problems were reported on real Switch hardware:

> **A.** A lot of people are friends but never appear online — as if the online status is never shared.
>
> **B.** When adding someone on the Switch, it adds them *as the person who wrote the code*: his friend
> code always appears. Reissuing a friend code does not help — it still adds people as him and not as
> the connected user.

This document is the analysis and the fix. It covers three code bases, so it says clearly which part is
in this repository, which is in a fork, and which needs a change in a component that is not public.

---

## The pieces involved

```
   ┌────────────┐   BAAS / nn::friends       ┌──────────────┐   /internal/*      ┌───────────────────┐
   │  Switch    │ ─────────────────────────> │  nx-account  │ ─────────────────> │ nextendo-account  │
   │ Home Menu  │  friend list, presence,    │ (NOT public) │  identity, friends │  the friend graph │
   └────────────┘  add by code               └──────────────┘  presence, resolve └───────────────────┘
                                                                                         ▲
   ┌────────────┐                                                                        │
   │ Splatoon 3 │ ─── NPLN Friends/Presence ──────────────────────────────────────────────┘
   └────────────┘     (this repository)
```

- **`nextendo-account`** owns the friend graph, the friend codes and the presence table. Public.
- **`nx-account`** serves the Switch's own account/BAAS/friends endpoints. **Not in the public org**, so
  it cannot be forked or fixed here — but the change it needs is small and specified below.
- **this server** is one more consumer of the same graph, for Splatoon 3.

---

## Problem A — friends never appear online

### Root cause A1: nothing reports a console's presence

Presence in `nextendo-account` is an in-memory table with a 90-second TTL
(`presence.go`), and it has exactly two writers:

| Writer | Covers |
| --- | --- |
| `POST /api/presence` (the Ryujinx fork) | an **emulator** user, mostly around private-battle hosting |
| `POST /internal/presence-batch` (a NEX game server) | a player **inside** Splatoon 2 / MK8 / … |

Nothing reports a **console that is simply on and online** — sitting in the Home Menu, in a game that
has no Nextendo server, or in a menu before matchmaking. And the Home Menu friend list is precisely
where players look at each other's status. So the normal state of a Switch friend is *offline*, which is
exactly the reported symptom.

The TTL makes it worse in a subtle way: presence expires after 90 s, so even a friend who *was* seen
turns offline a minute and a half later unless something keeps re-reporting them.

### Root cause A2: no way for the Switch-facing server to publish presence

`nx-account` *does* see the console's presence: the console pushes it, and `nx-account` reads friend
presence on every friend-list request. But `nextendo-account` offers it no way to write it:
`/api/presence` needs a *user bearer token* (a web session or an `nx2.` token), which a
server-to-server component does not have, and `/internal/presence-batch` is shaped for "these PIDs are
in this game".

### Root cause A3: presence has to survive being *read*

Even with a writer, a console that reports once and then idles goes offline after the TTL. The console,
however, keeps talking to `nx-account` (friend list polling, profile sync). Those requests are proof of
life and were not used as such.

### The fix

**In this repository (Splatoon 3):**

1. `internal/presence` reports every player currently in Splatoon 3 to
   `/internal/presence-batch` every 30 s, with the Splatoon 3 title id. A player in Splatoon 3 is now
   visible as "playing Splatoon 3" to every friend, on a Switch, on the website, and in the other games.
2. Presence is refreshed by *any* authenticated RPC (`Hub.Touch`), not just by the keepalive stream, so
   a player whose stream broke does not blink offline.
3. When a player leaves (their `KeepAlive` stream closes), they are published as **offline
   immediately** instead of waiting out the TTL.
4. `SubscribePresences` **merges** the account server's network-wide view with the local one, so a
   friend who is online *in another game* shows as ONLINE in Splatoon 3 too.
5. `FriendUser.Relationship` is filled with `presence_deliverable: true` and
   `presence_receivable: true`. If those are false, the client will not show a friend's presence and
   will not publish its own to them — a friend list that looks perfectly healthy while everybody is
   permanently offline. Nextendo friendships are mutual and carry no per-friend presence privacy
   setting, so both are true for an accepted friend.

**In the `nextendo-account` fork** (see `docs/HANDOFF.md` for the link):

6. New `POST /internal/presence` — a single-account presence write for a server-to-server caller
   (internal key, no user token):

   ```http
   POST /internal/presence
   X-Internal-Key: …
   {"pid": 1800000042, "status": 1, "app_id": "", "app_field": "", "ttl_seconds": 300}
   ```

   This is what `nx-account` calls when a console reports its presence.

7. **Liveness from console traffic**: `/internal/identity`, `/internal/pid-by-bsdid` and
   `/internal/whoami` now refresh a *console online* presence (status 1) for the resolved account, with
   a longer TTL (`NEXTENDO_CONSOLE_PRESENCE_TTL`, default 5 min). A console that polls its friend list
   is online by definition, and now says so — without `nx-account` changing a single line.
   A stronger, fresher presence (status 2, "playing") is never downgraded by this.

**In `nx-account` (not public — specification):**

8. On the console's presence update (`nn::friends` presence / the BAAS presence endpoint), call
   `POST /internal/presence` with the **resolved caller's** PID.
9. When building the console's friend list, map each friend's `presenceStatus` (already returned by
   `/internal/identity`'s `friends[]`) into the BAAS friend object's presence, and set the friend
   relationship so presence is deliverable/receivable in both directions.

---

## Problem B — every console adds friends as one person

This is the more serious of the two: it is an **identity** bug, not a display bug. A console that acts
as another account can send friend requests as them and shows their friend code to everybody.

### Root cause B1: identity is per-device, and the device identity is shared

The Switch authenticates with a BAAS **device account** and presents an `id_token` on every request:

```
sub      the BAAS/NSA user id     (per console user)
bs:did   the device account id    (per console)
```

`nx-account` resolves the caller from `bs:did` via `/internal/pid-by-bsdid`. But the console **caches
its device account in system save `8000000000000010`** and keeps presenting it forever. If that device
account was created while the project owner's account was linked — which is how the first console to
ever run the stack was set up — then every console that inherited or re-created that binding presents
*his* identity. Nothing the console does afterwards changes it, and **reissuing a friend code changes
the code, not the binding** — exactly what was observed.

### Root cause B2: `pid-by-bsdid` can only recognise derived ids

`nextendo-account` derives the Nintendo-shaped ids from the account PID:

```go
BaasID = HMAC(secret, "baas:"  + pid)[:8]
BsDid  = HMAC(secret, "bsdid:" + pid)[:8]
```

and `internalPIDByBsDid` walks every account comparing that derived value. That works only if the
console adopts the id **we** minted. A console that already holds a device account — created before the
account existed, or provisioned from another account — presents an id that matches nothing, and the
lookup 404s.

### Root cause B3: the fallback

The comment in `nextendo-account/main.go` states it plainly:

> *"Sans ça, nx-account retombait sur une variable globale (le dernier compte authentifié) et livrait le
> compte d'autrui."*
> (Without this, `nx-account` fell back to a global variable — the last authenticated account — and
> served somebody else's account.)

A 404 from `pid-by-bsdid` sends `nx-account` back to that fallback, and the fallback is a single global
account. Combined with B1/B2, every console converges onto one identity: whoever set the server up.

**This is the same class of bug this Splatoon 3 server is written to be immune to** — and why
`internal/identity` refuses rather than guesses, with a test whose only job is to prove an unresolvable
console does not become "some account".

### The fix

**In the `nextendo-account` fork:**

1. **A real binding table.** Instead of only recognising ids derived from the PID, the account server
   now stores the ids a console actually presents, per account:

   ```json
   "nintendo_bindings": [
     {"baas_id": "8ca8d7842f865b2f", "bs_did": "581ea786a91f1689", "na_id": "…",
      "bound_at": "2026-08-15T12:00:00Z", "label": "switch-lite"}
   ]
   ```

   Populated when the user signs into their Nextendo account on the console
   (`/internal/login`, and the new `/internal/bind`). Several bindings per account are allowed — one per
   console/profile — and **a binding may belong to only one account**: binding an id that is already
   bound elsewhere is refused with `409`, instead of silently moving it.

2. **`GET /internal/whoami`** — the single resolution entry point for the Switch-facing server:

   ```
   GET /internal/whoami?baas=<sub>&bsdid=<bs:did>            → {"pid": …, "via": "binding"|"derived"}
   404  no account owns this console identity
   ```

   Resolution order: explicit binding on `baas` → explicit binding on `bs_did` → derived `BaasID` →
   derived `BsDid`. It answers **404 and nothing else** when it cannot resolve. There is no default.

3. **`POST /internal/bind`** — records a binding for an account (used by the console link flow):

   ```json
   {"pid": 1800000042, "baas_id": "8ca8d7842f865b2f", "bs_did": "581ea786a91f1689", "label": "switch-1"}
   ```

4. **O(1) reverse indexes** for `baas_id`, `bs_did` and friend code, replacing the full scan (which was
   also a small denial-of-service surface on a public-facing lookup).

5. **A self-add guard**: a friend request where `from == to` is refused. With the identity bug in
   place, "add a friend" on the affected consoles was frequently a *self*-request, which is how the
   graph filled with nonsense.

6. **`from` is authoritative**: `/internal/friend-request` and `/internal/friend-accept` continue to
   take the PID, but the fork logs the resolution source, so a wrong caller is visible in one line
   instead of being invisible.

**In `nx-account` (not public — specification):**

7. Replace the `bs:did`-only lookup with **`GET /internal/whoami?baas=<sub>&bsdid=<bs:did>`**, taking
   both values **from the `id_token` of the request being served**.
8. **Delete the global fallback.** On 404, answer the console with an authentication error. A player
   seeing "please link your Nextendo account" is a fixable problem; a player silently acting as somebody
   else is not.
9. Cache resolutions **per (`sub`,`bs:did`) pair only**, never in a process-wide variable.
10. At link time, call `POST /internal/bind` with the ids from the token the console presented, so the
    console's *existing* device account becomes the recognised binding for that account. This is what
    makes a console that already has a cached device account work without a factory reset.

### Why this fixes the reported symptom

Once (7) + (10) are in place, the friend-add screen asks *who am I* with the ids from the request's own
token, gets the PID of the account bound to **that** console/profile, and the friend code it shows is
that account's. A console whose device account is not bound to anybody is told to link — it can no
longer borrow an identity. And because a binding is unique, two consoles cannot resolve to one account.

### If a console is already poisoned

The console has cached the owner's device account. After the server side is fixed, either:

- **bind it deliberately** — sign in on the console with the intended Nextendo account; the link flow
  calls `/internal/bind` with the ids the console presents, and from then on that console is that
  account (the previous owner of the binding is refused, so bind it away from the owner's account
  first); or
- **reset the console's binding** — delete the local user's online binding (the Linkalho-style
  `isNnLinked` / device-account state described in `Prelude-Nro`'s notes) and let it provision fresh.

Reissuing a friend code does *not* help, and now there is a reason why: the code was never the identity.

---

## What Splatoon 3 needed on top

The NPLN friend model is not the Home Menu one, and two details are easy to get wrong:

- **Identity must line up exactly.** The account server publishes a friend as
  `u-<base32(HMAC(secret,"npln-user:"+pid))>`. This server derives the logged-in player's id with the
  *same* function. Two independent derivations of the same thing are a trap, so
  `internal/identity/identity_test.go` re-implements the account server's version and asserts they are
  byte-identical. If somebody changes one, that test fails.
- **Streams must stay warm.** `SubscribeFriendUsers` re-reads the graph every 15 s and force-sends a
  snapshot every 60 s; `SubscribePresences` sends the enumeration-done marker the client waits for and
  then heartbeats. A stream that goes quiet is treated as dead by the client, and the friend list stops
  updating with nothing in the log to say why.

## Testing the friend path

1. Two accounts, friends on the website. On each console: enter Splatoon 3's online mode.
2. `[friends] ListFriendUsers pid=… -> N friend(s)` should show a non-zero N. Zero with a healthy
   account server means the derived user ids do not match — check that `NEXTENDO_SECRET` is identical.
3. `[presence] pid=… state=ONLINE attrs=…` on both, then each should see the other as online in game.
4. `curl "http://…:8087/api/stats?key=$DASH_TOKEN"` lists both players.
5. On the account server, `GET /internal/npln-friends?pid=<a>` should show `b` with a fresh presence
   (`status` 2) while `b` is playing.
6. Have one console leave online play: the other should see them go offline within seconds (the
   immediate publish), not after 90 s.

One console is not enough to prove anything here: with a single console the buggy fallback returns the
right account by accident. The full hardware procedure, including the `/internal/whoami` and
`/internal/bind` calls and how to recover a console already poisoned by the old fallback, is in
[SETUP-HARDWARE.md](SETUP-HARDWARE.md#part-e--the-friend-code-bug-on-hardware).
