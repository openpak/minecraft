# Changelog

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
