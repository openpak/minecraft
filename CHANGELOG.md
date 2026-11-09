# Changelog

## 0.1.2

- Fix: a deadlock on the first game message (the game callbacks ran under the host's lock while they send); found by the new
  end-to-end test, which joins a real Java 1.8.9 server with a joiner built from the capture's joiner messages.
- `minecraft joinprobe`: joins a running service the way a console does and counts what comes back (the key is read from a file).

## 0.1.1

- Station logging; the periodic RTT (0.2 s, then 2 s) and clock (1 s) traffic the console host sent in the capture.

## 0.1.0

- First release: Java 1.8.9 world server supervised in the container, the Wii U adapter (PIA GCM station, console session
  handshake, chunk codec) and the internal leg API. Players see the Java world's terrain; movement is forwarded to Java.
