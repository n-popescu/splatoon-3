# Connecting a real Switch to this server

This is the end-to-end procedure for a **retail Switch running Atmosphère + Prelude** — not the
emulator. It is deliberately explicit about the places where hardware behaves differently from the
emulator, because those are where every attempt so far has stalled.

> You must supply your own legally-dumped game, keys and system files. This project ships none of
> Nintendo's code, keys or data. Use it on hardware you own.

**Read this first — the two things that are different on hardware:**

1. **A retail console cannot produce the `nnex` claim.** That claim is the cryptographic proof of
   *which* Nextendo account a console is signed into, and the emulator fork injects it. On hardware
   the server has to resolve identity from the BAAS id alone, so
   **`NEXTENDO_REQUIRE_SIGNED_TOKEN` must stay `0`** (its default). Setting it to `1` — which
   [DEPLOYMENT.md](DEPLOYMENT.md) recommends for emulator-only deployments — rejects every console
   with `Unauthenticated`.
2. **The friend-code bug is not fixed by this server.** This server resolves whichever account the
   console's `id_token` points at. If `nx-account` still hands every console the same account (see
   [Part E](#part-e--the-friend-code-bug-on-hardware)), Splatoon 3 will show that same account too.
   That is a faithful reflection of an upstream defect, not a defect here.

Sections: [A — server](#part-a--the-server) · [B — the `:443` edge](#part-b--the-443-edge) ·
[C — the console](#part-c--the-console) · [D — verify](#part-d--verify-in-order) ·
[E — friends](#part-e--the-friend-code-bug-on-hardware) ·
[F — troubleshooting](#part-f--troubleshooting-by-symptom)

---

## Part A — the server

### A1. Build

```sh
git clone https://github.com/NextendoNetwork/splatoon-3 && cd splatoon-3
go build ./... && go test ./...          # 68 tests, all offline
go build -o splatoon-3 ./cmd/splatoon-3
```

### A2. Configuration

```sh
cp example.env .env
mkdir -p data
cp schedule.example.json schedule.json           # then put real rotation data in it
cp matchmaking.example.json data/matchmaking.json
```

Both `cp` steps are mandatory, not optional: without `schedule.json` the lobby has no rotation to
show, and without `data/matchmaking.json` every matchmaking config falls back to the 2–8 player
default.

The values that must line up with the rest of the stack:

| Variable | Value | Why it matters on hardware |
| --- | --- | --- |
| `NEXTENDO_SECRET` | **byte-identical** to `nextendo-account`'s | It derives the BAAS id ⇄ account mapping *and* the NPLN user ids. A mismatch never errors — it silently gives the console an identity the account server has never heard of, so login fails with `no Nextendo account for this console` and friend lists come up empty |
| `NEXTENDO_INTERNAL_KEY` | the account server's internal key | Every `/internal/*` call is rejected without it |
| `NEXTENDO_ACCOUNT_URL` | e.g. `http://127.0.0.1:8080` | Private network only |
| `DASH_TOKEN` | anything long and random | Guards `/api/stats`, which you will use in Part D |

If the account server generated its own secret rather than being given one, it wrote it to
`nextendo_secret.key` **hex-encoded**. Either point this server at the same file with
`NEXTENDO_SECRET_FILE=/path/to/nextendo_secret.key` (it hex-decodes, exactly like the account server
does) or set `NEXTENDO_SECRET` on **both** to the same raw string. Do not hex-decode by hand into
`NEXTENDO_SECRET` — that produces different bytes and the failure is silent.

Hardware-specific settings:

| Variable | Set to | Why |
| --- | --- | --- |
| `NEXTENDO_REQUIRE_SIGNED_TOKEN` | `0` (default — just do not set it) | A retail console has no `nnex` claim. `1` rejects every console |
| `NPLN_VERIFY_ID_TOKEN` | `0` (default) | Only turn this on once you have deployed the BAAS signing key and pointed `BAAS_SIGNING_KEY` at its public half. Until then the signature cannot verify and every login fails |
| `NEXTENDO_REQUIRE_ACCOUNT` | `1` (default) | Leave it on. Off means anonymous logins, which cannot have friends anyway |
| `NPLN_LOG_UNKNOWN` | `1` (default) | Logs every RPC the server does not implement, with a hexdump. This is your to-do list during bring-up |
| `NPLN_LOG_BODIES` | `1` **for the first session only** | Logs every request and response as JSON. Invaluable once, far too chatty to leave on |

### A3. NAT traversal — STUN and TURN

Splatoon 3 does **not** use `nextendo-nncs`. It uses ICE, so it needs a STUN server and, for players
behind symmetric NAT, a TURN relay. coturn is both:

```sh
turnserver -n --realm=nextendo --use-auth-secret --static-auth-secret="<TURN_SECRET>" \
           --no-cli --no-tls --no-dtls --min-port=49160 --max-port=49200
```

```
NPLN_STUN_HOST=<public IP or hostname>
NPLN_TURN_HOST=<public IP or hostname>
NPLN_TURN_SECRET=<TURN_SECRET>        # byte-identical to --static-auth-secret
```

The secret must match coturn's, because the server mints time-limited REST-API credentials with it
(`username = "<expiry>:<npln user id>"`, `password = base64(HMAC-SHA1(secret, username))`). Nothing
static is ever shipped to a console.

Use a hostname or IP the **console** can reach — not `127.0.0.1`, and not a private address unless the
console is on the same LAN. This is the single most common cause of "the lobby fills but the match
never starts": everything up to the ICE exchange works, then both consoles try to reach a candidate
address that does not exist for them.

### A4. Ports

| Port | Protocol | Faces | What |
| --- | --- | --- | --- |
| `443` | TCP | **the console** | gRPC/HTTP2 + TLS, via `sni-router` or directly |
| `3478` | **UDP** and TCP | **the console** | STUN/TURN |
| `49160–49200` | UDP | **the console** | TURN relay range (must match coturn's `--min-port`/`--max-port`) |
| `8087` | TCP | **private network only** | `/api/stats` and `/ugc/*` |

The UDP rules are the ones people forget. A firewall that only opens TCP gives you a game that logs
in, shows the lobby, and never starts a match.

### A5. TLS

Prelude installs Atmosphère `exefs_patches/disable_ca_verification`, which stops the console's `ssl`
sysmodule from validating the chain — so the certificate does **not** need to be signed by a CA the
console trusts, and a self-signed leaf is fine. Give it the tenant hostname as a SAN anyway, so that
`curl`, the smoke tool and any future non-patched client all work:

```sh
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
  -keyout key.pem -out cert.pem -subj "/CN=t-dce9377b-lp1.lp1.t.npln.srv.nintendo.net" \
  -addext "subjectAltName=DNS:t-dce9377b-lp1.lp1.t.npln.srv.nintendo.net"
```

Point `CERT_FILE` and `KEY_FILE` at them. If the rest of your deployment already has a CA, use it —
one fewer moving part.

Those patches are gated by firmware build id, so **if the console's firmware is newer than the newest
patch Prelude ships, cert trust silently does not apply** and every HTTPS call fails at the
handshake. That looks like a network error in-game with nothing at all in this server's log. Check
the console's firmware against the `.ips` filenames in Prelude's
`romfs/sd/atmosphere/exefs_patches/disable_ca_verification/`.

### A6. Run

```sh
./splatoon-3
```

The first line of the log states the tenant and whether TLS is on. Read it — a typo in
`NPLN_TENANT_ID` is otherwise invisible until the console is rejected.

---

## Part B — the `:443` edge

If this server owns `:443` on its own IP, skip to Part C.

If it shares `:443` with the NEX auth servers behind `sni-router`, **you need
[`audit/patches/08-sni-router-npln-route.patch`](../audit/patches/08-sni-router-npln-route.patch)**
(finding F19). Without it the router has no rule for `*.npln.srv.nintendo.net`, so the connection
falls through to `BACKEND_DEFAULT` — a NEX authentication server, which completes the TLS handshake
and then says nothing a gRPC client understands. The symptom is the worst kind: DNS resolves, the
port is open, TLS succeeds, the game fails, and **neither side logs anything**.

```sh
cd sni-router
git am /path/to/splatoon-3/audit/patches/03-sni-router-proxy-protocol-and-connection-hygiene.patch
git am /path/to/splatoon-3/audit/patches/08-sni-router-npln-route.patch
go build ./... && go test ./...
```

Then set, on the router:

```
BACKEND_NPLN=<splatoon-3 host>:443
```

Unset, the router behaves exactly as before, so the patch is safe to apply ahead of time.

---

## Part C — the console

### C1. Prelude, once

Run the Prelude `.nro`, choose **NEXTENDO**, let it reboot. That is the whole console-side setup: it
writes the DNS-MITM hosts file, enables `dns_mitm` in `system_settings.ini`, and installs the
certificate-trust patches.

### C2. Does the hosts file already cover NPLN? Yes, if you share the IP

Prelude writes `/atmosphere/hosts/sysmmc.txt` and `/atmosphere/hosts/emummc.txt` with, among others:

```
<NEXTENDO_IP>    *.srv.nintendo.net
<NEXTENDO_IP>    *srv.nintendo.net
```

`t-dce9377b-lp1.lp1.t.npln.srv.nintendo.net` matches the second pattern, and Atmosphère's rule is
*last matching line wins* — the only lines after it are the `nncs2` NAT-check host, the telemetry
null-routes and the conntest hosts, none of which match. **So a stock Prelude console already sends
Splatoon 3 to the main Nextendo IP.** If this server sits behind that IP via `sni-router` (Part B),
there is nothing to change on the console at all.

If the Splatoon 3 server is on a **different** IP, add a line for it *after* the wildcards:

```
<SPLATOON3_IP>   t-dce9377b-lp1.lp1.t.npln.srv.nintendo.net
```

Two warnings about doing that:

- **Prelude rewrites both files from scratch every time you re-enter Nextendo mode.** A hand-added
  line does not survive. For anything but a one-off test, add the host to
  `Prelude-Nro/source/nextendo_apply.c` (`nextendo_hosts_build`) and rebuild the `.nro`.
- Edit **both** `sysmmc.txt` and `emummc.txt`, or it works from one boot configuration and not the
  other — a confusing half-hour.

Sharing the IP and routing with `sni-router` is the better answer. It needs no console change and
survives Prelude updates.

### C3. Set the clock

**Console time must be correct** (System Settings → System → Date and Time → synchronise via
internet, or set it by hand). Every token in the chain carries an `exp`. A console whose clock is
badly off has its `id_token` rejected as expired, and the log line says the token expired rather than
anything about the clock.

### C4. Link the console to a Nextendo account

The account must exist and the console must be signed into it. This server resolves identity through
`GET /internal/resolve?baas=<sub>` on the account server; an unlinked console has no account behind
its BAAS id, and login fails with `no Nextendo account for this console`. Splatoon 3 is not where you
fix that — get the account layer working first (the Home Menu should show your Nextendo nickname and
friend code).

---

## Part D — verify, in order

Do these in sequence. Each one rules out everything before it, which is what makes hardware
debugging tractable.

**D1 — the server is alive and configured.**

```sh
curl "http://127.0.0.1:8087/api/stats?key=$DASH_TOKEN"
```

JSON with zero players. `401` means `DASH_TOKEN` does not match; an empty reply means the process is
not running.

**D2 — the protocol works locally, before involving a console.** Restart with `NPLN_DISABLE_TLS=1`
(second, temporary instance if you like) and run the on-the-wire smoke test:

```sh
NPLN_DISABLE_TLS=1 NPLN_LISTEN_ADDR=:50051 ./splatoon-3 &
go run ./cmd/npln-smoke -addr 127.0.0.1:50051
```

It performs a real gRPC login and exercises the services. An **unknown** NSA id must be refused —
that is the fail-closed behaviour the friends fix depends on. To test a real account end to end, pass
its signed token with `-nnex`.

**D3 — the console reaches TLS.** From a machine on the console's network:

```sh
curl -vk --resolve t-dce9377b-lp1.lp1.t.npln.srv.nintendo.net:443:<SERVER_IP> \
     https://t-dce9377b-lp1.lp1.t.npln.srv.nintendo.net/
```

You want a completed handshake. `curl` will then report an HTTP/2-ish error or `404` — that is fine,
it is not a gRPC client. What matters is that the certificate came from *this* server and not from a
NEX auth server (Part B).

**D4 — the console logs in.** Start Splatoon 3 and enter online play. The log should show, in this
order:

```
IssuePrearrangedUserToken   pid=<your PID>        <- identity resolved: the milestone
SubscribeMaintenanceSchedules
Schedule.Select*                                  <- the rotation the lobby draws
Friends.* / PresenceService.KeepAlive
GameSessionService.* / matchmaking
```

`pid=` with your real PID is the moment to celebrate: DNS, TLS, routing, the shared secret and the
account link are all correct.

**D5 — collect the gaps.** Two log patterns are the to-do list for anything still not working:

- `UNHANDLED /nn.npln.…` + hexdump — an RPC the game wants that is not implemented yet.
- an unknown matchmaking config id — add it to `data/matchmaking.json` with the right player counts
  (see [MATCHMAKING.md](MATCHMAKING.md)).

---

## Part E — the friend-code bug on hardware

Full analysis in [FRIENDS.md](FRIENDS.md). The short version, and what you can and cannot test today.

**Two causes.** (A) Nothing reported console presence, so friends looked permanently offline. (B)
Identity was resolved from `bs:did` rather than from the ids actually derived from the account's PID,
which fails for a console with a cached device account — and `nx-account` then fell back to a global
"last authenticated account", so **every console acted as, and displayed the friend code of, that one
account**.

**What is fixed and shipped as patches:**

```sh
cd nextendo-account
git am /path/to/splatoon-3/contrib/nextendo-account/0001-friends-console-identity-and-presence.patch
git am /path/to/splatoon-3/audit/patches/18-nextendo-account-revocation-loader.patch
go build ./... && go test ./...
```

and, so that friends in the NEX titles stop being invisible (finding F3):

```sh
# in each of mario-kart-8-deluxe, super-smash-bros-ultimate, super-mario-maker-2,
# mario-strikers, minecraft
git am /path/to/splatoon-3/audit/patches/1N-<repo>-presence….patch
```

**What is not fixed, and cannot be from here:** `nx-account` is not a public repository. Two changes
are needed in it, both small and both specified in [FRIENDS.md](FRIENDS.md):

1. resolve the caller with `GET /internal/whoami?baas=<sub>&bsdid=<bs:did>` per request, instead of
   looking the console up by `bs:did` alone;
2. **delete the global "last authenticated account" fallback.** An unresolvable console must be an
   error, never a default identity.

**Until (2) lands, the friend-code bug persists on hardware, including in Splatoon 3**, because
`nx-account` hands the console an `id_token` for the wrong account and this server correctly resolves
the account it was given.

### Testing the parts that are testable

With the account patches applied (`X-Internal-Key: $NEXTENDO_INTERNAL_KEY` on each call):

```sh
# fail-closed resolution: unknown ids MUST 404, never return a default account
curl -i -H "X-Internal-Key: $K" \
  "http://127.0.0.1:8080/internal/whoami?baas=deadbeefdeadbeef&bsdid=0000000000000000"

# the real ids of a linked console -> exactly one account
curl -s -H "X-Internal-Key: $K" \
  "http://127.0.0.1:8080/internal/whoami?baas=<sub>&bsdid=<bs:did>"

# what Splatoon 3 sees: NPLN identity + friends + presence + favourites/blocks
curl -s -H "X-Internal-Key: $K" \
  "http://127.0.0.1:8080/internal/npln-friends?pid=<PID>"
```

Recovery for a console already poisoned by the old fallback: `POST /internal/unbind` for those ids on
the owner's account, then sign in again from the console so a correct binding is recorded by
`POST /internal/bind`.

**The test that actually proves it — two accounts, two consoles.** One console is not enough: with a
single console the buggy fallback returns the right account by accident. Sign console 1 into account
A and console 2 into account B, then check that each shows **its own** friend code in the Home Menu
and that `/internal/whoami` returns a different PID for each. Then add each other and confirm that,
with one of them inside Splatoon 3, the other sees them **online and playing Splatoon 3** — that is
the presence half (cause A) working end to end.

---

## Part F — troubleshooting by symptom

| Symptom | Almost certainly |
| --- | --- |
| Nothing at all in this server's log; game reports a network error | Traffic is not arriving. Either the router has no `BACKEND_NPLN` (Part B) or cert trust did not apply because the firmware is newer than Prelude's patches (A5) |
| `Unauthenticated` / `no signed Nextendo token` | `NEXTENDO_REQUIRE_SIGNED_TOKEN=1` on a retail console. Set it to `0` |
| `no Nextendo account for this console` | The console's BAAS id resolves to no account: either it is not linked (C4), or `NEXTENDO_SECRET` differs from the account server's (A2) |
| `id token expired` | Console clock (C3) — or, genuinely, a stale token: reboot the console |
| `id token signature invalid` | `NPLN_VERIFY_ID_TOKEN=1` without a correct `BAAS_SIGNING_KEY`. Set it back to `0` |
| Login works, lobby is empty of rotation | `schedule.json` is missing or has no entry covering *now* (see [SCHEDULE.md](SCHEDULE.md)) |
| Lobby fills, match never starts | STUN/TURN: `NPLN_STUN_HOST` unreachable from the console, UDP 3478 closed, or the TURN relay range not forwarded (A3, A4) |
| Only one console can play at a time | `NEXTENDO_REQUIRE_ACCOUNT` plus the account server's "one place at a time" gate seeing both consoles as the *same* account — that is the `nx-account` bug in Part E, not a Splatoon 3 setting |
| Friends list empty or everyone offline | Presence patches not applied (F3), or the same identity collapse from Part E |
| Everyone shows the same friend code | Part E cause (B). Needs the two `nx-account` changes |
| `UNHANDLED /nn.npln.…` | An unimplemented RPC. Grab the path and the hexdump — that is exactly what is needed to add it |

---

## Related documents

| Document | For |
| --- | --- |
| [DEPLOYMENT.md](DEPLOYMENT.md) | full configuration reference, docker-compose, data directory |
| [FRIENDS.md](FRIENDS.md) | the friends/identity analysis and the `nx-account` change spec |
| [MATCHMAKING.md](MATCHMAKING.md) | matchmaking configs and player counts |
| [SCHEDULE.md](SCHEDULE.md) | the rotation file |
| [TESTING.md](TESTING.md) | the test suite and the smoke tool |
| [NPLN-PROTOCOL.md](NPLN-PROTOCOL.md) | how NPLN differs from NEX, and why none of the NEX tooling applies |
| [../audit/README.md](../audit/README.md) | the cross-repository findings, including F19 (the router route) |
