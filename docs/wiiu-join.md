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

## 4. What still stops a Quay join (2026-11-09)

The joiner's public URL in step 4 carries the address the NEX server saw: `10.89.2.24`, the Traefik UDP proxy's container address
(the service sees `10.89.2.171` the same way), with `natf=2;natm=1;probeinit=1`. A host cannot probe that. Until the NEX servers (and
this service) see real client addresses, or a relay path replaces host-first probing, the join times out after about 15 s
(error 118-0502 / 118-0521) with `EndParticipation` and `SendReport`. A real Wii U check by the owner is still open.
