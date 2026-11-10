# Provenance

Clean-room record (owner rule, card openpak/prds#162): every console (LCE) wire fact in this repo was read from a real console
capture, or from our own code. No leaked Legacy Console Edition source, no community or forked LCE server, no source-derived
document was used or consulted while writing this repo. The only inputs were:

- the Wii U capture `scratch/minecraft-lce-captures/wiiu-quay-20261107-123534/` (a real Wii U EUR v688 hosting, Quay joining,
  recorded 2026-11-07; analysis in the private research repo `minecraft-lce/wiiu-capture-2026-11-07.md`),
- `common/terraria/internal/pia` (our PIA 3.x code) and the OpenPak NEX title `minecraft-wiiu` (Pretendo's AGPL server, merged),
- the public Java Edition protocol documentation (minecraft.wiki) for the Java side,
- the public kinnay NintendoClients wiki (PIA) as a hint for the packet header, confirmed by the capture's GCM tags.

A fact is implemented only if a capture row below confirms it. Capture files are not in this repo (the session key is a live
secret); tests that need them read `MC_CAPTURE_DIR` and skip without it.

## Confirmed facts → where they live

| Id | Fact (capture evidence) | Implemented in |
| --- | --- | --- |
| P1 | PIA packet: magic 32AB9864, mode 02, connection id, packet id, two timers, 8-byte nonce at 0x0C, 16-byte GCM tag at 0x14, ciphertext from 0x24, messages padded with 0xFF to 16. All 3,882 packets of the join open with the session key and re-seal to the same bytes | `internal/pia5/codec.go`, `capture_test.go` |
| P2 | GCM key = first 16 bytes of the NEX matchmake session key; nonce = packet connection id + low 24 bits of the gathering id + the 8 header nonce bytes; no AAD (proven by the tags) | `pia5.NewCipher` |
| P3 | Message header: flags, u16 size, u64 destination, u64 source (the sender's PID), protocol type, port, 3 pad; payload padded to 4. Types 0x14 station, 0x18 session (port 0 join, port 1 reliable mesh), 0x1C clock, 0x34 NAT probe, 0x58 RTT, 0x7C reliable game channel | `pia5.Message` |
| P4 | Reliable segment: 24-byte header (flags 1 data / 2 last, size, 4 zero, seq, ack, 8 extra), first sequence 0xFFFFF82F per stream, one cumulative ack field | `pia5.Reliable`, `Peer.reliable` |
| P5 | Station handshake: probe pair (34/1 answered on 34/2, the last 8 bytes echoed), connection request 91 B (type 1) answered by an ack (type 5) and the inverse request, connection response 255 B (type 2), mesh join request 14 B (18/0) answered by the join response 164 B, RTT and clock answers, mesh update 588 B on 18/1; locations are 35 bytes (address, PID, CID, RVCID, URL/NAT bytes) | `pia5/host.go`; replay in `replay_test.go` |
| P6 | Packet header connection id: 0 during the handshake, then the id from the connection requests (0xBB host, 0xC4 joiner); packet ids 0 then 1, 2, 3 ... | `Peer.emit` |
| P7 | The 8 bytes `08 00 08 00 00 00 00 02` after the join response's station count and the connection response's unmapped bytes are replayed from the capture's host | `meshHeader`, `connResponse` |
| P8 | Nonce prefix = the sender's RVCID (host 3, joiner 4 in the capture) | `NewHost` |
| P9 | Max UDP payload 1,204 bytes | `segMax` |
| G1 | Game messages ride 0x7C port 0: `FE` joiner hello, `FF` host hello, `00 <channel>` + game bytes; channels 1 packets, 2 chunks, 3 joiner tick; pieces of at most 5,120 game bytes | `lce/session.go` |
| G2 | FE hello (257 B): 20-byte joiner id at 5, name (ASCII) at 85. FF hello (641 B): host row (id at 1, PID at 29, name at 33) and joiner row (id at 81, PID at 109, name at 113); the rest is replayed | `Session.hello`, `tmpl/hello_ff.bin` |
| G3 | Join order: E8 pre-login (short net version 0x0881, name) → host keep alive `00 <int 0>` then 02 pre-login reply (short 0x0881, host name UTF-16, host id) → 01 login → login response → world sync → chunks | `Session.packets` |
| G4 | Login response (193 B) and the 18,521-byte sync packet that follows it are replayed verbatim; the sync packet ends with the spawn position as three big-endian doubles (offset 18,352) and a window-items packet of 46 empty slots; only the doubles are changed | `tmpl/login.bin`, `tmpl/world_sync.bin`, `worldSync` |
| G5 | Client position: `0B` + 3 doubles (x, feet y, z) + ground byte (26 B), or `87 01 0D` + 3 doubles + 2 floats + ground | `Session.position` |
| G6 | Time packet `04` + game time (long) + day time (long) | `Session.timeUpdate` |
| C1 | Chunk message on channel 2: `33`, full flag 01, int x·16, short 0, int z·16, sizes 15/255/15, int (dimension<<30 \| compressed length), zlib data, two zero bytes | `chunkPacket` |
| C2 | Payload is zlib, then a 0xFF run-length pass (FF, count-1, value for runs of 4–256; FF, count-1 for 1–3 literal FF) | `lcechunk.unrle/rle` |
| C3 | Block part: u32 BE length of the rest, four u16 LE counters, 16 u16 LE section starts (256-byte pages), 16 section page counts; every section is a 128-byte index of 64 u16 LE tile entries and the tile data, padded to whole pages | `lcechunk.Parse/Serialize` |
| C4 | Tile entry = mode<<12 \| offset in 4-byte units; mode 0 uniform (entry is the value), 2/4/6/8 = 1/2/3/4 bit planes (MSB first within the byte, low plane first) with 2/4/8/16 u16 LE palette entries (first-use order, 0xFFFF fill), 14 = 64 raw u16. Voxel value = block id<<4 \| data | same |
| C5 | Voxel and tile order: x slowest, then z, then y fastest (bedrock on y 0 in 5,120 of 5,120 columns; x/z fixed by height continuity across neighbouring chunks, 0.9 vs 5.9 block mean difference) | `lcechunk.Idx` |
| C6 | After the blocks: four layer stores (block light lower 128, upper 128, sky light lower, upper): 128-byte index (0x80 all-zero layer, 0x81 all-0xFF layer, else a page number) and 128-byte pages of 16×16 nibbles, x slowest, low nibble first; verified against the decoded blocks (sky light 15 in 124,215 air voxels vs 1,831 solid) | same |
| C7 | The counters after the block length are ceil(pages/2) of each store with bit 15 set when the page count is even | same |
| C8 | 256 biome bytes close the column, index z·16+x (neighbour match 0.956 vs 0.508) | same |
| C9 | The console's own encodings are not canonical (palettes with duplicates, wider modes than needed); this encoder writes the smallest mode, which decodes to the same column | `lcechunk_test.go` |
| N1 | The world's matchmake row: game mode 15, 8 participants, participation policy 95, flags 2560, application buffer (212 B, net version 0x0881 first) and matchmake param (33 B), with the console names removed | nex `titles/minecraft-wiiu/nex/world*.go` |
| N2 | A Wii U joins with `JoinMatchmakeSessionWithParam` (protocol 109 method 39) then `GetSessionURLs` (protocol 21 method 41) | nex hooks |
| N3 | The joiner's PIA connect job (`InetConnectStationJob::CheckErrorOccurredImpl`, rpx 0x03587310) asks `NexMatchMeshLayerController::IsParticipated` (0x035d5d40) whether the host's station id (its PID) is in the gathering's participant list and fails the join (118-0502, monitoring result 0x6439) when it is not; that list is filled only by NEX 3001 participation events (`ProcessNotificationEvent` 0x035d661c, key = Param2). So the world gathering keeps the host PID among its participants and NEX's flag-0x800 recap names it to every joiner. Real Wii U 2026-11-10 20:46: probes complete in 60 ms, no connection request, `EndParticipation` 60 ms later | nex `titles/minecraft-wiiu/nex/world.go` (v1.19.8) |

## Tests built from the capture

`pia5.Joiner` (the joiner side of P3-P6, G1-G3) drives the host in `internal/relay/e2e_test.go` and `minecraft joinprobe`. It
shows the host and the capture's joiner agree with each other; it does not show that a console accepts the host. That needs a
real Wii U join (Quay or hardware) and is the open item of card #162.

## Unconfirmed (listed, not built)

- U1 Block ids above 174 and what the console draws for them; they are sent as air.
- U2 Tile modes 7, 9 and 15 (seen 48, 147 and 175 times) are not decoded; this encoder never writes them.
- U3 The console's single block change packet (the capture has packet 0x35 but its layout was not checked): Java block changes re-send the whole column.
- U4 Host-to-joiner teleport, entity spawn, entity move (A3/A4/A5/1C) and player list packets: no other players, mobs or items are shown.
- U5 Chat in either direction.
- U6 What the 18,521-byte sync packet contains besides the spawn position; it is replayed as captured.
- U7 Login response fields (game mode, difficulty, seed, entity id 237) are the capture's.
- U8 Disconnect packets and their reason codes, host keep-alive cadence after the first keep alive.
- U9 Client look direction (no rotation packet seen); Java gets yaw and pitch 0.
- U10 PS3 and Xbox 360 (captures in progress on card #163), Switch/PS4 Bedrock through Geyser (a later card).
