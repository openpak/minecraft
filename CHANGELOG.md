# Changelog

## 0.3.7

- `joinps3` completes the PS3 login (prds#177; run mc5 2026-11-13 was the first game data from a real PS3 host). What mc5's 219 host bytes are, walked
  with `internal/lce/walk` (names now in `walk.Name`, the 129 ids of the 01.84 table plus A2): one kind-1 message of 74 bytes = `00` KeepAlive (int 0) +
  `02` ClientboundPreLogin (net version 0x0884, login key "-", one PlayerUID, the host's); then `00` KeepAlive (5 bytes, kind 1) once a second for
  28 s; then `FF` Disconnect reason 3. The "4-byte prefix" before the 02 is the KeepAlive's int, not framing. The host's pre-login handler (EB
  0xbc1760) answered our E8 at once, then its pending connection waited for the `01` Login for 600 ticks (EB 0xbc01b4, 30.2 s measured) and
  dropped us with reason 3, because we never sent it.
- New `ps3join.Game`: the PS3 joiner's game layer, reusable by the bridge like `pia5.Joiner` + `lce.Client` on the Wii U leg. `OnGame(bytes)` takes
  the host's kind 1/2 payloads (pieces may cut a packet anywhere), `Write` hands ours out, `OnPacket` sees every packet both ways. It reuses
  `lce.Client` (new `NoSession` option: no FE hello, no channel-3 tick, E8 at once) through the walker: host packets are translated PS3 -> Wii U
  (version, 28 -> 20-byte PlayerUIDs; chunks 0x33 go to the client's chunk channel), the client's packets Wii U -> PS3. PS3 specifics on the `01`: our
  28-byte PlayerUID in both id fields and the host's friends-only byte + UGC players version echoed from its `02` (the host compares that int with
  its own and answers a mismatch with the 02 again, EB 0xbc1a24). In the world it is the Wii U joiner's behaviour: `87` + `0D` after D6, `E2` +
  `0B` once a second.
- `joinps3` records the game stream by name (`WALK rx/tx 0x.. <name>`), never stops on an unknown packet (the walker stops walking, the session and
  the raw `.rx.bin` go on), reports packets/chunks/position every 30 s and in a second SUMMARY line, holds until `-hold`, ends early only on the
  host's FF, then stops sending and leaves the room.
- `-nudge59` is now on by default (10 s; `-nudge59 0` turns it off). It is a workaround: only mc5's nudge made the host add us, and why it does not
  on its own 0x3207 reply (the path a real joiner takes) is still unknown.
- Tests: `TestGameLoginAfterMc5` replays mc5's host bytes (whole and cut at three places) and asserts our E8 and the next message, the `01`, byte
  level; `TestGameEchoesUGCPair`, `TestGameInWorld` (01 response + D6 -> 87 + 0D, in the PS3 dialect), `TestGameUnknownPacketDoesNotAbort`.

## 0.3.6

- `ps3p2p` takes the RUDP values a real PS3 showed (prds#177, #145; run live10 2026-11-13, a Terraria host's SYN and RESET, decoded against
  librudp.prx in research matching2 note §43 and transport note §8). Same library, so they carry over: window byte 0xff -> 0x30, MSS 1350 -> 1410
  (0x582, the library default; payload per segment 1346 = `MaxPiece`), cap word 0 -> 0x0201 and mirrored from the console's own SYN (librudp
  silently aborts when the peer's cap bits differ from its flags, so the old SYN would have been refused once the host's socket existed),
  version words confirmed (refused as the library does: verA < 0x0100 or verB > 0x0100), class 2 + port 1 confirmed.
- The SYN / SYN+ACK retransmit timer is librudp's: 1 s doubling to 16 s (the console showed 1, 2, 4, 8 s, then a RESET reason 3 at its 30 s
  connect timeout). `Describe` decodes the RESET block (reason, connection id); the SYN block is read after the options, in librudp's order.
- `guess.go` now separates "seen on hardware" (MSS, window, cap, versions, SYN timer) from what is still a guess (data RTO, keepalive,
  empty-ack form, kind 1/2 use, hello pad). `TestReplayRealConsoleSynAndReset` replays the console's two datagrams and asserts our reply byte for
  byte; `TestSynBackoff` pins the timer.

## 0.3.5

