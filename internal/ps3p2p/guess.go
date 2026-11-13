package ps3p2p

import "time"

// Seen on hardware (run live10, 2026-11-13: a real PS3's SYN and RESET, `ffc3 0101 8014 0001 80 30 3d4c 0201 0100 0100 f8ba92b1 0582` and
// `ffc3 0101 800e 0001 c0 00 0000 0003 f8ba92b1`, and librudp.prx 4.93; matching2 note §43, transport note §8). These were guesses until then.
const (
	// MSS is librudp's default (0x582 = 1410): the console's SYN carries it, nothing sets an option (R 0x194c0, R 0x1a7b4). The usable
	// payload per segment is MSS-0x40 = 1346 = MaxPiece, which is where the game's 0x542 comes from. A peer's MSS is clamped to [0x40, MSS].
	MSS = 0x582

	// WindowByte is the window byte of every packet the console sends: 0x30 = 48 segments (`(b&0x3f) << ((b>>5)&6)`, R 0xd3b0).
	WindowByte = 0x30

	// CapWord is the capability word of the console's SYN block. Bits 0x800 / 0x200 / 0x100 are the sender's flags and the receiver silently
	// aborts (reason 5 or 6, no packet) unless they equal its own (R 0xd318), so a peer mirrors the console's word once it has seen one:
	// Conn does. The low nibble (1) is copied as seen; nothing reads it.
	CapWord = 0x0201

	// VerA and VerB are both 0x0100 on the wire; the receiver needs verA >= 0x0100 and verB <= 0x0100 (reason 4 otherwise).
	VerA = 0x0100
	VerB = 0x0100

	// SynRetryFirst and SynRetryMax are the retransmit timer of an unanswered SYN (and of a SYN+ACK): 1 s, doubling, capped at 16 s.
	// Seen: SYNs 1, 2, 4, 8 s apart, then a RESET (reason 3, R 0xf560 -> abort R 0xb44c -> sender R 0xb280) after the 30 s connect timeout; ctx+0xa8 = 1000000 us at
	// start, cap 0xf42400 us (R 0xd778).
	SynRetryFirst = time.Second
	SynRetryMax   = 16 * time.Second
)

// Still a GUESS: the values below were not read from the binaries or seen on the wire. The first captured data exchange confirms or
// corrects each one; change them here and nowhere else.
const (
	// GuessRTO is the data retransmission timeout (go-back-N: everything unacked is resent).
	GuessRTO = 500 * time.Millisecond

	// GuessRTOMax caps the doubling of the retransmission timeout while the peer acks nothing, and GuessDeadAfter is how long data may go
	// unacknowledged before the connection is declared dead (librudp's own connect timeout is 30 s, seen: RESET reason 3).
	GuessRTOMax    = 8 * time.Second
	GuessDeadAfter = 30 * time.Second

	// GuessKeepalive is the idle interval after which we send an empty ack so the PS3's idle timer does not expire. Its real idle
	// timeout is unknown; a few seconds is safe either way.
	GuessKeepalive = 2 * time.Second

	// GuessEmptyAckIsType0 says an empty ack is a DATA packet (type 0) with the ack flag, `20 ff <seq> <ack>`, that does not consume a
	// sequence number (the form Terraria's probe completes the handshake with). Type 1 ("ack/keepalive" in §26) is accepted on input.
	GuessEmptyAckIsType0 = true

	// GuessKind2Counted: we send every game message as kind 2 (acked) when true, kind 1 otherwise. Which game packets are kind 1 or 2 is
	// not known (§2.3); a PS3 acks kind 2 either way, kind 1 needs no ack, so kind 2 only costs a few {0,3} replies.
	GuessKind2Counted = true

	// GuessHelloPad is what the three bytes after the small id of the 32-byte hello hold (§3.1 step 9); the host reads byte 0 only.
	GuessHelloPad = 0
)

// GuessSendWindow is how many unacknowledged data packets we keep in flight (the PS3's real window is in its window byte; we ignore it).
const GuessSendWindow = 16
