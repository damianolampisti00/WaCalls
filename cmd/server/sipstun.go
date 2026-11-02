package main

// Minimal ICE connectivity checks (RFC 5389 STUN, RFC 8445 checks) for the
// phone's call engine. Its AnyFirewall engine (EBMService, disassembled
// 2026-11-02) marks a media component ready only on an ICE check: an
// incoming Binding Request it answers (0x129c56) or a Binding Response to
// its own (0x12abb8). We are not an ICE peer, so it sent none and waited
// ~2 s for a fallback, then counted our media only from then on, and
// started its audio only at its 5 s timer: "connecting" on the phone while
// the caller already talked. So we send it Binding Requests at the start
// of the media and answer any it sends.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"hash/crc32"
	"net"
)

const (
	stunMagic           = 0x2112A442
	stunBindingRequest  = 0x0001
	stunBindingSuccess  = 0x0101
	stunAttrMappedXOR   = 0x0020
	stunAttrUsername    = 0x0006
	stunAttrIntegrity   = 0x0008
	stunAttrFingerprint = 0x8028
	stunAttrPriority    = 0x0024
	stunAttrControlled  = 0x8029
	stunFingerprintXOR  = 0x5354554e
	stunLocalUfrag      = "wacalls" // ours, in the USERNAME of our checks
)

// isSTUN: a STUN message (two zero top bits, the magic cookie).
func isSTUN(p []byte) bool {
	return len(p) >= 20 && p[0]&0xc0 == 0 && binary.BigEndian.Uint32(p[4:]) == stunMagic &&
		int(binary.BigEndian.Uint16(p[2:]))+20 == len(p)
}

func stunType(p []byte) uint16 { return binary.BigEndian.Uint16(p[0:]) }

type stunMsg struct{ b []byte }

func newSTUN(typ uint16, tid []byte) *stunMsg {
	b := make([]byte, 20, 128)
	binary.BigEndian.PutUint16(b[0:], typ)
	binary.BigEndian.PutUint32(b[4:], stunMagic)
	copy(b[8:20], tid)
	return &stunMsg{b}
}

func (m *stunMsg) setLen(n int) { binary.BigEndian.PutUint16(m.b[2:], uint16(n)) }

func (m *stunMsg) add(typ uint16, v []byte) {
	var h [4]byte
	binary.BigEndian.PutUint16(h[0:], typ)
	binary.BigEndian.PutUint16(h[2:], uint16(len(v)))
	m.b = append(m.b, h[:]...)
	m.b = append(m.b, v...)
	for len(m.b)%4 != 0 {
		m.b = append(m.b, 0)
	}
	m.setLen(len(m.b) - 20)
}

// integrity adds MESSAGE-INTEGRITY (HMAC-SHA1 with the short-term key).
func (m *stunMsg) integrity(key string) {
	m.setLen(len(m.b) - 20 + 24) // the length covers the attribute itself
	mac := hmac.New(sha1.New, []byte(key))
	mac.Write(m.b)
	m.add(stunAttrIntegrity, mac.Sum(nil))
}

func (m *stunMsg) fingerprint() {
	m.setLen(len(m.b) - 20 + 8)
	var v [4]byte
	binary.BigEndian.PutUint32(v[:], crc32.ChecksumIEEE(m.b)^stunFingerprintXOR)
	m.add(stunAttrFingerprint, v[:])
}

// stunBindingRequestFor: our check toward the phone. With its ICE
// credentials (from its SDP) it is a proper ICE check (USERNAME
// "theirs:ours", MESSAGE-INTEGRITY with its password); without, a plain
// Binding Request.
func stunBindingRequestFor(ufrag, pwd string) []byte {
	tid := make([]byte, 12)
	_, _ = rand.Read(tid)
	m := newSTUN(stunBindingRequest, tid)
	if ufrag != "" {
		m.add(stunAttrUsername, []byte(ufrag+":"+stunLocalUfrag))
		var prio [4]byte
		binary.BigEndian.PutUint32(prio[:], 0x6e0001ff)
		m.add(stunAttrPriority, prio[:])
		tie := make([]byte, 8)
		_, _ = rand.Read(tie)
		m.add(stunAttrControlled, tie)
		if pwd != "" {
			m.integrity(pwd)
		}
	}
	m.fingerprint()
	return m.b
}

// stunBindingResponse answers the phone's Binding Request: success, with
// the address we saw it from.
func stunBindingResponse(req []byte, from *net.UDPAddr) []byte {
	m := newSTUN(stunBindingSuccess, req[8:20])
	ip := from.IP.To4()
	if ip == nil {
		ip = net.IPv4(127, 0, 0, 1).To4()
	}
	v := make([]byte, 8)
	v[1] = 0x01 // IPv4
	binary.BigEndian.PutUint16(v[2:], uint16(from.Port)^uint16(stunMagic>>16))
	binary.BigEndian.PutUint32(v[4:], binary.BigEndian.Uint32(ip)^stunMagic)
	m.add(stunAttrMappedXOR, v)
	m.fingerprint()
	return m.b
}
