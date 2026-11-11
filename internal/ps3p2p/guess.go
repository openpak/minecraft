package ps3p2p

import "time"

// Every value below is a GUESS: the spec (ps3-0184-p2p-transport-2026-11-11.md §2.2, §6) could not read it from the binaries, because the
// game sets no librudp option and the library's own defaults are not decoded. The first real capture (the PS3's SYN carries its MSS,
// its keepalive cadence and its retransmit timing show the rest) confirms or corrects each one; change them here and nowhere else.
const (
	// GuessMSS is the maximum segment size we put in our SYN block. The game's largest message is 4 + 1346 bytes and 1346 is a game
	// constant (EB 0xc8a0d4), which suggests the library MSS is at least 1350; the PS3's own SYN shows the real value.
	GuessMSS = 1350

	// GuessWindowByte is the window byte of every packet: 0xff = (0x3f) << 6 = 4032 (what the Terraria probe sends; the PS3 accepted it).
	GuessWindowByte = 0xff

	// GuessCapWord and GuessVer are the SYN block's capability word and the two version words (0x0100 / 0x0100, the only values the
	// Terraria PS3 accepted; the §26 decode says they must be 0x0100-compatible).
	GuessCapWord = 0x0000
	GuessVerA    = 0x0100
	GuessVerB    = 0x0100

	// GuessSynRetry is how often an unanswered SYN is repeated. A real library retransmits on its own timer; ours only has to be
	// as eager as the PS3's, which is unknown.
	GuessSynRetry = time.Second

	// GuessRTO is the data retransmission timeout (go-back-N: everything unacked is resent).
	GuessRTO = 500 * time.Millisecond

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
