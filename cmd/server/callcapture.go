package main

// Call capture (-wa-call-capture <dir>): everything about one WhatsApp
// call, for offline analysis (cmd/videodiag) instead of a test call per
// question. Per call, <dir>/<time>-<call id>/:
//   media.pcap   every media packet from and to the relay, as on the wire
//                (SRTP), in fake IPv4/UDP (relay 10.66.0.1:3478, us
//                10.66.0.2:40000): Wireshark opens it, "Decode As" RTP
//   events.jsonl every call stanza in and out (full XML), and notes
//   keys.json    what the SRTP keys were derived from (CallKeyInfo)
// and for all calls <dir>/sip.log: the SIP with the phone's engine.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"wacalls/internal/voip/call"
	"wacalls/internal/voip/core"

	waBinary "go.mau.fi/whatsmeow/binary"
)

var captureDir string

type callCapture struct {
	mu      sync.Mutex
	dir     string
	start   time.Time
	pcap    *os.File
	events  *os.File
	in, out int
	cm      *call.CallManager
}

func newCallCapture(callID string) (*callCapture, error) {
	dir := filepath.Join(captureDir, time.Now().Format("20060102-150405")+"-"+callID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	pc, err := os.Create(filepath.Join(dir, "media.pcap"))
	if err != nil {
		return nil, err
	}
	ev, err := os.Create(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		pc.Close()
		return nil, err
	}
	// pcap global header: magic, v2.4, tz 0, sigfigs 0, snaplen, LINKTYPE_RAW
	var h [24]byte
	binary.LittleEndian.PutUint32(h[0:], 0xa1b2c3d4)
	binary.LittleEndian.PutUint16(h[4:], 2)
	binary.LittleEndian.PutUint16(h[6:], 4)
	binary.LittleEndian.PutUint32(h[16:], 65535)
	binary.LittleEndian.PutUint32(h[20:], 101)
	_, _ = pc.Write(h[:])
	return &callCapture{dir: dir, start: time.Now(), pcap: pc, events: ev}, nil
}

// media writes one packet (out: sent by us).
func (c *callCapture) media(out bool, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pcap == nil {
		return
	}
	src, dst := [4]byte{10, 66, 0, 1}, [4]byte{10, 66, 0, 2}
	sport, dport := uint16(3478), uint16(40000)
	if out {
		src, dst, sport, dport = dst, src, dport, sport
		c.out++
	} else {
		c.in++
	}
	n := 28 + len(data)
	pkt := make([]byte, 16+n)
	now := time.Now()
	binary.LittleEndian.PutUint32(pkt[0:], uint32(now.Unix()))
	binary.LittleEndian.PutUint32(pkt[4:], uint32(now.Nanosecond()/1000))
	binary.LittleEndian.PutUint32(pkt[8:], uint32(n))
	binary.LittleEndian.PutUint32(pkt[12:], uint32(n))
	ip := pkt[16:]
	ip[0], ip[8], ip[9] = 0x45, 64, 17
	binary.BigEndian.PutUint16(ip[2:], uint16(n))
	copy(ip[12:], src[:])
	copy(ip[16:], dst[:])
	var sum uint32
	for i := 0; i < 20; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(ip[i:]))
	}
	for sum > 0xffff {
		sum = sum&0xffff + sum>>16
	}
	binary.BigEndian.PutUint16(ip[10:], ^uint16(sum))
	binary.BigEndian.PutUint16(ip[20:], sport)
	binary.BigEndian.PutUint16(ip[22:], dport)
	binary.BigEndian.PutUint16(ip[24:], uint16(8+len(data)))
	copy(ip[28:], data)
	_, _ = c.pcap.Write(pkt)
	if !out && (c.in == 1 || c.in%500 == 0) {
		// Not here: media is the CallManager's OnRawMedia, which runs with
		// the manager's lock held on the send path, and KeyInfo takes that
		// lock (2026-11-03: a self-deadlock that froze every call and
		// WhatsApp node handling after a phone hangup).
		go c.writeKeys()
	}
}

// event records one stanza or note.
func (c *callCapture) event(dir, kind, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.events == nil {
		return
	}
	b, _ := json.Marshal(map[string]any{"t_ms": time.Since(c.start).Milliseconds(), "dir": dir, "kind": kind, "text": text})
	_, _ = c.events.Write(append(b, '\n'))
}

// writeKeys saves the call's keys. It takes the CallManager's lock (KeyInfo):
// never call it with c.mu held or from a CallManager callback.
func (c *callCapture) writeKeys() {
	if c.cm == nil {
		return
	}
	b, _ := json.MarshalIndent(c.cm.KeyInfo(), "", "  ")
	_ = os.WriteFile(filepath.Join(c.dir, "keys.json"), b, 0o600)
}

func (c *callCapture) close() string {
	c.writeKeys()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pcap == nil {
		return c.dir
	}
	b, _ := json.Marshal(map[string]any{"t_ms": time.Since(c.start).Milliseconds(), "dir": "-", "kind": "end",
		"text": fmt.Sprintf("packets in %d out %d", c.in, c.out)})
	_, _ = c.events.Write(append(b, '\n'))
	_ = c.pcap.Close()
	_ = c.events.Close()
	c.pcap, c.events = nil, nil
	return c.dir
}

// captureSocket records the stanzas the call manager sends.
type captureSocket struct {
	core.VoipSocket
	cap *callCapture
}

func (s captureSocket) SendNode(ctx context.Context, n waBinary.Node) error {
	s.cap.event("out", "stanza", n.String())
	return s.VoipSocket.SendNode(ctx, n)
}

func (s captureSocket) Query(ctx context.Context, n waBinary.Node) (*waBinary.Node, error) {
	s.cap.event("out", "query", n.String())
	r, err := s.VoipSocket.Query(ctx, n)
	if r != nil {
		s.cap.event("in", "query-result", r.String())
	}
	return r, err
}

// sipTrace (capture on): the SIP with the phone, all calls, one file.
var sipTraceMu sync.Mutex

func sipTrace(out bool, m *sipMsg) {
	if captureDir == "" || m == nil {
		return
	}
	sipTraceMu.Lock()
	defer sipTraceMu.Unlock()
	f, err := os.OpenFile(filepath.Join(captureDir, "sip.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	dir := "<- phone"
	if out {
		dir = "-> phone"
	}
	fmt.Fprintf(f, "==== %s %s\n%s\n", time.Now().Format("15:04:05.000"), dir, m.Bytes())
}
