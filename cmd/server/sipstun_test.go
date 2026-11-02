package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/binary"
	"hash/crc32"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

// stunAttrs returns the attributes of a STUN message by type, checking the
// framing and the FINGERPRINT.
func stunAttrs(t *testing.T, p []byte) map[uint16][]byte {
	t.Helper()
	if !isSTUN(p) {
		t.Fatalf("not STUN: % x", p)
	}
	a := map[uint16][]byte{}
	for off := 20; off < len(p); {
		typ, n := binary.BigEndian.Uint16(p[off:]), int(binary.BigEndian.Uint16(p[off+2:]))
		if typ == stunAttrFingerprint {
			want := crc32.ChecksumIEEE(p[:off]) ^ stunFingerprintXOR
			if binary.BigEndian.Uint32(p[off+4:]) != want {
				t.Fatal("bad FINGERPRINT")
			}
		}
		a[typ] = p[off+4 : off+4+n]
		off += 4 + (n+3)&^3
	}
	return a
}

func TestSTUNBindingRequestWithAndWithoutICECredentials(t *testing.T) {
	plain := stunBindingRequestFor("", "")
	if stunType(plain) != stunBindingRequest || len(stunAttrs(t, plain)) != 1 {
		t.Fatalf("plain request % x", plain)
	}
	req := stunBindingRequestFor("eng1", "secretpw")
	a := stunAttrs(t, req)
	if string(a[stunAttrUsername]) != "eng1:wacalls" {
		t.Fatalf("USERNAME %q", a[stunAttrUsername])
	}
	// MESSAGE-INTEGRITY over the message up to it, length field covering it
	off := 20
	for binary.BigEndian.Uint16(req[off:]) != stunAttrIntegrity {
		off += 4 + (int(binary.BigEndian.Uint16(req[off+2:]))+3)&^3
	}
	head := append([]byte(nil), req[:off]...)
	binary.BigEndian.PutUint16(head[2:], uint16(off-20+24))
	mac := hmac.New(sha1.New, []byte("secretpw"))
	mac.Write(head)
	if !hmac.Equal(mac.Sum(nil), a[stunAttrIntegrity]) {
		t.Fatal("bad MESSAGE-INTEGRITY")
	}
}

func TestSTUNBindingResponseMapsTheSender(t *testing.T) {
	req := stunBindingRequestFor("", "")
	from := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 45536}
	resp := stunBindingResponse(req, from)
	if stunType(resp) != stunBindingSuccess || string(resp[8:20]) != string(req[8:20]) {
		t.Fatal("not a success response to this transaction")
	}
	x := stunAttrs(t, resp)[stunAttrMappedXOR]
	port := binary.BigEndian.Uint16(x[2:]) ^ uint16(stunMagic>>16)
	ip := binary.BigEndian.Uint32(x[4:]) ^ stunMagic
	if port != 45536 || ip != 0x7f000001 {
		t.Fatalf("XOR-MAPPED-ADDRESS %x:%d", ip, port)
	}
}

func TestSIPRTPSendsICEChecksAndAnswersThePhones(t *testing.T) {
	sent := make(chan struct {
		port int
		p    []byte
	}, 256)
	tr := newTunnelRTP(func(port int, p []byte) bool {
		sent <- struct {
			port int
			p    []byte
		}{port, append([]byte(nil), p...)}
		return true
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := newSIPRTP(tr, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 45536}, codecOpusWB, nil, log)
	r.rtcpPort = 45535
	r.start()
	defer r.close()
	ports := map[int]bool{}
	deadline := time.After(2 * time.Second)
	for len(ports) < 2 {
		select {
		case s := <-sent:
			if isSTUN(s.p) && stunType(s.p) == stunBindingRequest {
				ports[s.port] = true
			}
		case <-deadline:
			t.Fatalf("ICE checks sent to %v, want 45536 and 45535", ports)
		}
	}
	req := stunBindingRequestFor("", "")
	tr.deliver(45535, req)
	for {
		select {
		case s := <-sent:
			if isSTUN(s.p) && stunType(s.p) == stunBindingSuccess && s.port == 45535 && string(s.p[8:20]) == string(req[8:20]) {
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the phone's check was not answered")
		}
	}
}

// The engine's offer of 2026-11-02 23:21 (with a working STUN server).
func TestParseSDPHostCandidatesAndICECredentials(t *testing.T) {
	sdp := "v=0\r\no=Eyeball 1 1 IN IP4 151.44.14.220\r\ns=Eyeball AnyFirewall Engine\r\nc=IN IP4 151.44.14.220\r\nt=0 0\r\n" +
		"a=ice-pwd:GojpgnGna8JKv4cHfEta+77j/Eyeball\r\na=ice-ufrag:AFZZs1\r\nm=audio 13268 RTP/AVP 126 0 101\r\n" +
		"a=candidate:1 1 UDP 2130706431 192.168.8.149 45536 typ host\r\n" +
		"a=candidate:2 1 UDP 1694498815 151.44.14.220 13268 typ srflx raddr 192.168.8.149 rport 45536\r\n" +
		"a=rtcp:13269\r\na=candidate:1 2 UDP 2130706430 192.168.8.149 45535 typ host\r\n" +
		"a=candidate:2 2 UDP 1694498814 151.44.14.220 13269 typ srflx raddr 192.168.8.149 rport 45535\r\n" +
		"a=rtpmap:126 OPUS/16000\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:101 telephone-event/8000\r\na=ptime:20\r\n"
	s, err := parseSDP([]byte(sdp))
	if err != nil {
		t.Fatal(err)
	}
	if s.Port != 13268 || s.RTCPPort != 13269 || s.HostPort != 45536 || s.HostRTCPPort != 45535 {
		t.Fatalf("ports m=%d rtcp=%d host=%d/%d", s.Port, s.RTCPPort, s.HostPort, s.HostRTCPPort)
	}
	if s.Ufrag != "AFZZs1" || s.Pwd != "GojpgnGna8JKv4cHfEta+77j/Eyeball" {
		t.Fatalf("ice %q %q", s.Ufrag, s.Pwd)
	}
}
