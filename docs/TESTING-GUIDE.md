# How to test everything — a guide for someone who doesn't write code

This guide assumes **no programming knowledge**. Every command is meant to be copied and pasted
exactly as written. After each one there is a **"what you should see"** section, so you always know
whether it worked.

You do not have to do all of it. The parts are ordered so that **each one is safe and useful on its
own**, and each builds confidence for the next:

| Part | What it proves | Needs |
| --- | --- | --- |
| [Part 1](#part-1--check-the-splatoon-3-server-on-your-own-computer) | The Splatoon 3 server is correct | just a computer, 15 min |
| [Part 2](#part-2--check-the-friends-fix) | The friends fix works | just a computer, 15 min |
| [Part 3](#part-3--apply-and-check-the-fixes-for-the-other-servers) | The other repos' fixes apply cleanly | just a computer, 20 min |
| [Part 4](#part-4--test-with-your-real-switch) | It actually works on a console | your Switch + the server online |
| [Part 5](#part-5--what-to-send-the-developer) | Handing it over | — |

There is a [glossary at the end](#glossary--what-all-these-words-mean). If a word looks like jargon,
it is explained there.

**Every command in this guide was run exactly as written before publishing it**, on a clean copy of
each repository, and the "what you should see" blocks are the real output — not an approximation. If
your output differs, that is worth reporting.

> **Nothing in Parts 1–3 touches your Switch, your server, or your live players.** Everything runs on
> your own computer, in temporary folders, and stops when you close it. You cannot break anything.

---

## Part 0 — Getting your computer ready

You need **three tools**. Most likely you already have two of them.

### Where do I type these commands?

- **macOS**: press `Cmd+Space`, type `terminal`, press Enter.
- **Linux**: open your terminal application.
- **Windows**: **please use WSL**, not PowerShell. Open the Start menu, type `powershell`, press Enter,
  and run this once:

  ```
  wsl --install -d Ubuntu
  ```

  Restart when it asks, then open **Ubuntu** from the Start menu. Every command in this guide will then
  work exactly as written. (PowerShell uses different syntax for setting variables and splitting lines,
  so half the commands below would need rewriting — WSL saves you that.)

That text window is where every command in this guide goes. Type it (or paste it), then press Enter.

**Pasting into a terminal:** `Ctrl+Shift+V` on Linux/WSL, `Cmd+V` on macOS. Right-click also usually
works.

### Check what you have

Copy and paste these three lines, one at a time:

```sh
git --version
go version
curl --version
```

**What you should see:** three version numbers, for example `git version 2.43.0`,
`go version go1.24.7`, `curl 8.5.0`.

**If one says "not found":**

| Missing | Install it from |
| --- | --- |
| `git` | <https://git-scm.com/downloads> |
| `go` | <https://go.dev/dl/> — **version 1.23 or newer** |
| `curl` | already on macOS/Linux/Windows 10+; otherwise <https://curl.se/download.html> |

After installing, **close the terminal window and open a new one**, then check again. (Installers only
affect windows opened afterwards — this trips up nearly everybody.)

### One-time note about `go`

The first time you build, Go downloads a few libraries from the internet. That is normal and takes
under a minute. If it fails with a network error, try again — and if it keeps failing, add this line
before the build command:

```sh
export GOFLAGS=-mod=mod
```

---

## Part 1 — Check the Splatoon 3 server on your own computer

**Time:** about 15 minutes. **Risk:** none — nothing leaves your computer.

### 1.1 Download the code

```sh
cd ~
git clone https://github.com/n-popescu/splatoon-3
cd splatoon-3
```

**What you should see:** a few lines ending in something like `Resolving deltas: 100% ... done.`
You are now "inside" the project folder — every later command in Part 1 assumes that.

> If you close the terminal and come back later, get back with: `cd ~/splatoon-3`

### 1.2 Build it

```sh
go build ./...
```

**What you should see: absolutely nothing.** No output at all. In this world, silence means success —
Go only speaks up when something is wrong.

**If you see errors** mentioning `cannot find module` or a download failure, it is a network problem;
try again, or run `export GOFLAGS=-mod=mod` first.

### 1.3 Run the automatic test suite

This is the single most valuable command in the guide. It runs **78 tests** that check the server's
logic — identity, matchmaking, the schedule, the token rules, the security fixes — without needing a
console, a network, or an account.

```sh
go test ./...
```

**What you should see:** about 40 lines. **Nine** of them start with `ok` — those are the nine parts
that have tests, and they must all say `ok`:

```
ok  	github.com/NextendoNetwork/splatoon-3	0.012s
ok  	github.com/NextendoNetwork/splatoon-3/npln/config	0.005s
ok  	github.com/NextendoNetwork/splatoon-3/npln/identity	0.005s
ok  	github.com/NextendoNetwork/splatoon-3/npln/names	0.004s
ok  	github.com/NextendoNetwork/splatoon-3/npln/presence	0.206s
ok  	github.com/NextendoNetwork/splatoon-3/npln/server	0.006s
ok  	github.com/NextendoNetwork/splatoon-3/npln/services/matchmaking	0.195s
ok  	github.com/NextendoNetwork/splatoon-3/npln/services/toyohr	0.006s
ok  	github.com/NextendoNetwork/splatoon-3/npln/token	0.005s
```

**All the other lines look like this, and they are completely normal:**

```
?   	github.com/NextendoNetwork/splatoon-3/gen/npln/auth/v1	[no test files]
?   	github.com/NextendoNetwork/splatoon-3/npln/services/auth	[no test files]
```

- `ok` = that part passed.
- `?   ... [no test files]` = nothing to test there (most of those are auto-generated code).
  **This is not an error and not a warning.** You will see roughly 30 of them.
- `FAIL` anywhere = something is genuinely wrong. Copy the whole output and send it to me.

Don't want to read 40 lines? This shows only what matters:

```sh
go test ./... 2>&1 | grep -v "no test files"
```

Nine `ok` lines and nothing else = perfect.

**What this proves:** the server's rules behave as designed. For example, one of those tests asserts
that a console presenting an unknown identity is *refused* rather than being handed somebody else's
account — the exact bug behind the friend-code problem.

### 1.4 Start the server

Now let's actually run it. This starts a real server on your own computer, in "local test" mode: no
encryption, no account server, no risk.

Copy this **whole block** at once:

```sh
mkdir -p /tmp/s3test
NPLN_DISABLE_TLS=1 \
NPLN_LISTEN_ADDR=127.0.0.1:50051 \
NEXTENDO_SECRET=dev-secret \
NEXTENDO_REQUIRE_ACCOUNT=0 \
DASH_TOKEN=test123 \
NPLN_DATA_DIR=/tmp/s3test \
go run .
```

**What you should see** — six lines, and then it stops and *stays* there:

```
[auth] 1 revoked nx2 token(s) (0 from configuration)
[npln] Splatoon 3 (NPLN) server starting: tenant=t-dce9377b-lp1 app=0100c2500fc20000
[mm] no matchmaking config file at /tmp/s3test/matchmaking.json; using min=2 max=8 for every config
[schedule] no schedule file at schedule.json; serving the built-in placeholder rotation
[dashboard] HTTP listening on :8088 (/api/stats, /ugc/*)
[npln] gRPC listening on 127.0.0.1:50051 (tls=false proxy-protocol=false)
```

**The window will look frozen. That is correct** — the server is running and waiting. Leave it alone.

Reading those lines:
- `1 revoked nx2 token(s)` — the leaked-password blocklist loaded. Good.
- `tenant=t-dce9377b-lp1` — it knows it is serving Splatoon 3.
- the two `no ... file` lines are **expected here**: we did not give it a rotation or match settings
  for this local test. On the real server you will provide them.
- the last two lines are the two doors the server opens.

### 1.5 Open a second terminal window

Leave the server running in the first window. **Open a new terminal window** (`Cmd+N` on macOS, or
just launch it again). Everything below goes in the *new* window.

### 1.6 Ask the server how it's doing

```sh
curl -s "http://127.0.0.1:8088/api/stats?key=test123"
```

**What you should see:** one long line of `{"game":"splatoon3", ...}`. That is already a pass.

To make it readable (optional — skip if `python3` isn't installed):

```sh
curl -s "http://127.0.0.1:8088/api/stats?key=test123" | python3 -m json.tool
```

```json
{
    "game": "splatoon3",
    "serverTime": "2026-08-15T17:39:16Z",
    "uptimeSeconds": 11,
    "connected": 0,
    "inLobby": 0,
    "activeLobbies": 0,
    "players": [],
    "gatherings": []
}
```

Zeros and empty lists are **exactly right** — nobody is playing. This is the same information the
Nextendo dashboard website reads.

### 1.7 Check the password on that page actually works

The stats page shows player names, account numbers and IP addresses, so it must not be public.

```sh
curl -s -i "http://127.0.0.1:8088/api/stats"
```

**What you should see:** the first line is

```
HTTP/1.1 403 Forbidden
```

**403 means "refused" — that is the correct, desired answer.** We deliberately left off the password.
If you ever see player data here *without* a password, that is a problem worth telling me about.

### 1.8 The health check

```sh
curl -s http://127.0.0.1:8088/healthz
```

**What you should see:** `ok`

### 1.9 The real test: pretend to be a console

This is the best check in Part 1. It uses a tool in the project that talks to the server **the same
way a Nintendo Switch does** — real network protocol, real messages — and verifies the answers.

```sh
cd ~/splatoon-3
go run ./cmd/npln-smoke -addr 127.0.0.1:50051
```

**What you should see:**

```
  ok   a request without npln-tenant-id is refused
  ok   anonymous token issued for tenants/t-dce9377b-lp1/users/u-anonymous
  ok   the anonymous user is refused by Friends
  ok   schedule: 12 contiguous slots, current one 16:00..18:00, regular stages [1 2]
  ok   a console that resolves to no Nextendo account is refused

all checks passed
```

That last line is what you want. In plain English, those five checks prove:

1. a request that doesn't say which game it is gets rejected;
2. the server can issue a login token;
3. an unidentified player can't read a friend list;
4. the stage rotation is continuous with no gaps (no "nothing to play" holes);
5. **a console belonging to no Nextendo account is refused** — it is *not* quietly given somebody
   else's account. This is the safeguard against the friend-code bug.

Meanwhile, look at the **first** window: you will see the server logging each request as it arrives.
That is what a real console session will look like.

### 1.10 Stop the server

Click on the first window and press **Ctrl+C** (hold Ctrl, press C). It prints a shutdown line and
exits. Then clean up:

```sh
rm -rf /tmp/s3test
```

### Part 1 result

If `go test ./...` was all `ok` and the smoke test said **all checks passed**, the Splatoon 3 server is
working correctly on your machine. That is genuinely the bulk of what can be verified without hardware.

---

## Part 2 — Check the friends fix

**Time:** about 15 minutes. **Risk:** none — it uses a throwaway copy with fake accounts.

This tests the fix for the two problems you reported: friends never showing online, and every console
adding people as the same person.

### 2.1 Download the account server (the fixed version)

```sh
cd ~
git clone https://github.com/n-popescu/nextendo-account
cd nextendo-account
git checkout claude/friends-identity-and-presence-3bd6df3185c98061980500a965e504ec
```

**What you should see:** `Switched to a new branch ...` or `branch set up to track ...`.

That long name is the branch holding the fix. If typing it is painful, paste it — or use tab
completion (type `git checkout claude/fri` and press Tab).

### 2.2 Apply the one remaining piece

One file's change ships as a patch (I explain why in `contrib/README.md`). Apply it:

```sh
git apply contrib/0001-wire-friends-fix-into-main.patch
```

**What you should see: nothing at all.** Silence = applied cleanly.

**If you see `error: patch failed`**, stop and tell me — it means the branch moved.

### 2.3 Build and test it

```sh
go build ./...
go test ./...
```

**What you should see:** silence from the first, and from the second:

```
ok  	nextendo-account	0.395s
```

To watch the friends-specific tests by name:

```sh
go test -run "Whoami|Binding|ConsolePresence|InternalPresence|NplnFriends|Unbind|TwoProfiles" -v . 2>&1 | grep -E "^(---|ok|FAIL)"
```

**What you should see:** thirteen `--- PASS` lines. The important ones, in plain English:

| Test | What it proves |
| --- | --- |
| `TestWhoamiFailsClosed` | an unknown console is **refused**, not given a default account |
| `TestTwoProfilesOnTwoConsoles` | two consoles get **two different friend codes** ← your bug |
| `TestBindingIsExclusive` | one console cannot be stolen by a second account |
| `TestConsolePresenceMakesAFriendOnline` | a console being on makes you appear **online** ← your other bug |
| `TestConsolePresenceDoesNotDowngradePlaying` | someone in a game keeps showing "playing", not just "online" |
| `TestInternalPresenceWrite` | quitting shows you offline **immediately**, not 90 seconds later |

### 2.4 See it with your own eyes

Tests are convincing, but let's watch it happen. Build the account server, then run it in a temporary
folder with throwaway settings — copy this whole block:

```sh
cd ~/nextendo-account
go build -o /tmp/acct .
mkdir -p /tmp/acctest && cd /tmp/acctest
NEXTENDO_SECRET=test-secret NEXTENDO_INTERNAL_KEY=test-key PORT=8099 /tmp/acct
```

**What you should see:** several start-up lines ending with something like
`Nextendo Network account service on http://localhost:8099`. Leave this window running.

**In a second window**, create two pretend players:

```sh
curl -s -X POST http://127.0.0.1:8099/api/register -H 'Content-Type: application/json' \
  -d '{"username":"alice","email":"alice@example.test","password":"Passw0rd!23"}'
echo
curl -s -X POST http://127.0.0.1:8099/api/register -H 'Content-Type: application/json' \
  -d '{"username":"bob","email":"bob@example.test","password":"Passw0rd!23"}'
```

**What you should see:** two long blocks of JSON starting with `{"account":{...`. Buried inside each
is the account number. To see just those:

```sh
grep -o '"pid":[0-9]*' /tmp/acctest/accounts.json | sort -u
```

**Expect:** `"pid":1800000001` (Alice) and `"pid":1800000002` (Bob). Those two numbers are what you use
in the commands below.

Now the interesting part. Pretend Alice has one Switch and Bob has another, and tell the server which
console belongs to whom:

```sh
curl -s -X POST http://127.0.0.1:8099/internal/bind -H "X-Internal-Key: test-key" \
  -d '{"pid":1800000001,"baas_id":"aaaa000000000001","bs_did":"aaaa000000000002","label":"alice-switch"}'
echo
curl -s -X POST http://127.0.0.1:8099/internal/bind -H "X-Internal-Key: test-key" \
  -d '{"pid":1800000002,"baas_id":"bbbb000000000001","bs_did":"bbbb000000000002","label":"bob-switch"}'
```

**What you should see:** `{"created":true,"ok":true,"pid":1800000001}` and the same for `1800000002`.

Now ask the server "who is this console?" for each one:

```sh
curl -s -H "X-Internal-Key: test-key" "http://127.0.0.1:8099/internal/whoami?baas=aaaa000000000001"
echo
curl -s -H "X-Internal-Key: test-key" "http://127.0.0.1:8099/internal/whoami?baas=bbbb000000000001"
```

**What you should see — and this is the whole point:**

```json
{"baasUserID":"6c45acecde5f51e1","found":true,"friendCode":"SW-7446-6695-9504","nickname":"alice","pid":1800000001,"via":"binding:baas"}
{"baasUserID":"8b54844d1604e1d1","found":true,"friendCode":"SW-8151-1741-6145","nickname":"bob","pid":1800000002,"via":"binding:baas"}
```

**Two consoles, two different friend codes.** Your friend codes will be different numbers from mine —
what matters is that **the two lines differ**. Before the fix, both consoles produced the *same*
account and the *same* friend code.

Three more checks worth doing:

```sh
# An unknown console must be REFUSED, not given somebody's account:
curl -s -i -H "X-Internal-Key: test-key" \
  "http://127.0.0.1:8099/internal/whoami?baas=deadbeefdeadbeef&bsdid=0000000000000000" | head -1
```
**Expect:** `HTTP/1.1 404 Not Found` — "I don't know this console." That refusal *is* the fix.

```sh
# Alice must not be able to claim Bob's console:
curl -s -i -X POST http://127.0.0.1:8099/internal/bind -H "X-Internal-Key: test-key" \
  -d '{"pid":1800000001,"baas_id":"bbbb000000000001"}' | head -1
```
**Expect:** `HTTP/1.1 409 Conflict` — refused, as it should be.

```sh
# Someone playing Splatoon 3, then quitting:
curl -s -X POST http://127.0.0.1:8099/internal/presence -H "X-Internal-Key: test-key" \
  -d '{"pid":1800000001,"status":2,"app_id":"0100c2500fc20000"}'
echo
curl -s -X POST http://127.0.0.1:8099/internal/presence -H "X-Internal-Key: test-key" \
  -d '{"pid":1800000001,"status":0}'
```
**Expect:** `{"ok":true}` twice. Now look at the **server's window**: you will see

```
[presence] pid=1800000001 status=2 app="0100c2500fc20000" (poussé par la console)
[presence] pid=1800000001 hors ligne (poussé par la console)
```

("poussé par la console" = pushed by the console; "hors ligne" = offline. The account server's logs
are in French.) That is a friend appearing as playing, then going offline **instantly** instead of
after a 90-second delay.

Stop it with **Ctrl+C** and clean up:

```sh
rm -rf /tmp/acctest /tmp/acct
```

### 2.5 The one thing that is still broken — and it is not this code

There is a third component called **`nx-account`** (the piece that talks directly to the Switch). It is
**not public**, so I could not change it. Until somebody makes two small changes there, **your consoles
will still show the same friend code in real life**, even with everything above installed.

The two changes are written out precisely in `FRIENDS-FIX.md` (section "What `nx-account` still has to
do"). In plain English:

1. for each request, *ask* the account server "who is this console?" (the `whoami` call you just tested);
2. **delete the emergency fallback** where, if it can't tell who the console is, it uses the last
   account that logged in. That fallback is the bug.

Everything you tested in Part 2 is the foundation those two changes need. Point the developer at that
section.

---

## Part 3 — Apply and check the fixes for the other servers

**Time:** about 20 minutes. **Risk:** none while you are only testing locally.

While building this I audited every Nextendo repository and found 19 problems, 13 with fixes ready.
They ship as **patch files** — a patch is a precise list of changes that a single command applies.

The full list with explanations is in `audit/README.md`. Here is how to apply and verify them.

### 3.1 The most important one: the router

**Without this, your console cannot reach the Splatoon 3 server at all.** The router that shares port
443 between all your games had no rule for Splatoon 3, so it sent it to a Mario-Kart-style server that
cannot understand it. The failure looks maddening: everything appears fine and the game just fails.

```sh
cd ~
git clone https://github.com/NextendoNetwork/sni-router
cd sni-router
git checkout -b fixes
git am ~/splatoon-3/audit/patches/03-sni-router-proxy-protocol-and-connection-hygiene.patch
git am ~/splatoon-3/audit/patches/08-sni-router-npln-route.patch
go build ./... && go test ./...
```

**What you should see:** `Applying: ...` for each patch, then silence from the build, then `ok`.

Then, on the machine that runs the router, add this setting:

```
BACKEND_NPLN=<the splatoon-3 server's address>:443
```

### 3.2 The one that makes friends visible in the other games

Five servers — including Mario Kart 8 Deluxe — never told the account server that anyone was playing.
So your friends looked offline even when they were mid-race.

```sh
cd ~
for repo in mario-kart-8-deluxe super-smash-bros-ultimate super-mario-maker-2 mario-strikers minecraft; do
  git clone https://github.com/NextendoNetwork/$repo
done
```

Then for each one, apply its patch (the numbers are in `audit/README.md`), for example:

```sh
cd ~/mario-kart-8-deluxe
git checkout -b fixes
git am ~/splatoon-3/audit/patches/11-mario-kart-8-deluxe-presence-and-revocation-loader.patch
go build ./... && go test ./...
```

**What you should see:** `Applying: ...`, silence, then `ok`.

### 3.3 The leaked password

A login token leaked in the 1.6.5 release. It had been blocked in 3 of your 9 components and **still
worked on the other 6**. Patches `10`, `16`, `17`, `18` and the presence patches close that everywhere,
and add a setting so the next leak is one config change instead of nine code edits.

### 3.4 The dashboard

To make Splatoon 3 appear on your monitoring page next to the other games:

```sh
cd ~
git clone https://github.com/NextendoNetwork/nextendo-dashboard
cd nextendo-dashboard
git checkout -b fixes
git am ~/splatoon-3/contrib/nextendo-dashboard/0001-show-splatoon-3.patch
```

Then set `DASH_S3_URL=http://<splatoon-3 host>:8088` where the dashboard runs.

### 3.5 A general rule for patches

- `Applying: <description>` = worked.
- `error: patch does not apply` = that repository has changed since I wrote the patch. Not dangerous;
  tell me and I will refresh it.
- To undo everything: `git checkout main` and delete the `fixes` branch. Patches only touch your local
  copy until someone pushes them.

---

## Part 4 — Test with your real Switch

**Time:** an evening. This is the part with real-world moving pieces.

The complete step-by-step is in **`docs/SETUP-HARDWARE.md`**, which covers the server settings, the
certificate, the STUN/TURN server, the firewall ports, the console side, and a symptom-to-cause table.
Rather than repeat it, here are the things most likely to catch you out, and the order to check them.

### 4.1 Before you start, the two settings that matter most

In your server's `.env` file:

```
NEXTENDO_REQUIRE_SIGNED_TOKEN=0
```

**A real Switch cannot provide the cryptographic proof this demands.** If it is `1`, every console is
rejected. (`1` is only correct for emulator-only setups.)

```
NEXTENDO_SECRET=<exactly the same value as your account server>
```

If this differs by even one character, **nothing tells you**. The server runs happily and every
friend list comes up empty. Copy and paste it; do not retype it.

### 4.2 The order to test in

Do these in sequence. Each one rules out everything before it, which is what stops you chasing the
wrong problem.

| # | Do this | Expected | If it fails |
| --- | --- | --- | --- |
| 1 | `curl "http://127.0.0.1:8088/api/stats?key=YOURTOKEN"` on the server | JSON, zero players | the server isn't running, or the token is wrong |
| 2 | The Part 1.9 smoke test, against the real server | `all checks passed` | a configuration problem, not a console problem |
| 3 | From another machine: `curl -vk --resolve t-dce9377b-lp1.lp1.t.npln.srv.nintendo.net:443:<SERVER_IP> https://t-dce9377b-lp1.lp1.t.npln.srv.nintendo.net/` | the encrypted connection completes | the router has no `BACKEND_NPLN` (Part 3.1), or the certificate |
| 4 | Start Splatoon 3, go online, watch the server log | a line saying `IssuePrearrangedUserToken pid=<your account number>` | see the table below |

**Step 4 is the moment of truth.** That one line means DNS, encryption, routing, the shared secret and
your account link are *all* correct simultaneously.

### 4.3 When something goes wrong

| What you see | Almost certainly |
| --- | --- |
| Nothing at all in the server log; the game says "network error" | traffic isn't arriving: router missing `BACKEND_NPLN`, or the certificate patches don't cover your console's firmware |
| `no signed Nextendo token` | `NEXTENDO_REQUIRE_SIGNED_TOKEN` is `1`. Set it to `0` |
| `no Nextendo account for this console` | the console isn't linked to an account, or `NEXTENDO_SECRET` doesn't match |
| `id token expired` | the console's clock is wrong. Set date and time via the internet |
| Login works, no stages/modes shown | `schedule.json` is missing or has no entry covering right now |
| The lobby fills but the match never starts | STUN/TURN: `NPLN_STUN_HOST` unreachable from the console, or **UDP** port 3478 closed. This is the most common one |
| Everyone shows the same friend code | the `nx-account` change from Part 2.5. Not fixable from here |
| `UNHANDLED /nn.npln.…` in the log | a feature the game wants that isn't built yet. **Send me those lines** — they are exactly what I need to add it |

### 4.4 To really test friends, you need two consoles

**One console proves nothing about the friend-code bug**, because with a single console the broken
fallback accidentally returns the right account. You need **two accounts on two consoles**: each should
show *its own* friend code, and with one of them inside Splatoon 3 the other should see them online and
playing.

---

## Part 5 — What to send the developer

When Parts 1–3 pass, you have something concrete to hand over. I would send, in this order:

1. **The repository**: <https://github.com/n-popescu/splatoon-3>
2. **`docs/INTEGRATION.md`** — the single most useful file for him. It explains why Splatoon 3 uses a
   different network protocol (with checkable evidence: Splatoon 3 has no NEX server id and no access
   key, unlike every NEX game), what is deliberately identical to his other servers, and the three
   one-line settings plus two patches needed elsewhere.
3. **`audit/README.md`** — the 19 findings across his repositories, 13 with ready patches. Several are
   independent of Splatoon 3 and worth his attention regardless: the leaked token still working on 6 of
   9 components, five servers never reporting presence, and a file-writing bug where **48% of reads
   under load saw a truncated file**, which breaks people joining each other's games.
4. **`docs/FRIENDS.md`** — the friends analysis and the two changes `nx-account` needs.
5. **Your own test results**: "`go test ./...` passes, the smoke test says all checks passed, and I saw
   `IssuePrearrangedUserToken pid=…` from my own console." That is the sentence that makes a maintainer
   take a contribution seriously.

Be upfront about the two honest gaps: the `nx-account` change is not done (it's not public), and the
matchmaking player counts and stage-rotation data are placeholders that need real game data —
`docs/HANDOFF.md` lists exactly which numbers and where they go.

---

## Glossary — what all these words mean

| Word | What it means |
| --- | --- |
| **terminal / shell** | the text window where you type commands |
| **repository (repo)** | one project's folder of code, stored on GitHub |
| **clone** | download a copy of a repository |
| **branch** | a parallel version of a repository, used for work in progress |
| **patch** | a precise list of changes, applied with `git am` or `git apply` |
| **build** | turn source code into a runnable program |
| **NEX** | Nintendo's *older* online system. Splatoon 2, Mario Kart 8 Deluxe and Smash use it |
| **PRUDP** | the network language NEX speaks |
| **NPLN** | Nintendo's *newer* online system. **Splatoon 3 uses this** — this is why the Splatoon 3 server is built differently, and it cannot be changed |
| **tenant** | NPLN's name for one game's slot. Splatoon 3's is `t-dce9377b-lp1` |
| **gRPC** | the network language NPLN speaks |
| **PID** | your Nextendo account number, e.g. `1800000001` |
| **NSA id / BAAS id** | the id a Switch presents for the user signed into it |
| **`bs:did`** | the id a Switch presents for the console itself |
| **`nx2.` token** | the signed proof of which Nextendo account a console is using |
| **presence** | whether you show as offline / online / playing a game |
| **friend code** | `SW-xxxx-xxxx-xxxx`. It is a *label*, not an identity — which is why reissuing it never fixed the bug |
| **STUN / TURN** | helpers that let two consoles behind home routers reach each other. Without them lobbies fill but matches never start |
| **ICE** | the method Splatoon 3 uses to pick a working connection between players |
| **sni-router** | the piece that shares port 443 between all your games |
| **`/api/stats`** | the page each server publishes so the dashboard can show who is playing |
| **403 / 404 / 409** | refused / not found / conflict. In this guide, all three are sometimes the *correct* answer |
| **`curl`** | a tool that fetches a web address from the terminal |
| **Ctrl+C** | stops a running program in the terminal |

---

## Quick reference card

```sh
# Part 1 — the Splatoon 3 server
cd ~/splatoon-3
go build ./...                                    # expect: silence
go test ./...                                     # expect: all "ok"
NPLN_DISABLE_TLS=1 NPLN_LISTEN_ADDR=127.0.0.1:50051 NEXTENDO_SECRET=dev-secret \
  NEXTENDO_REQUIRE_ACCOUNT=0 DASH_TOKEN=test123 NPLN_DATA_DIR=/tmp/s3test go run .   # Ctrl+C to stop
curl -s "http://127.0.0.1:8088/api/stats?key=test123"   # expect: JSON
curl -s http://127.0.0.1:8088/healthz             # expect: ok
go run ./cmd/npln-smoke -addr 127.0.0.1:50051     # expect: all checks passed

# Part 2 — the friends fix
cd ~/nextendo-account
git apply contrib/0001-wire-friends-fix-into-main.patch
go build ./... && go test ./...                   # expect: ok

# Part 3 — the other repos
git am ~/splatoon-3/audit/patches/<number>-<repo>-....patch
go build ./... && go test ./...                   # expect: ok
```

If any step gives you something other than what this guide says to expect, **copy the whole output**
and send it over. There is no such thing as too much detail in an error report.
