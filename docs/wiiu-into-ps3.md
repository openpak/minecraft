# A Wii U joins a game a real PS3 hosts (card openpak/prds#177)

The first cross-play session. A player hosts Minecraft on their PS3; the game is advertised on the Wii U network (docs/session-mapping.md);
a Wii U player joins it. The service is neither player nor host: toward the Wii U it is the station the gathering's NEX data names, toward
the PS3 it is the joining player, under that player's own PS3 identity (every OpenPak account has one). The code is `internal/bridge`
(session, manager, trace), `internal/relay` (the bridged leg), `internal/ps3join` (the PS3 joiner, now embeddable). The opposite direction
(a PS3 into a Wii U-hosted game) reuses `bridge.Session` with the dialects swapped and is not built.

## 1. What happens, in order

| # | Step | Who | Proof |
| --- | --- | --- | --- |
| 1 | The Wii U lists the game (friend presence), calls JoinMatchmakeSessionWithParam, GetSessionURLs, RequestProbeInitiationExt. | nex minecraft-wiiu | **real console** 2026-11-11 and 11-13 |
| 2 | nex (v1.19.12) pushes the gathering's PIA leg: `POST /internal/legs {gathering, host_pid, joiner_pid, session_key, game:"ps3:<room>"}`. The service registers the leg (station RVCID 0xFFF2, as in the URL handed out) and starts a session. | nex -> relay | tests only |
| 3 | PS3 leg (`ps3join.Run`, plugged with `NewGame`): the joiner's account comes from the Wii U PID (nn-account resolve), its PS3 identity, NP ticket and the title passphrase from np-matching2 `GET /internal/bridge/identity`, the host's online id the same way. RT sign-in as the joiner, Matching2 login, search, GetRoomDataExternalList, JoinRoom with 0x59, udpp2p signalling, RUDP, the 0x59 nudge, the host's table entry, the 32-byte hello. | service -> PS3 | **real PS3** (joinps3 runs mc4-mc6, 2026-11-13), but never under a service identity or from the box |
| 4 | Meanwhile the Wii U probes the station (type 0), the leg answers (type 1, token echoed), the Wii U opens its PIA session (mesh, station handshake) and sends the FE session hello. | Wii U <-> relay | PIA host: **real Wii U** 2026-11-10 against the world; the bridged RVCID is new |
| 5 | The leg answers the FE hello with the FF hello (host under the host's own Wii U PID, the PS3 host's name, the joiner as it called itself). No PS3 counterpart: made here. | leg | the template is the capture's, as the world's |
| 6 | The Wii U's E8 waits until the PS3 hello left, then reaches the host with net version 0x0884 and the joiner's PS3 online id as its name: byte for byte mc5's E8. | session | **mc5** (bytes), test |
| 7 | The host answers `00` KeepAlive + `02` PreLogin; the Wii U gets it as a Wii U 02 (0x0881, the host's 20-byte id). The Wii U's `01` Login reaches the host with the joiner's PS3 PlayerUID in both id fields, the joiner's online id as the name and the host's UGC pair echoed. | session | the host's side **mc5**; the Wii U's 01 reply and everything after it are **guesses** (no PS3 host has ever accepted a login) |
| 8 | From the login response on both streams are walked (`internal/lce/walk`, all ids), translated and forwarded: the Wii U's 87/0D/0B/E2 to the host, the host's chunks (0x33) to the Wii U on channel 2 in 5,120-byte pieces, everything else on channel 1, a PS3 message cut at 1,346 bytes is reassembled before it is walked. The Wii U's clock tick (channel 3) is dropped. | session | walker layouts: Wii U **capture**, PS3 **binary-derived** |
| 9 | Either side leaving ends both legs: the host's FF is passed on and the PS3 leg leaves the room; the Wii U's FF, or 20 s of silence, ends the session; any other end gives the Wii U `FF 00000003` and closes its peer. | session | the FF reason 3 is mc5's; for the Wii U a guess |

## 2. Identities (never the service's)

* The joiner is the account behind the Wii U PID; on the PS3 it is that account's np link (online id, country) with a UID built like the
  probe's (`PlayerUID`: online id, flags, MAC `00:1f:a7:0f:00:01`, user id 1; one console MAC for every bridged player is a guess).
* The host is shown on the Wii U with the PID of its own Wii U identity (nex's gathering owner), the name its PS3 wrote into the game,
  and a 20-byte id derived from that PID; the PS3 PlayerUID of the host (its table entry 0, and the 02) is paired with it, so any later
  packet that carries the host's id carries the same one on both sides (`walk.UIDMap`).
* Names inside packets other than E8 and 01 (AddPlayer's name, chat) are not rewritten: the Wii U sees the host's PS3 online id, the PS3
  sees the joiner's online id (E8 and 01 carry it). A guess that this is enough.

## 3. On the box (see the service's README for the env names)

| What | Value |
| --- | --- |
| Wii U leg | UDP 10.0.0.192:22960, public 145.241.206.67:22960 (unchanged) |
| PS3 leg socket | UDP, bound on the second address (`MINECRAFT_PS3_BIND`, default the host of `MINECRAFT_RELAY_ADDR`), announced as `145.241.206.67:<port>` in the signalling candidates; one port per session, first free of `MINECRAFT_PS3_UDP_PORTS` (default **3658-3667**). Open them (UDP, from the PS3's address or anywhere) in the cloud security list and the firewall. |
| np-matching2 | internal HTTP (`MINECRAFT_PS3_MATCHING_URL`, loopback 8538), plain Matching2 `127.0.0.1:3480`, TLS front `127.0.0.1:443` (SNI `matching.np.community.playstation.net`; or 8443) |
| np-community | RT `127.0.0.1:5223` (TLS) |
| Trace files | `MINECRAFT_BRIDGE_TRACE_DIR` (default `/data/bridge`) |

## 4. The trace (one set of files per session, `<UTC time>-<game>-pid<Wii U PID>.*`)

| File | Content |
| --- | --- |
| `.log` | `STATE` lines (preparing, ps3-joining, ps3-signalling, ps3-established, ps3-rudp, ps3-room, ps3-hello, wiiu-connected, wiiu-hello, prelogin, login, in-world, ended <reason>), every packet by name in both directions with the size before and after translation (`WALK wiiu->ps3 0x01 ClientboundLogin, 136 -> 152 bytes`), the PS3 leg's RT stanzas (skeys masked), datagrams, RUDP frames and messages in full hex (the joinps3 log) |
| `.rx.bin` | raw game bytes the PS3 host sent (what `walk.Walk(.., walk.PS3)` reads) |
| `.tx.bin` | what we sent the PS3 host: the hello and every translated message |
| `.wiiu.rx.bin` | raw reliable messages the Wii U sent, each `u16 length` + message (`00 channel` framing on it) |
| `.wiiu.tx.bin` | what we sent the Wii U (translated, Wii U dialect), same framing |

The service log has one line per state change (`bridge session game=... joiner_pid=... state=... detail=...`).

## 5. Guessed

The login template tail the Wii U sends (skin and cape ids, flags) is accepted by a PS3 host; the PS3 login response and D6 translate to
something the Wii U accepts; both sides tolerate the other's keep-alive rhythm and no PS3 keep-alive timeout kills the session; the Wii U
accepts a host it was handed the station of with RVCID 0xFFF2 whose first answer is a probe reply (the world's RVCID 0xFFF0 worked); the
Wii U's FE hello timing allows the E8 to wait for the PS3 leg (seconds); the first Wii U join may fall into the 15 s NAT budget while the
PS3 leg is still signalling (it runs in parallel and does not delay the NAT traversal); `FF` reason 3 reads as a normal end on the Wii U.
