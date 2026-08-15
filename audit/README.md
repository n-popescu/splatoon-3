# Nextendo Network — cross-repository audit

An audit of every repository in the [NextendoNetwork](https://github.com/orgs/NextendoNetwork/repositories)
organisation, done while building the Splatoon 3 server: **13 findings**, of which **8 are fixed** by
the patches in [`patches/`](patches) and 5 are documented with a recommendation.

Two of the fixes are for defects that were actively breaking gameplay and identity across the whole
fleet:

- the NAT-check responder rewrote the file the NEX core reads on the **join path** non-atomically, so
  **48 % of reads under load saw a truncated file** — a joiner then gets an unusable peer address and
  the console reports a communication error (measured, see F1);
- a **leaked login token** from the 1.6.5 release was revoked in 3 of 9 components and still worked on
  the other 6 (F2);
- **five game servers, including Mario Kart 8 Deluxe, never reported presence**, so their players were
  invisible to every friend everywhere (F3).

| | Document |
| --- | --- |
| **[FINDINGS.md](FINDINGS.md)** | every finding: what breaks, evidence, and the fix |
| **[patches/](patches)** | ready-to-apply `git am` patches, one per repository |

## Why this lives in the splatoon-3 repository

The agent that wrote it had push access to this repository only, so the fixes for the other
repositories ship as patches rather than as branches on each one. A patch series is also what an
operator can apply to the checkout they actually deploy from, including a private one. Move it wherever
it belongs — nothing here depends on the location.

## Severity summary

| # | Severity | Repository | Finding | Status |
| --- | --- | --- | --- | --- |
| F1 | **High** | `nextendo-nncs` | NAT files rewritten non-atomically on every probe; the NEX join path reads them | **fixed** |
| F2 | **High** | 6 game servers + account | A leaked `nx2.` token was revoked in 3 components of 9 | **fixed** |
| F3 | **High** | MK8D, SSBU, SMM2, Strikers, Minecraft | No presence reporting: players invisible to friends | **fixed** |
| F4 | **High** | `nx-scsi` | Cloud-save URLs unsigned and never expiring; no ownership check; unbounded upload | **fixed** |
| F5 | Medium | `nextendo-nex` | 16-bit sequence wraparound → permanent retransmit storm | **fixed** |
| F6 | Medium | `nextendo-nex` | `Connection.state` data race; `OnDisconnect` could run twice | **fixed** |
| F7 | Medium | `sni-router` | No PROXY protocol → the auth server sees the router's IP | **fixed** |
| F8 | Medium | `sni-router` | Accept-error spin loop; file-descriptor leak on half-open connections | **fixed** |
| F9 | Medium | `nextendo-account` | Default trusted subnet is an example CIDR; `/internal/*` may be LAN-reachable | documented |
| F10 | Low-Med | dashboards | `DASH_TOKEN` unset ⇒ `/api/stats` fully open (names, PIDs, IPs) | documented |
| F11 | Low-Med | all game servers | `NEXTENDO_REQUIRE_ACCOUNT` defaults **off** (anonymous logins) | documented |
| F12 | Low | `nx-dauth`, `mario-strikers` | Unbounded request bodies; non-atomic write of persistent state | documented |
| F13 | Low | tooling | A test that cannot pass in a clean checkout; `go vet` warning; two repos need a newer Go than the docs claim | documented |

## Applying the patches

Each patch is a `git format-patch` against the current `main` of its repository:

```sh
git clone https://github.com/NextendoNetwork/<repo> && cd <repo>
git checkout -b audit-fixes
git am /path/to/audit/patches/NN-<repo>-....patch    # -3 if your checkout has moved on
go build ./... && go test ./...
```

Order does not matter — each patch touches one repository and they are independent. The suggested
sequence by impact is F1 (`02`), F3 (`11`–`15`), F2 (`10`, `16`–`18`), F4 (`04`), then the core (`01`)
and the router (`03`).

| Patch | Repository |
| --- | --- |
| `01-nextendo-nex-ack-wraparound-and-state-race.patch` | `nextendo-nex` |
| `02-nextendo-nncs-atomic-writes-and-bounded-tables.patch` | `nextendo-nncs` |
| `03-sni-router-proxy-protocol-and-connection-hygiene.patch` | `sni-router` |
| `04-nx-scsi-signed-blob-urls-ownership-and-limits.patch` | `nx-scsi` |
| `10-splatoon-2-revoke-leaked-token.patch` | `splatoon-2` |
| `11-mario-kart-8-deluxe-presence-and-revocation-loader.patch` | `mario-kart-8-deluxe` |
| `12-super-smash-bros-ultimate-presence-and-revocation.patch` | `super-smash-bros-ultimate` |
| `13-super-mario-maker-2-presence-and-revocation.patch` | `super-mario-maker-2` |
| `14-mario-strikers-presence-and-revocation.patch` | `mario-strikers` |
| `15-minecraft-presence-and-revocation.patch` | `minecraft` |
| `16-animal-crossing-revoke-leaked-token.patch` | `animal-crossing-new-horizons` |
| `17-luigis-mansion-3-revocation-loader.patch` | `luigis-mansion-3` |
| `18-nextendo-account-revocation-loader.patch` | `nextendo-account` |

The `nextendo-account` friends/identity fix is a separate series in
[`../contrib/nextendo-account/`](../contrib/nextendo-account); it and patch `18` are independent.

## New configuration introduced by the patches

| Variable | Repository | Meaning |
| --- | --- | --- |
| `NEXTENDO_REVOKED_TOKENS` / `_FILE` | every game server + account | Revoke a leaked token by configuration instead of nine source edits |
| `SNI_SEND_PROXY_PROTOCOL=1` | `sni-router` | Send PROXY v1 to the backends (pair with `NEXTENDO_PROXY_PROTOCOL=1`) |
| `SCSI_URL_KEY` | `nx-scsi` | HMAC key for blob URLs (generated into the data dir if unset) |
| `SCSI_MAX_BLOB_BYTES` | `nx-scsi` | Upload cap, default 64 MiB |
| `SCSI_ALLOW_UNSIGNED_BLOBS=1` | `nx-scsi` | Temporary: keep accepting old, unsigned URLs while consoles migrate |

## How it was audited

- `go build`, `go vet` and `go test` on every Go module (14 of them), plus `go test -race` where tests
  exist. Findings were reproduced with tests **before** being fixed — F1's test fails on the original
  code with a precise number (239/500 truncated reads), and F5's does the same for the wraparound.
- Manual review of the paths where a defect is expensive: identity and login, anything touching a
  shared map from more than one goroutine, every unauthenticated listener, every file that one service
  writes and another reads, and every place a request body or an in-memory table can grow.
- A comparison across the eight game servers, which is where the drift lives: the same file copied
  eight times diverges, and F2 and F3 are both instances of that.
- The open GitHub issues on each repository, cross-checked against the code (see F14 in
  [FINDINGS.md](FINDINGS.md)).

## What was checked and is fine

Worth recording, so nobody re-audits it:

- **Path traversal in `nx-scsi`** is properly contained (`safeIDComponent` / `pathComp`) *and* has a
  test that asserts confinement. The ids come from a client, and the containment holds.
- **`nextendo-account` bounds every request body** it reads (`MaxBytesReader` in seven places, with the
  cloud-save cap set just above the largest legitimate quota) and serialises the quota check per
  account. That is the standard the smaller services should be held to.
- **The `/internal/*` guard reads `RemoteAddr`, never `X-Forwarded-For`**, with a constant-time key
  comparison — the right call, and explicitly commented as such.
- **No data race** was reported by `-race` across the existing suites of `nextendo-nex` and
  `nextendo-account`. The two races found (F6, F4) are on paths those suites do not exercise.
- **The identity resolution path is consistent** across all eight game servers: same nx2 proof, same
  NSA→PID resolution, same fail-closed handling of an unknown console.
