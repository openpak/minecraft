// Package ps3p2p is the PS3 side of the Minecraft 01.84 transport: the kernel's encrypted UDPP2P datagram (kind 6), the librudp
// layer above it and the {len, kind} message layer of the game. Everything follows research/minecraft-lce/ps3-0184-p2p-transport-2026-11-11.md
// (static analysis, no packet of it was ever captured); the constants that are guesses are all in guess.go.
package ps3p2p

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/binary"
	"errors"
)

// Header is the UDPP2P header of a kind-6 (encrypted and signed) datagram with both vports 1: 0xff, 0x80|0x40|6, src vport 1, dst vport 1
// (spec §1.1, §1.4).
var Header = [4]byte{0xff, 0xc6, 0x01, 0x01}

const overhead = 12 // header, seed, 4-byte MAC

var ErrNotKind6 = errors.New("ps3p2p: not a compact kind-6 datagram")
var ErrMAC = errors.New("ps3p2p: MAC mismatch")

// Seal builds a kind-6 datagram: IV = seed x4, AES-128-CBC over the full blocks, the tail (len mod 16) XORed with E(last ciphertext
// block, or the IV when the message is shorter than a block), then HMAC-SHA1 over the ciphertext truncated to 4 bytes (spec §1.3-1.4).
// key is the PEER's skey: the receiver decrypts with its own.
func Seal(key []byte, seed uint32, plain []byte) ([]byte, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	iv := ivOf(seed)
	c := make([]byte, len(plain))
	full := len(plain) &^ 15
	prev := iv[:]
	if full > 0 {
		cipher.NewCBCEncrypter(blk, iv[:]).CryptBlocks(c[:full], plain[:full])
		prev = c[full-16 : full]
	}
	xorTail(blk, prev, c[full:], plain[full:])
	out := make([]byte, 0, overhead+len(c))
	out = append(out, Header[:]...)
	out = binary.BigEndian.AppendUint32(out, seed)
	out = append(out, mac(key, c)...)
	return append(out, c...), nil
}

// SealRandom is Seal with a fresh random seed (the kernel draws it from its PRNG table).
func SealRandom(key, plain []byte) ([]byte, error) {
	var b [4]byte
	rand.Read(b[:])
	return Seal(key, binary.BigEndian.Uint32(b[:]), plain)
}

// Open verifies and decrypts a kind-6 datagram with OUR OWN skey. On a MAC mismatch it still returns the plaintext it would have
// produced (with ErrMAC) so the recorder can show what a wrong key or a wrong guess made of it.
func Open(key, d []byte) (plain []byte, seed uint32, err error) {
	if len(d) < overhead || d[0] != 0xff || d[1]&0x8f != 0x86 || d[1]&0x40 == 0 {
		return nil, 0, ErrNotKind6
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, 0, err
	}
	seed = binary.BigEndian.Uint32(d[4:8])
	c := d[overhead:]
	iv := ivOf(seed)
	plain = make([]byte, len(c))
	full := len(c) &^ 15
	prev := iv[:]
	if full > 0 {
		cipher.NewCBCDecrypter(blk, iv[:]).CryptBlocks(plain[:full], c[:full])
		prev = c[full-16 : full]
	}
	xorTail(blk, prev, plain[full:], c[full:]) // the tail is a stream XOR with E(prev): the same operation both ways
	if subtle.ConstantTimeCompare(mac(key, c), d[8:12]) != 1 {
		return plain, seed, ErrMAC
	}
	return plain, seed, nil
}

func ivOf(seed uint32) (iv [16]byte) {
	for i := 0; i < 4; i++ {
		binary.BigEndian.PutUint32(iv[4*i:], seed)
	}
	return
}

func xorTail(blk cipher.Block, prev, dst, src []byte) {
	if len(src) == 0 {
		return
	}
	ks := make([]byte, 16)
	blk.Encrypt(ks, prev)
	for i := range src {
		dst[i] = src[i] ^ ks[i]
	}
}

func mac(key, c []byte) []byte {
	h := hmac.New(sha1.New, key)
	h.Write(c)
	return h.Sum(nil)[:4]
}
