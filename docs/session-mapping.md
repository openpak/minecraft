# Session directory: one game on every console (card openpak/prds#177)

A player hosts on their own console; players on other consoles join that game. The OpenPak service is an invisible bridge, never a
visible host or player. A game hosted on ANY platform is advertised on EVERY other platform, under the hosting player's name, through
that platform's own discovery. This file is the contract: what the directory holds, what each platform adapter does, and, for every
field, where the mapping comes from. Proof tags: **log** (a real request of the owner's PS3, 2026-11-11 14:12Z, np-matching2 v0.2.21),
**capture** (the 2026-11-07 Wii U capture and docs/provenance.md), **static** (a retail binary, research/minecraft-lce notes), **code**
(a deployed server's behaviour that a real console already drove), **guess** (nobody has seen it on the wire or in a binary).

## 1. The directory (`internal/directory`)

One `Game` per player-hosted game: `ID` (`<platform>:<native ref>`), `Platform` (hosting), `Ref` (the platform-native session reference:
a Wii U gathering id, a PS3 matchmaking session id), `HostAcct` (the OpenPak account: every account already has an identity on every
network, so no table of who is who is kept), `Settings`, `Players`, `MaxPlayers`, `State`.

`Adapter` has a source half and a sink half:

| Call | Meaning |
| --- | --- |
| `Hosted` | every game hosted on this platform's real consoles now (bridged games left out); a full snapshot |
| `Advertise(game)` | create or update the advertisement of a game hosted elsewhere; idempotent, repeated by the directory every 30 s (`Renew`), which is what keeps it alive |
| `Withdraw(id)` | remove it |
| `Joins` | the join attempts, probe data and signalling that local consoles sent for advertised games since the last call |

`Directory.Sync` (every 5 s in `minecraft serve`) reads each source, advertises new and changed games on every other platform, withdraws the
ones that vanished, and collects the joins (`GET /internal/directory` on the service shows games and the last 200 joins; bearer
`MINECRAFT_INTERNAL_KEY`). A source that fails keeps its games as they were. A game without a host account is not advertised. A failed
`Withdraw` is not retried (the advertisement then ends by its lease: PS3 room 300 s, PS3 presence 90 s, Wii U presence 90 s; the Wii U
gathering is only unlisted by a successful withdraw).

### A new platform plugs in

Write one `Adapter` and add it to the list in `startDirectory` (`cmd/minecraft/serve.go`). Its `Hosted` reports its consoles' games with
the host's OpenPak account; its `Advertise` creates, in its network's own discovery, a game owned by the host's identity on that network
and keeps the host's friends-visible presence; its `Joins` reports who tried to join. The directory, the other adapters and the neutral
`Settings` do not change. A platform's codec maps `Settings` to its native blobs (section 3 is the pattern); a platform that carries
less than `Settings` simply drops the rest. The two services behind an adapter stay thin: they store and serve what the adapter hands
them (nex `bridged.go`, np-matching2 `agent/bridge.go`, np-community `xmpp/bridge.go`).

## 2. Neutral settings and state

| Neutral field | Wii U (application buffer, 212 bytes) | PS3 (session data, 184 bytes = attributes 0x55+0x56) |
| --- | --- | --- |
| Net version (not neutral; each side writes its own) | `0x0881` at +0, **capture** N1, **static** (`cmpwi 0x881`, rpx 0x03499ab0 and others) | `0x0884` at +0, **log** (first two bytes of 0x55), **static** (0xc86e78, 0xc87dec) |
| `HostName` | NUL-terminated at +0x17 (17 bytes) and again at +0x02 (9 bytes) | NUL-terminated at +0x14 (17 bytes) |
| `Visibility` friends / friends-of-friends | byte +0xc3: `0x8f` friends of friends, otherwise friends | low nibble of byte +0xa7: `0xf` friends of friends, otherwise friends |
| `Players` | byte +0xca | byte +0xae |
| joinable flag | byte +0xc9 = 1 (kept from the template) | byte +0xad = 1 (set) |
| `MaxPlayers` | gathering `max_participants` (nex) | Matching2 max slot = core session max members |
| `State` open / closed | `open_participation` of the matchmake session, `registered` of the gathering | a room is listed only while open with a free slot; a full or closed room drops out of `Hosted` and is withdrawn elsewhere |

Proof per field:

* **Net versions**: above. Both builds enforce strict equality, so each side is always given its own value.
* **Host name, PS3 +0x14**: **log** (`PS3Test` at 0x14..0x1a of 0x55, a lone host named PS3Test). The width 17 is a **guess** (the next non-zero byte of the capture is at +0x25; the 01.02 notes say 18 bytes at +0x02, a different layout).
* **Host name, Wii U +0x17 and +0x02**: **capture** (N1: the host row has the name at both places, `OpenPak` in our copy). The widths are a **guess**: the first copy is cut at 9 bytes because the capture has a used byte at +0x0b; a real host's longer name may overlap there, in which case this is wrong.
* **Visibility, Wii U**: **code** (`gameSpecificCanJoinMatchmakeSession` reads `ApplicationBuffer[0xc3] == 0x8F`, from the Pretendo title this fork is based on). The captured row has `0x8d`, so friends-only is `0x8d` (**capture**).
* **Visibility, PS3**: **guess**. The captured PS3 byte at +0xa7 is `0x9d`, the twin of the Wii U `0x8d` two bytes into the same `3c 8d` / `3c 9d` pair, at the same distance from the player count; `0xf` low nibble for friends of friends is carried over, nobody has seen a PS3 friends-of-friends game.
* **Players, PS3 +0xae**: **log** (value 1 for the lone host) and **static** (01.02 `ps3-static` §2.4: "u8 player count at +0xae, the popcount of the host's local mask"; 01.84 keeps the offsets). **Players, Wii U +0xca**: **guess** (the same structural position as the PS3 byte; the capture row holds 2). `Hosted` on the Wii U side does not use it: the count is the number of participants nex lists (**code**).
* **Joinable flag, PS3 +0xad**: **log** (1) and **static** (the 01.84 room-list filter at 0xc87dec drops a room whose +0xad is 0). Wii U +0xc9: **capture** (1, template).
* **Everything else in both blobs** (PS3 +0x00..0x13, 0x25.., +0xa6, +0xb0 `0x1f`, +0xb4; Wii U +0x0b..0x16, +0xc2, +0xcb, +0xcc `0x0f`) is copied from the real host's bytes (**log**, **capture**) and not translated. Not translated also: world type, difficulty, game mode shown in the Wii U "survival, easy" text (where they live is not mapped), texture pack, host skin.
* **Max players** PS3: **log** (`max_slot=8` in `matching2 room created`); 01.84 computes the slot count, so a host with another value is **static** only ([C] in ps3-0184-vs-wiiu §7). Wii U 8: **capture** N1.

## 3. PS3 native objects the sink writes (np-matching2, np-community)

| Object | Content | Proof |
| --- | --- | --- |
| Room (core session) | kind room, title `minecraft`, compat `lce-ps3-0884`, owner = the host's account (so the room shows the host's NP online id, resolved from the account core when read), max slot, `unique_key` = the game id, attribute `bridge` = the game id (searchable, so real-host listing skips it), no owner migration, ttl 300 s renewed by every PUT | rule from np-matching2 (compat, owner), **code** (core calls); `bridge` attribute and unique key are new |
| 0x55 + 0x56 | the 184-byte session data above, in two 92-byte attributes | **log** (92 + 92) |
| 0x57 + 0x58 | the 256-byte player table, two 128-byte attributes, the host as the only player: name in the first 17 bytes, the rest as the real host wrote it | **log** (128 + 128, name at +0). The four bytes at +0x13..0x16 (`1f a7 32 aa` in the log) are made up per host from its account (**guess**: their meaning is unknown) |
| 0x59 of the host | `00 00 00 01` (one local player) | **log** (`member_bin=0` on create; the 01.84 notes name the 4-byte local-player mask; the probe joins with `00000001`) |
| Host endpoint | none unless the transport leg supplies one (`endpoint` in the PUT) | n/a |
| Presence data blob (56 bytes) | name 18 @0, room id u64 @0x18, server id u32 @0x20 = 1, version u16 @0x28 = 0x0884, @0x2b = 0, @0x2c/@0x2d = the nibbles of session +0xb0, @0x30 = session +0xb4 | layout **static** (builder 0xc93cd4, friend filter 0xc96138 needs size 0x38); the value `@0x2b = 0` is the 01.02 rule (**static**, 01.02), 01.84 only says "joinable flag"; byte widths of the server id and the unknown fields are **guess**; no real blob has been seen |
| Presence XML around the blob | `<x xmlns="http://playstation.net/np/rt/presence-data" title="…">hex</x>` | **guess**. A real console's presence stanza was never captured. np-community v0.4.11 logs the first presence data of each account in full (`NP presence xml`); until then bridged presence may not reach the game even though the room is correct |

What a friend needs on the PS3 side: the host online (the account's core presence, from whatever console it plays on), the presence data (the
blob above) and the room (JoinRoom by the room id in the blob). A real PS3 signed in on the same account is not disturbed: the bridged
room has no room key and no console connection, the agent's latest-room and room-key lookups never see it, and bridged presence is
used only while the real console has stored none.

## 4. Wii U native objects the sink writes (nex minecraft-wiiu, nn-friends)

| Object | Content | Proof |
| --- | --- | --- |
| Gathering | owner = host = the host's Wii U PID (from nn-account `/internal/resolve/account`), max participants, participation policy 95, flags 2560, state 0, description `openpak-bridge:<game id>` (marks it bridged), the host as its only participant, registered | policy 95, flags 2560, state 0: **capture** (gatherings 1-4 on prod, `ensureWorld`); the marker is new |
| Matchmake session | game mode 15, application buffer (section 2), matchmake param (33 bytes, the capture's), progress score 100, random session key | **capture** N1; attribute 0 = `0x881` (without the public marker `0x30881`): **guess** (a friends-only game's value was never captured) |
| Station URL (GetSessionURLs) | the bridge's PIA station address, `PID` = the host's PID, `RVCID` 0xFFF2, `natm=1;natf=1`, `sid=15;stream=10;type=3` | shape **code** (the world's, which real Wii U clients accepted up to the NAT probe), values **guess** until a console joins |
| Friend presence | core session `minecraft` / `g<gathering>` for the host account, rendered by nn-friends as joinFlagMask 1, matchmakeType 2, joinGameId `0x101D9D00`, joinGameMode 1, `PID` = the host's PID, `groupId` = the gathering id, the viewer's own game key | the field set is **capture** (wiiu-join.md §1: what a real host publishes, read from nn-friends' log); using it for a friend whose own console is not the Wii U is new (**guess** until a Wii U friend sees the Join tab entry). The core shows an account the **newest** session that has a title: if the host's own console registers a newer session, the entry is hidden |

A Wii U that joins a bridged gathering: `JoinMatchmakeSessionWithParam` goes through the normal common handler (the title's friends check
runs against the host's friends; a verdict is recorded), then `GetSessionURLs` (recorded with the station handed out), then
`RequestProbeInitiationExt` naming the bridged station (recorded with the joiner's own station URL, the data the transport leg needs).

## 5. Join records

Every adapter's `Joins` returns, per attempt: time, game id, joiner's platform, kind (`join`, `urls`, `probe`, `signal`), who (native id and
OpenPak account when known) and `Data`, verbatim:

* Wii U (nex `GET /internal/minecraft/joins`): `join` {gathering, host_pid}, `urls` {station}, `probe` {target, station}; the joiner's PID.
* PS3 np-matching2 (`GET /internal/bridge/joins`): `join` {room, room_id, host, member_no, member_bin, endpoint, nat, request (the whole JoinRoom request, secrets redacted)}.
* PS3 np-community (`GET /internal/bridge/signaling`): `signal` {from JID, to JID, type, id, stanza (the whole stanza)}: NP signaling addressed to a bridged host's JID, which no console holds. The stanza is then handled as before.

All are also logged in full by the service that saw them.
