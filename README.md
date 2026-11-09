# minecraft

OpenPak's one Minecraft (LCE) service for every console, with a stock Java Edition 1.8.9 server as the shared world. Consoles join
through adapters that speak their own session protocol and translate to the Java server as a client. Built clean-room from real
console captures (`docs/provenance.md`). Card openpak/prds#162; PS3/360 adapters follow with the captures on #163.

## State

- `internal/lcechunk`: the console chunk column format, read and written (verified on 400 chunks from a real Wii U).
- `internal/pia5`: PIA 5.7-5.10 packets (AES-GCM) and a host-side station for Minecraft: Wii U Edition (handshake, mesh, reliable).
- `internal/lce`: the game layer: hello, pre-login, login, world sync, chunk stream, position.
- `internal/javamc`: a Java Edition 1.8.9 client (login, compression, chunks, keep alive, position).
- `internal/javaserver`: runs the official server jar (fetched on first start, never shipped).
- Wii U only; PS3 and 360 plug in beside `internal/lce` once their captures exist.
- Not yet: other players and mobs, chat, console block-change packets (columns are re-sent), the stock friend-list listing.

## Run

`minecraft serve` (default). Ports: block 20270 (HTTP, internal), Wii U transport UDP 22960 (public). Environment:
`MINECRAFT_HTTP_ADDR` (:20270), `MINECRAFT_RELAY_ADDR` (:22960), `MINECRAFT_RELAY_PUBLIC` (the ip:port consoles are given),
`MINECRAFT_DATABASE_URL` (Postgres: `legs`, `sessions`), `MINECRAFT_INTERNAL_KEY` (bearer for `/internal/*`),
`MINECRAFT_JAVA_DIR` (/data, the world volume), `MINECRAFT_JAVA_XMX` (768m), `MINECRAFT_VIEW_DISTANCE` (6),
`MINECRAFT_JAVA_EXTERNAL=1` (use `MINECRAFT_JAVA_ADDR` instead of starting a server). Quadlet sample: `deploy/openpak-minecraft-core.container`.
The nex title `minecraft-wiiu` creates the world's gathering and registers its leg here every minute (`MINECRAFT_SERVICE_URL`).

## Build and test

    GOWORK=off go vet ./... && GOWORK=off go test ./...

`minecraft joinprobe -host ip:port -gathering N -keyfile F` joins a service like a console and prints the join order, chunk count
and whether the first chunk decodes (the verification of the deployed stack without a console).

Live checks: `MC_JAVA_ADDR=127.0.0.1:25565` (a running 1.8.9 server) and `MC_CAPTURE_DIR` (the Wii U capture) enable more tests.

License: AGPL-3.0-only.