- Why a PS3 would still drop a Wii U-hosted game after np-matching2 v0.2.28 answers its list request (prds#177): the session data we
  advertise had byte +0xaf = 0, copied from the golden, which is a real host's first update. The 01.84 list build (EB 0xc87b04) drops any
  entry whose +0xaf is 0. All seven recorded hostings set +0xaf = 1 about 6 s after creating the room and never change it back, so
  `PS3Codec.External` writes 1. Test: `TestPS3ExternalIsARealHostsFinalSessionData` (the last 0x1207 of the 2026-11-13 hosting, every byte but
  the +0xa6 game setting, and the joiner's version/+0xad/+0xaf filter).
- `joinps3` asks GetRoomDataExternalList the way the real game does (typed ids 0x2055/0x2056 plus the four owner-info ids, `0x29` = room id bits 47..32), so a probe run exercises the list path a real PS3 takes.
- `docs/session-mapping.md`: the joiner's checks in order, what the 2026-11-13 run proved, and the NpId a PS3 uses toward a bridged host.

## 0.3.4

- Why mc4 (2026-11-13) had no RUDP after signalling was established (prds#177, transport note §7, static on the 01.84 EBOOT and lv2 4.93):
  the PS3 host opens its P2P socket and Initiates RUDP only after it has added the joiner, and it adds the joiner only in the handler of
  its GetRoomMemberDataInternal (0x3207) reply (EB 0xc9735c event 0x5102 -> EB 0xc96560 event 0x10c -> AddPlayer EB 0xc95900 -> 0x3204 ->
  EB 0xc943e0). mc4 shows no 0x320e with our entry, so that reply never succeeded. Until the host binds UDPP2P vport 1 the kernel drops
  every kind-6 datagram before decrypting it (K 0x16063c), so our 284 SYNs, which are correct (the MAC verifies with the PS3's skey and
  they decrypt to a class-2 SYN on port 1), went nowhere.
- `joinps3` sends the host's own request for itself after signalling is established (0x3207, our member, attr 0x59) and logs the reply's
  member and 0x59 (`diag: 0x3207 reply`), so the log shows what the host's AddPlayer gets from the room server.
- `joinps3 -nudge59 <d>` (experiment, off by default): if the host has not added us `<d>` after Established, write our 0x59 as 0 and then as
  the mask with SetRoomMemberDataInternal (0x3206). np-matching2 pushes each as 0x320d; the host's 0x1107 handler (EB 0xc96ea0) adds new
  pads the same way, AddPlayer and RUDP Initiate included. Logged as `EXPERIMENT nudge59 ...`.
- `ps3m2`: `Client.Send` (a request whose reply arrives on the Events reader), `MemberBin`, `CmdSetMember`; replies after the join are MAC
  checked with the session key. Test: the three post-Established requests on the wire.

## 0.3.3

- `rtsig` (prds#145, research §42): an iq `error` (the peer refusing one of our sets, vsh code in `<x code=..>`, `Stanza.ErrCode`) is reported
  through `Participant.OnReject` and never acked or parsed as a fresh offer/accept. On run mc3 the PS3's echoed rejection of our offer was
  taken as its offer and the probe probed its own address 158 times.
- `rtsig`: `Participant.SetHostNpID` / `JIDFromNpID` address the console by the JID of its own NpId (`PS3Test`+`ptnpps3` ->
  `PS3Test@pt.np.np.playstation.net/ps3`) and pin it against np-community's stamped `from` (the console's stream domain `pt.pt`). The
  PS3 rejected mc3's offer with 0x8002a80e because `pt.pt` maps to `ptptps3` and the connection its Matching2 activated holds `ptnpps3`.
- Tests replay the console-initiated mc3 exchange and the probe-initiated mc2 one stanza for stanza (`signal_replay_test.go`).
- `joinps3` takes the host's NpId from the room owner in the SearchRoom reply (`ps3m2.SearchItem.OwnerNpID`, needs np-matching2 >= v0.2.27)
  before JoinRoom, and logs `REJECTED` with the error code when the PS3 refuses a stanza.

## 0.3.2

- PS3 presence blob and session data built like a real host's (prds#177): the 56-byte presence blob now equals, byte for byte, the blob the owner's real
  PS3 published while hosting (2026-11-11 14:58; server id is a u16, +0xa8 and +0xac of the session data are copied, the name comes from
  session +0x02..+0x13). The session data writes the host name at +0x02 too (the real host does; the 14:12 capture had it blank), so the blob
  and the room list carry the bridged host's own name. Open: the real blobs' @0x18 is not the room id (docs/session-mapping.md section 3).
- Tests pin that a game's end takes the PS3 room, the PS3 presence, the Wii U gathering and the Wii U presence away together.

## 0.3.1

- PS3 transport and the `joinps3` probe (prds#177), built from `research/minecraft-lce/ps3-0184-p2p-transport-2026-11-11.md` (static analysis; no
  PS3 game packet has ever been captured, so the first hardware run confirms or corrects it):
  - `internal/ps3p2p`: the kernel's kind-6 datagram (`ff c6 01 01`, seed, 4-byte HMAC-SHA1 over the ciphertext, AES-128-CBC with the
    seed x4 IV and the partial-block rule; each side opens with its own skey and seals with the peer's), the librudp layer (class-2
    frames, port = pad + 1, simultaneous open, cumulative acks, go-back-N retransmit, keepalive) and the `{u16 len, u16 kind}` message layer
    (kind 1 plain, kind 2 acked by an empty kind 3, 1,346-byte pieces). Every guessed constant is in `guess.go`. Tests: three independent
    crypto vectors (openssl + Python), the SYN frame, a two-endpoint loopback through the real crypto, retransmit after loss.
  - `internal/ps3m2`: a headless PS3 Matching2 client (trimmed copy of np-matching2's `wire` and `client`), with parsers for the search list,
    the external attrs, the member list and the player-table attrs 0x57/0x58. `internal/rtsig`: copy of the Terraria RT/udpp2p signalling
    package (verified against a real PS3), plus `PeerSKey`/`HostCandidate`. Copied, not shared: both are `internal` packages of other modules.
  - `minecraft joinps3`: signs in to RT, searches, reads the external attrs, joins the host's room with member attr 0x59, signals to
    Established, opens RUDP, waits for the host to write our table entry, and only then sends the 32-byte hello (and E8). It records
    everything to `-out`: every `ff c6` datagram raw and decrypted, every RUDP packet and message, hex dumps, the game bytes each way
    (`.rx.bin` walks with `internal/lce/walk` as PS3), the skeys in a 0600 `.keys` file. Never aborts on an odd packet; prints a SUMMARY line
    (signalling, established, rudp connected, table entry, hello, first game packet). Runner: `scratch/ps3-signaling-2026-11-09/run-minecraft.sh`.

## 0.3.0

- Session directory (prds#177, `internal/directory`, `docs/session-mapping.md`): a game a player hosts on one console is advertised on every other
  console network, under the hosting player's name, through that network's own discovery; the service is an invisible bridge. One
  `Game` per player-hosted game (host account, platform, native session reference, neutral settings, players, state) fed and consumed
  through a two-halved `Adapter` (source: `Hosted`; sink: `Advertise`/`Withdraw`; `Joins` reports join attempts and handshake data back).
  Adapters for the Wii U (nex minecraft-wiiu `/internal/minecraft/*`, presence in the account core for nn-friends) and the PS3
  (np-matching2 `/internal/bridge/*`, np-community presence and signalling); codecs checked against the real PS3 01.84 host's bytes
  (2026-11-11 log) and the Wii U capture row. A new platform is one more adapter. Game traffic between consoles is not part of it.
- `minecraft serve` starts the directory when `MINECRAFT_NEX_URL`, `MINECRAFT_PS3_MATCHING_URL` and `MINECRAFT_PS3_COMMUNITY_URL` are set
  (also `MINECRAFT_PS3_KEY`, `MINECRAFT_PS3_TITLE`); `GET /internal/directory` shows its games and the last 200 joins.

## 0.2.13

- `joinconsole` (prds#177): also probes the host's public address on its private station's port from the start, so traversal
  does not depend on the host's first probes arriving (second real Wii U attempt 2026-11-11: none arrived in 20 s).

## 0.2.12

- `joinconsole` (prds#177): the joiner also probes the address the host's own probes come from. The public station URL NEX
  holds for a console host can carry the port of its NEX socket, not its PIA socket (real Wii U 2026-11-11: URL port 5224, PIA
  on 5254), so probing only the listed addresses never completed NAT traversal.
- `joinconsole`: `-nex` defaults to the minecraft-wiiu health listener on 127.0.0.1:22742.

## 0.2.11

- `minecraft joinconsole` (prds#177): joins a world hosted on a real Wii U as a Wii U joiner on behalf of a player on another
  console, and holds the session. NEX side through nex minecraft-wiiu's internal routes (v1.19.9); `pia5.Joiner` now does a console
  joiner's NAT probe phase (probe every host address, connect after the first type 1 reply), answers the host's probes and RTT
  requests and sends RTT and clock requests; `lce.Client` is the joiner's game layer (FE, E8, 01, 87/0D, 0B, E2, channel 3 ticks).
  Checked byte for byte against the 11-07 recorded joiner (docs/provenance.md J1-J6, docs/wiiu-join.md section 9).
- `internal/lce/walk` (prds#177): splits a length-less console game stream into packets for all 130 Wii U v688 ids (the 129 shared
  with PS3 01.84 plus A2) and translates packets between the Wii U and PS3 dialects (net version 0x0881 <-> 0x0884, PlayerUID
  20 <-> 28 bytes in 02, 01, 14, D1, E7, CE). Library only; nothing calls it yet. docs/provenance.md W1-W12.

## 0.2.10

- The PIA mesh join response goes to destination 0, as a console host sends it. Sent to the joiner's station bit (2), a real Wii U
  dropped it and the join ended 118-0502 after the mesh join (2026-11-10 21:10). docs/wiiu-join.md section 8.

## 0.2.9

- Presence is shared per Wii U PID: two world sessions of one PID (a joinprobe restart, or a console that rejoins before its old
  session timed out) publish one core session, and only the last to leave ends it. Before, the old session's leave expired the row
  its successor held (the core keeps one row per account and title), so the player dropped off the Join tab.
- docs/wiiu-join.md section 6: the 2026-11-10 14:49 real Wii U join. The console accepted our probes and replies; the join timed out
  because packets to the house arrived 4 to 8 s late. Probe test pinned to that exchange.

## 0.2.6

- `POST /internal/probe` keeps probing the joiner every 250 ms for up to 20 s until the joiner is heard from, then probes once more. A
  joiner behind a port-restricted NAT drops the host's probes until its own probe (nex sends it `InitiateProbe`) has opened its mapping.

## 0.2.5

- A datagram that no leg's key opens is logged (once a minute per address) instead of dropped silently.

## 0.2.4

- `POST /internal/probe` (gathering, address, port): the host probes a joiner's public address, which is what a console host does when
  the NEX server relays the joiner's `RequestProbeInitiationExt` (the Wii U game sends it after the join and gives up after 15 s
  without the host's probes). nex forwards it for the world.
- The host announces RVCID 0xFFF0 (was 1): nex relays that request to the connection with the URL's RVCID, so it must not be a live one.

## 0.2.3

- The PIA host sends the two NAT probe requests (0x34/1) a console host sends a joiner before anything else (read from the capture's
  first two host packets); a real console joiner waits for them and never sent its connection request without them (first real
  Quay join through the Join tab, card #162).

## 0.2.2

- `joinprobe` sends a tick message every 5 s like a console, so it stays in the world for `-wait` (the service drops a silent joiner after 30 s).

## 0.2.1

- The default nn-account resolve URL is the real port (20050); `joinprobe -wait` sets how long the probe stays in the world.

## 0.2.0

- Players in the world are published to the account core's live-session registry (namespace `minecraft`, title `world`, client
  `wiiu`), renewed every 15 s and ended when they leave, so adapters can show a friend as being in the shared world (card #162).
  The Wii U PID is resolved to the account through nn-account's `/internal/resolve/pid`. Enabled by `MINECRAFT_CORE_ADDRESS`
  (`MINECRAFT_CORE_INTERNAL_KEY`, `MINECRAFT_RESOLVE_URL` default `http://openpak-nn-account:20050`, `MINECRAFT_RESOLVE_KEY`).
- `minecraft joinprobe -pid N`: join as a given Wii U PID (a test account), so that account shows as in the world.

## 0.1.2

- Fix: a deadlock on the first game message (the game callbacks ran under the host's lock while they send); found by the new
  end-to-end test, which joins a real Java 1.8.9 server with a joiner built from the capture's joiner messages.
- `minecraft joinprobe`: joins a running service the way a console does and counts what comes back (the key is read from a file).

## 0.1.1

- Station logging; the periodic RTT (0.2 s, then 2 s) and clock (1 s) traffic the console host sent in the capture.

## 0.1.0

- First release: Java 1.8.9 world server supervised in the container, the Wii U adapter (PIA GCM station, console session
  handshake, chunk codec) and the internal leg API. Players see the Java world's terrain; movement is forwarded to Java.
