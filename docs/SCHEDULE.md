# The schedule

Splatoon 3 will not let a player into an online mode whose rotation does not cover *now*. The
`Schedule` service is therefore not optional decoration: it is a precondition for the game working at
all.

## What the game asks for

| RPC | Answers |
| --- | --- |
| `SelectVsSchedules` | the versus rotation: regular battle, Anarchy (two concurrent rules), X battle |
| `SelectCoopSchedules` | the Salmon Run rotation, each with a shift id results are reported against |
| `SelectSeasonSchedules` | the current season window (ranked modes need one that contains *now*) |
| `SelectLeagueSchedules` | event battles, which run in bursts rather than continuously |
| `SelectVsParams` | a tuning parameter set (empty here: the game falls back to its built-in values) |

## Why it is a config file and not code

Everything the schedule *contains* — stage numbers, rule numbers, weapon ids, boss names, season
boundaries — is **game data**. It cannot be derived from the protocol, and Nintendo's server simply
serves numbers the game trusts blindly. Hardcoding a guess would produce a rotation that looks right in
the log and shows the wrong stages, or no stages, in the game.

So the rotation is `schedule.json` (`NPLN_SCHEDULE_FILE`), and the server ships a **placeholder** that
is structurally valid and content-wise a guess. The placeholder exists so a freshly cloned server
answers something coherent rather than an empty schedule (which reads as "online is broken"); it is not
meant to survive contact with a real client.

## Slots

Slots are aligned to the rotation length **since the Unix epoch**, exactly like retail's on-the-hour
rotations. Every client computes the same boundaries, and a server restart does not shift them:

```
rotation_minutes = 120  →  slots start at 00:00, 02:00, 04:00 … UTC
```

The entry served for a slot is picked by walking the configured list with the slot's global index, so
the rotation is deterministic: two consoles asking at the same moment get the same answer, and so does
the same console an hour later.

`etag` short-circuits the whole thing: if the client sends back the etag it already has, the response
carries the etag and **no entries**, and the console skips the download. The etag is derived from
`schedule_set_id` + rotation length — so **bump `schedule_set_id` whenever you change the content**, or
clients will keep their cached copy.

## The file

```json
{
  "schedule_set_id": "nextendo-1",
  "target": "default",
  "rotation_minutes": 120,
  "slot_count": 12,

  "regular": [
    { "stages": [1, 2] },
    { "stages": [3, 4] }
  ],

  "bankara": [
    { "modes": [ { "rule": 1, "stages": [5, 6] }, { "rule": 2, "stages": [7, 8] } ] },
    { "modes": [ { "rule": 3, "stages": [9, 10] }, { "rule": 4, "stages": [1, 3] } ] }
  ],

  "x": [
    { "rule": 1, "stages": [2, 4] }
  ],

  "league": [
    {
      "rule": 2,
      "stages": [5, 7],
      "slots": [ { "name": "window-1", "start_offset_min": 0, "length_min": 60 } ]
    }
  ],

  "coop": [
    {
      "stage": 1,
      "boss": "",
      "main_weapons": [0, 1, 2, 3],
      "kuma_weapon": 0,
      "reward_type": "",
      "reward_gear_id": 0
    }
  ],

  "season": { "id": "current", "start": "2026-06-01T00:00:00Z", "end": "2026-09-01T00:00:00Z" },

  "fest": null
}
```

| Field | Notes |
| --- | --- |
| `schedule_set_id` | groups the entries the client caches; **bump it when you change content** |
| `target` | the schedule "target" (region group) the client asks for; a private deployment has one |
| `rotation_minutes` | slot length; 120 matches retail |
| `slot_count` | how many future slots to serve at once |
| `regular` / `bankara` / `x` / `league` | versus rotation, walked per slot. `bankara` holds *two* rules per slot (series + open) |
| `coop` | Salmon Run rotation |
| `season` | omit and a rolling three-month window is served, so ranked modes stay open |
| `fest` | `null` for no splatfest — the normal state |

## Splatfests

```json
"fest": {
  "id": "fest-1",
  "regions": ["EU"],
  "teams": ["alpha", "bravo", "charlie"],
  "open_time":  "2026-08-20T00:00:00Z",
  "start_time": "2026-08-22T00:00:00Z",
  "mid_time":   "2026-08-23T00:00:00Z",
  "end_time":   "2026-08-24T00:00:00Z",
  "close_time": "2026-08-26T00:00:00Z",
  "game_data": "",
  "game_data_revision": ""
}
```

With a fest configured, `SelectFestSchedule` serves it, players can pick a team
(`CreateFestEntry`, one choice per fest — no switching sides), and `GetFestEntry` returns it.

Two things are deliberately *not* faked:

- **Results** (`GetFestResult`) are served with `is_valid: false` on every block. The protocol has that
  flag exactly so a server can say "not yet"; inventing ratios would show players a fabricated outcome.
- **Decryption keys** (`GetFestDecryptionKey`) are empty. They gate the fest content bundle; with no
  bundle there is nothing to unlock, and the call still answers so the fest flow does not abort.

Running a *real* fest (with power measurement and computed results) is out of scope for this server as
it stands — see [HANDOFF.md](HANDOFF.md).

## How to build a real schedule

1. Run with `NPLN_LOG_BODIES=1` and enter the game's mode-selection screens.
2. The requests show which target, which duration and which season the game expects.
3. The stage/rule numbers themselves come from the game's own data (its parameter tables, or a
   community reference). Put them in the file and bump `schedule_set_id`.
4. Restart, re-enter, and check that the mode opens and the stage names on screen match what you set.
