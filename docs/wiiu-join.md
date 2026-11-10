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

The service and nex answer each request in under 1 ms (every reply is on `enp0s6` within 0.2 ms of the request). Our replies
then took 4 to 10 s to be acknowledged, and only during these windows. The owner's PS3 (np-community TCP 5223) on the same public
address stalled in the same way. Our segment sent at 09.903 was acked at 13.778, with the ack echoing the original's timestamp. Its
retransmits from 10.3 to 13.1 s arrived as duplicate SACKs from 20.58 to 21.67 s, in order and 8 to 10 s late, so the packets sat in
a queue. We sent the house under 7 KB/s at the time. A box capture cannot say which direction held them: an upstream delay that is
the same for every packet keeps the console's 2 s retransmit spacing intact.

Measured again from the house at 17:13 to 17:35 the same day (`scratch/minecraft-lce-captures/wiiu-real-20261110-1449/linktest/`, box and desktop clocks NTP-synced
within 20 ms):

| Test | Result |
| --- | --- |
| Box to 1.1.1.1:443, TCP connect every 0.25 s for 3 min | 712 connects, max 22 ms. The box and OCI are clean. |
| Desktop to 1.1.1.1:443, same test, same 3 min | p50 19 ms, p99 1.1 s, max 4.2 s, one failure. |
| Desktop to box, timestamped echo over SSH every 0.1 s for 5 min | Upstream p99 18.7 s (max 21.5 s), downstream max 23 s, p50 60 ms. |
| Desktop to Wii U 10.87.0.12 on the LAN, ping every 0.1 s | 0.8 ms p50, 2.8 ms max, no loss. |

So the stall is not on the consoles' side of the router, and it is not specific to consoles. The Wii U is wired (USB LAN adapter,
ASIX MAC 00:0e:c6) and its LAN link is clean. The desktop, also wired, stalls in both directions on its internet traffic. The two
desktop SSH round trips that looked normal at 14:49:25 and 14:49:31 were lucky samples: other SSH sessions from the house (box
ports 22/5360 and 22/5342) were acked 10 to 30 s late all afternoon. The shared hop is the home router or the line behind it.

The router answers LAN pings in 2 ms to 355 ms (mean 18 ms), so it is busy. The desktop has been sending a Sunshine game stream
(119 Mbit/s plus FEC, about 150 Mbit/s on `enp4s0`) to a LAN client almost without a break since 2026-11-09 20:07. Sunshine is not
reachable from the internet, so the stream stays on the LAN. It is the main suspect for overloading the router, but this is not
proven: a test with the stream stopped is still needed.

No server change can help: the console spends its 15 s budget waiting on a house link that holds packets for seconds, and our
replies are already sent. Before the next retry, stop the Moonlight session and run the desktop-to-1.1.1.1 test again. If it is
clean (max well under 1 s), retry the join. With normal latency, the whole sequence fits in the budget with about 10 s to spare.

## 7. The joiner refuses a host that is not a gathering participant (2026-11-10 20:46, 118-0502)

With a clean house link the probe exchange finished in under 100 ms (console probes 15.843 to 15.889, our type 1 replies at 15.844,
the console's replies to our probes up to 15.889), and the console still sent no connection request: `EndParticipation` (50/1) at
15.950 and `SendReport` at 16.019. So 118-0502 is not the 15 s NAT budget; the 14:49 run (section 6) ended the same way, one probe
pair then `EndParticipation` about 100 ms later, and its 15 s match was a coincidence of the slow link.

The `SendReport` blob is the game's PIA session-end monitoring report: AES-GCM with a key table in the rpx (key 13, IV = header
bytes 6-13 plus 4 table bytes), then zlib (`MonitoringDataSender::MakeReportData`, 0x0358f23c). Decoded, it records join result
0x6439 after 2802 ms. 0x6439 is the generic code JoinMeshJob stores when its connect job fails (`StartConnectingToHost` /
`CheckTransportConnectionStatus`).

Why the connect job fails: after the NAT traversal it goes `ResolveCurrentAddress` -> `TryCurrentAddress` ->
`SendConnectionRequest` (0x035496f8), which first runs `CheckErrorOccurred`. For the inet job (0x03587310) that takes the host's
station id (its PID) and calls `NexMatchMeshLayerController::IsParticipated(gathering, id)` (0x035d5d40). That looks the PID up in
a list filled only by NEX notification 3001 (participation, subtype 1; `ProcessNotificationEvent` 0x035d661c keys the entry on
Param2). If the host is not listed, the job errors out before the connection request is sent.

Our world gathering (flags 2560, including 0x800 "participation events reproducible") makes nex send each joiner a 3001 recap for
every current participant. But nex's minute reset set `participants = '{}'`, so the recap was empty and the world host 2000000001
was never named. nex v1.19.8 keeps the host PID in the list (creation and the minute reset). A real console host is always in its
own gathering's participant list, which is why the 2026-11-07 console-to-Quay join worked.
