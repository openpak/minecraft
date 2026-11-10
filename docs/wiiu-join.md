# Wii U: how a player gets into the world (card openpak/prds#162)

Facts read from the 2026-11-07 real-console capture and from Quay (a Wii U emulator, same game binary) driving Minecraft: Wii U
Edition against production on 2026-11-09. Clean room: captures and the game's own behaviour only.

## 1. The Join tab is friend presence

The tab lists a friend when the friend's presence is online with `joinGameId`, `matchmakeType` and `groupId` set, `joinGameMode`
below 64 and `joinFlagMask` 1 (open) or 2 (friends). What a real game publishes while hosting an online world (read from nn-friends'
own log, Minecraft EUR v688, new online world):

| Field (NEX NintendoPresenceV2) | Value |
| --- | --- |
| game key | the title id and version of the game itself |
| `joinFlagMask` (Unknown2) | 1 |
| `matchmakeType` (Unknown3) | 2 |
| `joinGameId` (GameServerID) | 0x101D9D00, the game's id (the same on the EUR title) |
| `joinGameMode` (Unknown4) | 1 |
| host PID (PID) | the host's own PID |
| `groupId` (GatheringID) | **the gathering id** |
| application data | 20 bytes: `00000000 0881 0000 0f00 5c70 00000000 00000000` (net version 0x0881 and game mode 15 are the readable parts) |

The game then calls `FindBySingleID(groupId)` and shows the gathering's host name and settings ("OpenPak (Host)", survival, easy).

## 2. How a friend in the world gets that presence

`common/minecraft` publishes each console player in the world to the account core as a session (`minecraft` / `world`, renewed every
15 s, ended when they leave). nn-friends renders a friend with that session, for a Wii U viewer that runs Minecraft, with the values
above (the viewer's own game key, `groupId` = `MINECRAFT_WORLD_GATHERING`, host PID = the world's owner PID), and sends it again when
the viewer starts or leaves Minecraft.

## 3. After the join, before the first PIA packet (what the game does and expects)

1. `JoinMatchmakeSessionWithParam` (109/39), `GetSessionURLs` (21/41), `ReplaceURL` (11/7).
2. A NAT check against `nncs1/nncs2.app.nintendowifi.net` (UDP 10025 and the sinkholes 33334/33335). Quay resolves those names through
   the host's DNS, so against OpenPak the check goes to other servers and never completes; consoles use OpenPak's DNS (nncs2 has its
   own address there). With the names mapped to OpenPak's nncs hosts the check completes.
3. The station URL of the host must look like a registered host's: `PID`, `RVCID` (a connection id no live NEX connection has, because
   `RequestProbeInitiationExt` is relayed to the connection with that id) and the NAT types (`natm=1;natf=1` for an open server).
4. NAT traversal `RequestProbeInitiationExt` (3/3): the joiner gives its own public URL and waits about 15 s for the host to start
   probing it. A console host does (`0x34/1`, two requests, before anything else) and also calls `RequestProbeInitiationExt` back for
   its own public and private URLs (2026-11-07 NEX log: the joiner's two calls at 12:39:51.96, the host's two at 52.02), so the server
   sends the joiner `InitiateProbe` and the joiner probes the host (52.27). `POST /internal/probe` makes the service probe (repeated
   every 250 ms for 20 s until the joiner is heard from) and nex sends the joiner `InitiateProbe` with the world's station URL. Both
   halves matter: a joiner behind a port-restricted NAT (the test workstation, ufw included) drops every host probe until it has sent to
   the host itself. The station URL carries no private address: the box's private address is no use to any joiner.
5. Only then the joiner's PIA station handshake (connection request, session join) starts, as in the capture.

## 4. What still stops a Quay join (2026-11-09, evening)

Since #166 every NEX title and this service run on the host network, so the addresses are real; since nex v1.19.7 / minecraft v0.2.6
the joiner is told to probe the host (InitiateProbe) and does: 300 ms after it, Quay sends 0x34 port-1 probes to 22960 every ~310 ms
for 6 s and the service answers each (port 2) and probes back. The joiner never answers our probes and never sends its connection
request; 106-0334 follows. Capture: scratch `minecraft-lce-captures/wiiu-quay-20261109-2025/join-box.pcap`.

