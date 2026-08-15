# The `nextendo-account` friends patch

`0001-friends-console-identity-and-presence.patch` is the **server-side fix for the two Switch friends
problems**, as a ready-to-apply patch against
[`NextendoNetwork/nextendo-account`](https://github.com/NextendoNetwork/nextendo-account) (base commit
`ad321e6`, "Account country, and the table the BAAS server reads").

The analysis lives in [`../../docs/FRIENDS.md`](../../docs/FRIENDS.md); the patch adds its own
`FRIENDS-FIX.md` to the account server with the API reference and the `nx-account` specification.

## Why a patch and not a fork branch

The fork exists (`n-popescu/nextendo-account`), but the git credentials available to the agent that
wrote this were scoped to the `splatoon-3` repository, so the branch could not be pushed. A
`git format-patch` series is the lossless equivalent — and arguably more useful, because it applies to
whatever checkout you actually deploy from, including a private one.

## Applying it

```sh
git clone https://github.com/n-popescu/nextendo-account   # or your own fork / deployment checkout
cd nextendo-account
git checkout -b friends-fix
git am ../splatoon-3/contrib/nextendo-account/0001-friends-console-identity-and-presence.patch
go build ./... && go test ./...
```

If your checkout has moved on from `ad321e6`, `git am -3` (three-way merge) handles it; the only hunks
that touch an existing file are four small ones in `main.go` (route registration and three
`noteConsoleOnline` calls) plus the `favorite`/`blocked` fields in `npln_friends.go`.

## What it changes

| File | Change |
| --- | --- |
| `nintendo_bindings.go` *(new)* | `/internal/whoami`, `/internal/bind`, `/internal/unbind`, and `resolveConsole()` — per-request identity resolution that **fails closed** |
| `nintendo_bindings_store.go` *(new)* | the binding table: a sidecar JSON file with O(1) reverse indexes, so upstream's data model is untouched and the fork rebases cleanly |
| `console_presence.go` *(new)* | `/internal/presence` (single-account write) and `noteConsoleOnline()` — the missing presence writer |
| `nintendo_bindings_test.go` *(new)* | 12 tests, including the regression guard that an unknown console gets **404 with no `pid`** |
| `FRIENDS-FIX.md` *(new)* | full analysis, API reference, and what `nx-account` must do |
| `main.go` | route registration + three `noteConsoleOnline` calls + `pid-by-bsdid` now uses the binding index |
| `npln_friends.go` | publishes `favorite` per friend and a `blocked` array (Splatoon 3 reads both) |
| `README.md` | a section describing the fork |

Nothing existing changes shape: every addition is a new `/internal/*` route or a new JSON field.

## The part that is not in this patch

`nx-account` — the server that answers the Switch's own account/BAAS/friends endpoints — **is not in
the public organisation**, so it could not be changed. Its part is six small steps, listed in
`FRIENDS-FIX.md` and in [`../../docs/FRIENDS.md`](../../docs/FRIENDS.md). The two that actually kill
the bug:

1. resolve with `GET /internal/whoami?baas=<sub>&bsdid=<bs:did>`, using the ids from the `id_token` of
   the request being served;
2. **delete the global "last authenticated account" fallback** — on 404, return an authentication
   error to the console.

Until step 2 is done, consoles can still borrow an identity, because the fallback is on that side of
the wire.