**Found (v0.2.7): byte 4 of the probe payload is the probe type.** The game's `nn::pia::nat::NatProbeData::Serialize`
(Minecraft.Client.rpx 0x03608f1c) writes u32 sender RVCID, u8 type, three bytes it never sets (stack leftovers, so the "constant per
station" `00aaa640` word is type 0 plus garbage), u64 token. `NatTraversalProtocol::ReceiveMessage` (0x03607004) runs `ProcessProbe`
for type 0 (and answers with a type 1 reply that echoes the token, port 2) and `ProcessProbeReply` for type 1, and drops anything
above 1. Only a type 1 reply whose RVCID matches the station the joiner was told to probe completes its NAT traversal; on 2026-11-07
the joiner sent its connection request 6 ms after the host's type 1 reply. v0.2.6 put a millisecond timer in that word, so every
reply had type 0 and read as one more probe; v0.2.7 sends type 0 / type 1 with zero padding. A joiner answers a type 0 probe only
while its entry for that address is fresh (`NatProbe::UpdateTargetAddress` returns 0 once a probe from the same address was seen), so
it not answering our repeated probes is expected. Retest: the joiner should connect right after our first reply.

## 5. The world needs its own IP: the game ignores the NAT check servers (2026-11-10, real Wii U)

A real Wii U (friends list -> Join) got as far as the probe exchange and stopped: it sent type 0 probes to 22960 for 6 s, the service
answered each with a correct type 1 reply (same layout as a console host's, token echoed), and the console never reacted. Its header
echo timer (bytes 10-11) stayed `0000` the whole time, so it never accepted a single packet from us; it ended with 118-0513.

Cause, in the game binary: `nn::pia::inet::NexFacade::UpdateNatServerAddress` (Minecraft.Client.rpx 0x03578c04) resolves
`nncs1.app.nintendowifi.net` and `nncs2.app.nintendowifi.net` (strings at file offset 0x1cbe110) and hands both addresses to
`InetInputStream::AddIgnoreAddress` (0x03589858, table at 0x10de4958). `InetInputStream::readFromSocket` (0x0358943c) compares
every received datagram's source IP (IP only, not the port) with that table and drops a match before decryption (result 0x2c09).
On OpenPak nncs1 is 145.241.199.19, the address the world used, so every PIA packet from the world was thrown away. Quay had the
same mapping, which is why its joiner ignored our probes too; joinprobe has no such filter, so it always worked.

Fix: the world has its own address. OCI private IP 10.0.0.192 (display name `minecraft`) on the primary VNIC with reserved public
IP 145.241.206.67; `deploy/openpak-minecraft-ip.service` puts it on `enp0s6` at boot; the service binds `MINECRAFT_RELAY_ADDR=
10.0.0.192:22960` (so replies and host-first probes leave from it), `MINECRAFT_RELAY_PUBLIC` and nex `MINECRAFT_SERVICE_STATION`
are `145.241.206.67:22960`. Rule: never serve PIA from an address an nncs name resolves to (any Wii U/3DS PIA title).
Capture: scratch `minecraft-lce-captures/wiiu-real-20261110/`.

## 6. First real-console probe exchange that worked, and the timeout that still ends it (2026-11-10 14:49, 118-0502)

On the console's second try after v0.2.8, our PIA packets were accepted. At 14:49:36.832 the console (RVCID 2) probed 22960
(`0000000200fac840 00000039548b665d`), the service replied type 1 at .833 with the token echoed, and the console answered our own probe
with a type 1 reply whose header echo field held our timer. It stopped probing after that one pair. About 100 ms later it sent
`EndParticipation` (MatchMakingExt 50/1) and then `SendReport`, and showed 118-0502. No connection request was sent.

What failed was the console's time budget, not the protocol. `InetConnectStationJob::PrepareNatTraversal` (rpx 0x03587600) starts a
15000 ms deadline (`li r0,0x3a98`) once the host URL is known. `WaitForNatTraversalStarted` (0x0358783c) and
`WaitForNatTraversalCompleted` (0x03587ba0) then wait for `IsTraversed`. On timeout they call `NotifyLeave` with a result chosen by
the NAT mappings (0x6442 when both sides are EIM). The per-probe window is NexFacade 0x74 = 5000 ms, plus 0x78 = 1500 ms for each EDM
side (constructor 0x03575de8). GetSessionURLs was answered at 21.935, and `EndParticipation` arrived at 36.974: 15.04 s later.

Where the 15 s went (box capture, `minecraft-lce-captures/wiiu-real-20261110-1449/`):

| When (14:49) | What |
| --- | --- |
| 14.036 to 21.934 | The console's JoinMatchmakeSessionWithParam was answered in 10 ms. The console retransmitted it at 16, 18 and 20 s and acknowledged our reply only at 21.934. |
| 23.5 to 28.6 | NAT check, three rounds. nncs2's first reply took 560 ms on the secondary box, which was swapping. |
| 32.607 / 32.631 | ReplaceURL and RequestProbeInitiationExt were answered at once, and InitiateProbe was sent at 32.633. The console retransmitted both at 34.6 and 36.6 and acknowledged at 36.712. |
| 36.832 | The console's first probe, 4.2 s after InitiateProbe. The exchange then took 57 ms. |

The service and nex answer each request in under 1 ms, and the console's upstream packets arrived on time. Packets from the box to
the house took 4 to 8 s, and only in these windows. The owner's PS3 on the same public address (np-community TCP 5223) shows the same
gaps in its kernel TCP acks: data sent at 09.903 was acked at 17.400, and data sent at 31.307 was acked at 36.718. Kernel acks do not
depend on any game, so both consoles waited on the same delayed downstream path: the home link, or something between the box and it.
The earlier attempt at 14:23 had no such delay: requests were answered and acknowledged within 30 to 70 ms, and the console probed at
once (its packets were still ignored because of section 5).

Retry when nothing else at home is downloading. With normal latency, the whole sequence fits in the budget with about 10 s to spare.
