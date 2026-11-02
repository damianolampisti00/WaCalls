package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- unit tests ------------------------------------------------------------

func TestParseSIPStreamAndCompactHeaders(t *testing.T) {
	raw := "INVITE sip:393331234567@wacalls.local SIP/2.0\r\n" +
		"v: SIP/2.0/TCP 127.0.0.1:5000;branch=z9hG4bKa, SIP/2.0/TCP 10.0.0.1;branch=z9hG4bKb\r\n" +
		"f: \"me\" <sip:wacalls@wacalls.local>;tag=1\r\nt: <sip:393331234567@wacalls.local>\r\n" +
		"i: abc@x\r\nCSeq: 7 INVITE\r\nl: 5\r\n\r\nv=0\r\n" +
		"SIP/2.0 200 OK\r\nCall-ID: z\r\nContent-Length: 0\r\n\r\n"
	m, n, err := parseSIP([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.Method != "INVITE" || sipURIUser(m.URI) != "393331234567" || string(m.Body) != "v=0\r\n" {
		t.Fatalf("bad request: %+v body %q", m, m.Body)
	}
	if v := m.All("via"); len(v) != 2 || !strings.Contains(v[1], "10.0.0.1") {
		t.Fatalf("via = %q", v)
	}
	if sipParam(m.Get("from"), "tag") != "1" || m.Get("call-id") != "abc@x" {
		t.Fatalf("headers: %+v", m.Headers)
	}
	if num, meth := m.CSeq(); num != 7 || meth != "INVITE" {
		t.Fatalf("cseq %d %s", num, meth)
	}
	r, _, err := parseSIP([]byte(raw[n:]))
	if err != nil || r.Code != 200 || r.Reason != "OK" {
		t.Fatalf("second message: %+v %v", r, err)
	}
	if _, _, err := parseSIP([]byte(raw[:20])); err != errSIPIncomplete {
		t.Fatalf("partial: %v", err)
	}
}

func TestSIPResponseKeepsDialogHeaders(t *testing.T) {
	req, _, _ := parseSIP([]byte("BYE sip:a@b SIP/2.0\r\nVia: SIP/2.0/TCP h;branch=z9hG4bKx\r\n" +
		"From: <sip:a@b>;tag=f\r\nTo: <sip:c@d>\r\nCall-ID: id1\r\nCSeq: 2 BYE\r\n\r\n"))
	r := sipResponse(req, 200, "OK", "tt")
	back, _, err := parseSIP(r.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if back.Get("via") != "SIP/2.0/TCP h;branch=z9hG4bKx" || sipParam(back.Get("to"), "tag") != "tt" ||
		back.Get("call-id") != "id1" || back.Get("cseq") != "2 BYE" {
		t.Fatalf("response: %s", r.Bytes())
	}
}

func TestULawRoundTrip(t *testing.T) {
	for _, v := range []int16{0, 1, -1, 100, -100, 1000, -1000, 8000, -8000, 32000, -32000} {
		got := ulawDecode(ulawEncode(v))
		tol := int(math.Abs(float64(v)))/16 + 8
		if d := int(got) - int(v); d > tol || d < -tol {
			t.Errorf("ulaw(%d) -> %d", v, got)
		}
	}
	// the well-known encoding of silence
	if ulawEncode(0) != 0xff {
		t.Errorf("silence encodes to %#x", ulawEncode(0))
	}
}

func rms(x []float32) float64 {
	var s float64
	for _, v := range x {
		s += float64(v) * float64(v)
	}
	return math.Sqrt(s / float64(len(x)))
}

func sipTone(freq float64, rate, n int) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(0.5 * math.Sin(2*math.Pi*freq*float64(i)/float64(rate)))
	}
	return out
}

func TestResamplersPassVoiceAndCutAliases(t *testing.T) {
	var d downsampler
	// in 20 ms blocks, like the call core: state must carry over
	var down []float32
	in := sipTone(1000, 16000, 16000)
	for i := 0; i < len(in); i += 320 {
		down = append(down, d.process(in[i:i+320])...)
	}
	if len(down) != 8000 {
		t.Fatalf("16k->8k gave %d samples", len(down))
	}
	if r := rms(down[400:]); r < 0.33 || r > 0.38 { // 0.5/sqrt2 = 0.354
		t.Errorf("1 kHz after downsampling: rms %.3f", r)
	}
	var d2 downsampler
	if r := rms(d2.process(sipTone(6000, 16000, 16000))[400:]); r > 0.03 {
		t.Errorf("6 kHz should be filtered out before decimation, rms %.3f", r)
	}
	var u upsampler
	up := u.process(sipTone(1000, 8000, 8000))
	if len(up) != 16000 {
		t.Fatalf("8k->16k gave %d samples", len(up))
	}
	if r := rms(up[800:]); r < 0.33 || r > 0.38 {
		t.Errorf("1 kHz after upsampling: rms %.3f", r)
	}
}

func TestParseSDPFromTheBBMVideoEngine(t *testing.T) {
	// The answer EBMService gave on 2026-11-02.
	body := "v=0\r\no=Eyeball 1435777689 4062234343 IN IP4 192.168.1.72\r\nc=IN IP4 192.168.1.72\r\n" +
		"m=audio 45536 RTP/AVP 0\r\nb=RS:0\r\nb=RR:0\r\na=rtpmap:0 PCMU/8000\r\na=ptime:20\r\n" +
		"m=video 45538 RTP/AVP 109\r\nb=RS:0\r\nb=RR:0\r\na=rtpmap:109 H264/90000\r\n"
	s, err := parseSDP([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if s.IP != "192.168.1.72" || s.Port != 45536 || !s.hasPCMU() || !s.Video || s.RTPMap[0] != "PCMU/8000" {
		t.Fatalf("%+v", s)
	}
	ans, _ := parseSDP(buildSDP("10.0.0.1", 40000, 1, true))
	if ans.Port != 40000 || ans.IP != "10.0.0.1" || ans.Video {
		t.Fatalf("our answer: %+v", ans)
	}
}

func TestRTPPayloadSkipsHeaderExtensions(t *testing.T) {
	p := []byte{0x90, 0x00, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0xbe, 0xde, 0, 1, 1, 2, 3, 4, 0xaa, 0xbb}
	pl, pt, ok := rtpPayload(p)
	if !ok || pt != 0 || string(pl) != "\xaa\xbb" {
		t.Fatalf("%x %d %v", pl, pt, ok)
	}
	if _, _, ok := rtpPayload([]byte{0x80, 0xc8, 0, 6, 0, 0, 0, 0, 0, 0, 0, 0}); ok {
		t.Fatal("RTCP sender report taken for audio")
	}
}

// ---- the gateway against a fake phone --------------------------------------

type fakeBackend struct {
	mu       sync.Mutex
	started  []string
	accepted []string
	rejected []string
	hungUp   []string
	attached map[string]mediaBridge
	fed      int
	fedLevel float64
}

func (f *fakeBackend) SessionID() string          { return "s1" }
func (f *fakeBackend) PhoneOf(peer string) string { return "+" + strings.Split(peer, "@")[0] }
func (f *fakeBackend) lock() func()               { f.mu.Lock(); return f.mu.Unlock }
func (f *fakeBackend) Reject(_ context.Context, id string) {
	defer f.lock()()
	f.rejected = append(f.rejected, id)
}
func (f *fakeBackend) Hangup(_ context.Context, id string) {
	defer f.lock()()
	f.hungUp = append(f.hungUp, id)
}
func (f *fakeBackend) StartCall(_ context.Context, phone string) (string, error) {
	defer f.lock()()
	f.started = append(f.started, phone)
	return "wa-out-1", nil
}
func (f *fakeBackend) Accept(_ context.Context, id string) error {
	defer f.lock()()
	f.accepted = append(f.accepted, id)
	return nil
}
func (f *fakeBackend) Attach(id string, b mediaBridge) {
	defer f.lock()()
	if f.attached == nil {
		f.attached = map[string]mediaBridge{}
	}
	f.attached[id] = b
}
func (f *fakeBackend) Feed(_ string, pcm []float32) {
	defer f.lock()()
	f.fed += len(pcm)
	if r := rms(pcm); r > f.fedLevel {
		f.fedLevel = r
	}
}

type fakePhone struct {
	t    *testing.T
	conn net.Conn
	buf  []byte
	rtp  *net.UDPConn
}

func (p *fakePhone) send(s string) {
	if _, err := p.conn.Write([]byte(s)); err != nil {
		p.t.Fatal(err)
	}
}

func (p *fakePhone) sendMsg(m *sipMsg) { p.send(string(m.Bytes())) }

func (p *fakePhone) read() *sipMsg {
	p.t.Helper()
	tmp := make([]byte, 8192)
	deadline := time.Now().Add(3 * time.Second)
	for {
		for len(p.buf) >= 2 && p.buf[0] == '\r' && p.buf[1] == '\n' { // keepalive pong
			p.buf = p.buf[2:]
		}
		if m, n, err := parseSIP(p.buf); err == nil {
			p.buf = p.buf[n:]
			return m
		}
		_ = p.conn.SetReadDeadline(deadline)
		n, err := p.conn.Read(tmp)
		if err != nil {
			p.t.Fatalf("phone read: %v", err)
		}
		p.buf = append(p.buf, tmp[:n]...)
	}
}

func (p *fakePhone) sdp() string {
	port := p.rtp.LocalAddr().(*net.UDPAddr).Port
	return fmt.Sprintf("v=0\r\no=Eyeball 1 1 IN IP4 127.0.0.1\r\nc=IN IP4 127.0.0.1\r\n"+
		"m=audio %d RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\nm=video %d RTP/AVP 109\r\na=rtpmap:109 H264/90000\r\n", port, port+2)
}

// readRTP waits for one of the gateway's audio packets (probes skipped).
func (p *fakePhone) readRTP() ([]byte, *net.UDPAddr) {
	p.t.Helper()
	for {
		buf := make([]byte, 1500)
		_ = p.rtp.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, from, err := p.rtp.ReadFromUDP(buf)
		if err != nil {
			p.t.Fatalf("no RTP from the gateway: %v", err)
		}
		if !isProbe(buf[:n]) {
			return buf[:n], from
		}
	}
}

func startSIPTest(t *testing.T, opts ...func(*sipConfig)) (*sipGateway, *fakeBackend, *Broker, *fakePhone) {
	t.Helper()
	be := &fakeBackend{}
	broker := NewBroker()
	cfg := sipConfig{MediaIP: "127.0.0.1", RTPLo: 41000, RTPHi: 41099}
	for _, o := range opts {
		o(&cfg)
	}
	gw := newSIPGateway(be, broker, cfg, slog.Default())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go gw.serve(ctx, ln)
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	u, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.Close() })
	p := &fakePhone{t: t, conn: c, rtp: u}
	// The engine's REGISTER as seen on 2026-11-02 (shortened).
	p.send("REGISTER sip:wacalls.local SIP/2.0\r\nVia: SIP/2.0/TCP 127.0.0.1:18724;rport;branch=z9hG4bK1;alias\r\n" +
		"From: \"wacalls\" <sip:wacalls@wacalls.local>;tag=1\r\nTo: \"wacalls\" <sip:wacalls@wacalls.local>\r\n" +
		"Call-ID: reg1@127.0.0.1\r\nCSeq: 1 REGISTER\r\n" +
		"Contact: \"wacalls\" <sip:wacalls@127.0.0.1:18724;transport=tcp;pin=2AFD07B0>\r\nExpires: 43200\r\n" +
		"User-Agent: Video Chat Client 3.3.0\r\nContent-Length: 0\r\n\r\n")
	if r := p.read(); r.Code != 200 || r.Get("expires") != "43200" {
		t.Fatalf("REGISTER answer: %s", r.Bytes())
	}
	if !gw.registered() {
		t.Fatal("phone not registered")
	}
	// keepalive ping/pong
	p.send("\r\n\r\n")
	return gw, be, broker, p
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 150; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSIPGatewayIncomingCallAnsweredAndHungUpByPhone(t *testing.T) {
	_, be, broker, p := startSIPTest(t)
	broker.upsertCall(CallRecord{SessionID: "s1", CallID: "wa1", Direction: "inbound", Status: StatusRinging})
	broker.emitIncoming("s1", "wa1", "393331234567@s.whatsapp.net")

	inv := p.read()
	if inv.Method != "INVITE" || !strings.Contains(inv.Get("from"), "\"+393331234567\" <sip:393331234567@") {
		t.Fatalf("INVITE: %s", inv.Bytes())
	}
	offer, err := parseSDP(inv.Body)
	if err != nil || !offer.hasPCMU() || offer.IP != "127.0.0.1" {
		t.Fatalf("offer %+v %v", offer, err)
	}
	p.sendMsg(sipResponse(inv, 180, "Ringing", "ph1"))
	ok := sipResponse(inv, 200, "OK", "ph1")
	ok.Add("Contact", "<sip:wacalls@127.0.0.1:18724;transport=tcp>")
	ok.Add("Content-Type", "application/sdp")
	ok.Body = []byte(p.sdp())
	p.sendMsg(ok)
	if ack := p.read(); ack.Method != "ACK" || sipParam(ack.Get("to"), "tag") != "ph1" {
		t.Fatalf("ACK: %s", ack.Bytes())
	}
	waitFor(t, "accept", func() bool { defer be.lock()(); return len(be.accepted) == 1 && be.attached["wa1"] != nil })

	// downlink: the peer's voice reaches the phone as PCMU
	be.mu.Lock()
	br := be.attached["wa1"]
	be.mu.Unlock()
	for i := 0; i < 10; i++ {
		_ = br.WritePCM(sipTone(800, 16000, 960))
	}
	var gotVoice bool
	var gwAddr *net.UDPAddr
	for i := 0; i < 30 && !gotVoice; i++ {
		pkt, from := p.readRTP()
		gwAddr = from
		if pkt[1]&0x7f != 0 || len(pkt) != 12+160 {
			t.Fatalf("bad RTP packet % x", pkt[:12])
		}
		for _, b := range pkt[12:] {
			if b != 0xff && b != 0x7f {
				gotVoice = true
			}
		}
	}
	if !gotVoice {
		t.Fatal("only silence reached the phone")
	}
	// uplink: the phone's mic reaches the call core
	for i := 0; i < 20; i++ {
		pkt := make([]byte, 12+160)
		pkt[0] = 0x80
		binary.BigEndian.PutUint16(pkt[2:], uint16(i))
		binary.BigEndian.PutUint32(pkt[4:], uint32(i*160))
		for k, v := range sipTone(500, 8000, 160) {
			pkt[12+k] = ulawEncode(floatToInt16(v))
		}
		_, _ = p.rtp.WriteToUDP(pkt, gwAddr)
		time.Sleep(20 * time.Millisecond)
	}
	waitFor(t, "uplink audio", func() bool { defer be.lock()(); return be.fed >= 3200 && be.fedLevel > 0.2 })

	bye := &sipMsg{Method: "BYE", URI: "sip:x@127.0.0.1"}
	bye.Add("Via", "SIP/2.0/TCP 127.0.0.1:18724;branch=z9hG4bKbye")
	bye.Add("From", ok.Get("to"))
	bye.Add("To", inv.Get("from"))
	bye.Add("Call-ID", inv.Get("call-id"))
	bye.Add("CSeq", "2 BYE")
	p.sendMsg(bye)
	if r := p.read(); r.Code != 200 {
		t.Fatalf("BYE answer: %s", r.Bytes())
	}
	waitFor(t, "hangup", func() bool { defer be.lock()(); return len(be.hungUp) == 1 && be.hungUp[0] == "wa1" })
}

func TestSIPGatewayCallerGivesUpWhileRinging(t *testing.T) {
	_, be, broker, p := startSIPTest(t)
	broker.upsertCall(CallRecord{SessionID: "s1", CallID: "wa2", Direction: "inbound", Status: StatusRinging})
	broker.emitIncoming("s1", "wa2", "393330000000@s.whatsapp.net")
	inv := p.read()
	broker.endCall("wa2", "timeout")
	cancel := p.read()
	if cancel.Method != "CANCEL" || cancel.Get("call-id") != inv.Get("call-id") || cancel.Get("via") != inv.Get("via") {
		t.Fatalf("CANCEL: %s", cancel.Bytes())
	}
	p.sendMsg(sipResponse(cancel, 200, "OK", "ph2"))
	p.sendMsg(sipResponse(inv, 487, "Request Terminated", "ph2"))
	if ack := p.read(); ack.Method != "ACK" {
		t.Fatalf("ACK: %s", ack.Bytes())
	}
	time.Sleep(50 * time.Millisecond)
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.rejected) != 0 || len(be.accepted) != 0 {
		t.Fatalf("WhatsApp side touched after the caller left: %+v", be)
	}
}

func TestSIPGatewayPhoneDeclines(t *testing.T) {
	_, be, broker, p := startSIPTest(t)
	broker.upsertCall(CallRecord{SessionID: "s1", CallID: "wa3", Direction: "inbound", Status: StatusRinging})
	broker.emitIncoming("s1", "wa3", "39333@s.whatsapp.net")
	inv := p.read()
	p.sendMsg(sipResponse(inv, 486, "Busy Here", "ph3"))
	if ack := p.read(); ack.Method != "ACK" {
		t.Fatalf("ACK: %s", ack.Bytes())
	}
	waitFor(t, "reject", func() bool { defer be.lock()(); return len(be.rejected) == 1 && be.rejected[0] == "wa3" })
}

func TestSIPGatewayPhoneCallsOut(t *testing.T) {
	_, be, broker, p := startSIPTest(t)
	inv := &sipMsg{Method: "INVITE", URI: "sip:+39%20333%20123%204567@wacalls.local"}
	inv.Add("Via", "SIP/2.0/TCP 127.0.0.1:18724;branch=z9hG4bKinv")
	inv.Add("From", "\"wacalls\" <sip:wacalls@wacalls.local>;tag=me")
	inv.Add("To", "<sip:393331234567@wacalls.local>")
	inv.Add("Call-ID", "out1@127.0.0.1")
	inv.Add("CSeq", "1 INVITE")
	inv.Add("Contact", "<sip:wacalls@127.0.0.1:18724;transport=tcp>")
	inv.Add("Content-Type", "application/sdp")
	inv.Body = []byte(p.sdp())
	p.sendMsg(inv)
	if r := p.read(); r.Code != 100 {
		t.Fatalf("want 100: %s", r.Bytes())
	}
	if r := p.read(); r.Code != 180 {
		t.Fatalf("want 180: %s", r.Bytes())
	}
	waitFor(t, "start", func() bool { defer be.lock()(); return len(be.started) == 1 })
	if be.started[0] != "393331234567" {
		t.Fatalf("dialled %q", be.started[0])
	}
	broker.upsertCall(CallRecord{SessionID: "s1", CallID: "wa-out-1", Direction: "outbound", Status: StatusConnected})
	ok := p.read()
	if ok.Code != 200 || ok.Get("content-type") != "application/sdp" {
		t.Fatalf("want 200 with SDP: %s", ok.Bytes())
	}
	ans, err := parseSDP(ok.Body)
	if err != nil || !ans.hasPCMU() || !strings.Contains(string(ok.Body), "m=video 0 ") {
		t.Fatalf("answer must keep audio and refuse video: %s", ok.Body)
	}
	p.readRTP() // media flows
	broker.endCall("wa-out-1", "user_ended")
	if bye := p.read(); bye.Method != "BYE" || sipParam(bye.Get("to"), "tag") != "me" ||
		sipParam(bye.Get("from"), "tag") != sipParam(ok.Get("to"), "tag") {
		t.Fatalf("BYE: %s", bye.Bytes())
	}
}

func TestSIPGatewayRefusesOutsiders(t *testing.T) {
	allow, _ := parseCIDRs("192.168.0.0/16,10.1.2.3")
	gw := newSIPGateway(&fakeBackend{}, NewBroker(), sipConfig{Allow: allow}, slog.Default())
	if gw.allowed(&net.TCPAddr{IP: net.IPv4(8, 8, 8, 8)}) || !gw.allowed(&net.TCPAddr{IP: net.IPv4(10, 1, 2, 3)}) ||
		!gw.allowed(&net.TCPAddr{IP: net.IPv4(192, 168, 1, 72)}) {
		t.Fatal("allow list")
	}
}

func TestTunnelRegistrationSurvivesRestart(t *testing.T) {
	f := filepath.Join(t.TempDir(), "sip_tunnel.json")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	g := newSIPGateway(nil, nil, sipConfig{StateFile: f}, log)
	g.saveTunnelState("sip:wacalls@127.0.0.1:5000;transport=tcp", "<sip:wacalls@wacalls.local>", time.Now().Add(-time.Minute))

	g2 := newSIPGateway(nil, nil, sipConfig{StateFile: f}, log)
	if g2.tunnelContact != "sip:wacalls@127.0.0.1:5000;transport=tcp" || g2.tunnelAOR != "<sip:wacalls@wacalls.local>" {
		t.Fatalf("restored %q %q", g2.tunnelContact, g2.tunnelAOR)
	}

	// Older than the lifetime we grant: the engine has re-registered or is gone.
	g.saveTunnelState("sip:old@127.0.0.1:1", "<sip:old@x>", time.Now().Add(-(sipTunnelExpires+1)*time.Second))
	if g3 := newSIPGateway(nil, nil, sipConfig{StateFile: f}, log); g3.tunnelContact != "" {
		t.Fatalf("expired registration restored: %q", g3.tunnelContact)
	}

	g.saveTunnelState("", "", time.Time{})
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Fatalf("state file not removed: %v", err)
	}
}

func TestPickCodecPrefersTheEnginesOpusWB(t *testing.T) {
	eng, err := parseSDP([]byte("v=0\r\nc=IN IP4 192.168.1.72\r\nm=audio 45536 RTP/AVP 126 0\r\n" +
		"a=rtpmap:126 OPUS/16000\r\na=rtpmap:0 PCMU/8000\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := eng.pickCodec(true); !ok || c != codecOpusWB {
		t.Fatalf("opus allowed: %v %v", c, ok)
	}
	if c, ok := eng.pickCodec(false); !ok || c != codecPCMU {
		t.Fatalf("opus off: %v %v", c, ok)
	}
	std, _ := parseSDP([]byte("v=0\r\nc=IN IP4 1.2.3.4\r\nm=audio 5000 RTP/AVP 111\r\na=rtpmap:111 opus/48000/2\r\n"))
	if c, ok := std.pickCodec(true); !ok || c != (sipCodec{PT: 111, Opus: true, Clock: 48000}) {
		t.Fatalf("RFC 7587 opus: %v %v", c, ok)
	}
	speex, _ := parseSDP([]byte("v=0\r\nc=IN IP4 1.2.3.4\r\nm=audio 5000 RTP/AVP 102\r\na=rtpmap:102 SPEEX/16000\r\n"))
	if _, ok := speex.pickCodec(true); ok {
		t.Fatal("speex only: no codec of ours")
	}
}

func TestBuildSDPOffersOpusThenPCMU(t *testing.T) {
	b := buildSDP("127.0.0.1", 40000, 7, false, codecOpusWB, codecPCMU)
	for _, want := range []string{"m=audio 40000 RTP/AVP 126 0\r\n", "a=rtpmap:126 OPUS/16000\r\n", "a=rtpmap:0 PCMU/8000\r\n"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %q in\n%s", want, b)
		}
	}
	s, _ := parseSDP(b)
	if c, ok := s.pickCodec(true); !ok || c != codecOpusWB {
		t.Fatalf("our own offer read back: %v %v", c, ok)
	}
}

func TestSIPRTPOpusRoundTrip(t *testing.T) {
	sent := make(chan []byte, 256)
	tr := newTunnelRTP(func(_ int, p []byte) bool {
		sent <- append([]byte(nil), p...)
		return true
	})
	got := make(chan int, 16)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := newSIPRTP(tr, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 45536}, codecOpusWB,
		func(pcm []float32) { got <- len(pcm) }, log)
	r.start()
	defer r.close()
	tone := make([]float32, 16000*400/1000)
	for i := range tone {
		tone[i] = 0.3 * float32(math.Sin(2*math.Pi*440*float64(i)/16000))
	}
	_ = r.WritePCM(tone)

	dec, err := newBBOpusDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer dec.close()
	var prevTS uint32
	var first []byte
	for n := 0; n < 6; n++ {
		var p []byte
		for p == nil || isProbe(p) {
			select {
			case p = <-sent:
			case <-time.After(2 * time.Second):
				t.Fatal("no RTP sent")
			}
		}
		if pt := int(p[1] & 0x7f); pt != 126 {
			t.Fatalf("payload type %d", pt)
		}
		ts := binary.BigEndian.Uint32(p[4:])
		if n > 0 && ts-prevTS != 320 {
			t.Fatalf("timestamp step %d, want 320 (20 ms at 16 kHz)", ts-prevTS)
		}
		prevTS = ts
		pcm, err := dec.decode(p[12:])
		if err != nil || len(pcm) != 320 {
			t.Fatalf("decode: %d samples, %v", len(pcm), err)
		}
		if first == nil {
			first = p
		}
	}
	if len(first)-12 > 120 {
		t.Fatalf("opus packet of %d bytes: not the ~60 of 24 kbit/s", len(first)-12)
	}
	tr.deliver(45536, first) // the phone's packets decode the same way
	select {
	case n := <-got:
		if n != 320 {
			t.Fatalf("uplink %d samples", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("received Opus not decoded")
	}
}

func opusDelayed(c *sipConfig) { c.Opus, c.DelayedOffer = true, true }

// engineOffer: the engine's own SDP offer as on 2026-11-02 (OPUS-WB first).
func (p *fakePhone) engineOffer() string {
	port := p.rtp.LocalAddr().(*net.UDPAddr).Port
	return fmt.Sprintf("v=0\r\no=Eyeball 1 1 IN IP4 127.0.0.1\r\nc=IN IP4 127.0.0.1\r\n"+
		"m=audio %d RTP/AVP 126 0 101\r\na=rtpmap:126 OPUS/16000\r\na=rtpmap:0 PCMU/8000\r\n"+
		"a=rtpmap:101 telephone-event/8000\r\n", port)
}

func TestSIPGatewayDelayedOfferGetsOpus(t *testing.T) {
	_, be, broker, p := startSIPTest(t, opusDelayed)
	broker.upsertCall(CallRecord{SessionID: "s1", CallID: "wa1", Direction: "inbound", Status: StatusRinging})
	broker.emitIncoming("s1", "wa1", "393331234567@s.whatsapp.net")

	inv := p.read()
	if inv.Method != "INVITE" || len(inv.Body) != 0 || inv.Get("content-type") != "" {
		t.Fatalf("INVITE should carry no SDP: %s", inv.Bytes())
	}
	ok := sipResponse(inv, 200, "OK", "ph1")
	ok.Add("Contact", "<sip:wacalls@127.0.0.1:18724;transport=tcp>")
	ok.Add("Content-Type", "application/sdp")
	ok.Body = []byte(p.engineOffer())
	p.sendMsg(ok)
	ack := p.read()
	if ack.Method != "ACK" || ack.Get("cseq") != "1 ACK" {
		t.Fatalf("ACK: %s", ack.Bytes())
	}
	ans, err := parseSDP(ack.Body)
	if err != nil || len(ans.Formats) != 1 || ans.Formats[0] != 126 || ans.RTPMap[126] != "OPUS/16000" {
		t.Fatalf("our answer in the ACK: %+v %v\n%s", ans, err, ack.Body)
	}
	waitFor(t, "accept", func() bool { defer be.lock()(); return len(be.accepted) == 1 && be.attached["wa1"] != nil })
	be.mu.Lock()
	br := be.attached["wa1"]
	be.mu.Unlock()
	for i := 0; i < 10; i++ {
		_ = br.WritePCM(sipTone(800, 16000, 960))
	}
	if pkt, _ := p.readRTP(); pkt[1]&0x7f != 126 {
		t.Fatalf("downlink payload type %d, want Opus 126", pkt[1]&0x7f)
	}
}

func TestSIPGatewayDelayedOfferRefusedRingsAgainWithOffer(t *testing.T) {
	_, be, broker, p := startSIPTest(t, opusDelayed)
	broker.upsertCall(CallRecord{SessionID: "s1", CallID: "wa1", Direction: "inbound", Status: StatusRinging})
	broker.emitIncoming("s1", "wa1", "393331234567@s.whatsapp.net")

	inv := p.read()
	p.sendMsg(sipResponse(inv, 488, "Not Acceptable Here", "ph1"))
	if ack := p.read(); ack.Method != "ACK" || ack.Get("cseq") != "1 ACK" {
		t.Fatalf("ACK of the 488: %s", ack.Bytes())
	}
	inv2 := p.read()
	if inv2.Method != "INVITE" || inv2.Get("cseq") != "2 INVITE" || inv2.Get("call-id") != inv.Get("call-id") {
		t.Fatalf("second INVITE: %s", inv2.Bytes())
	}
	offer, err := parseSDP(inv2.Body)
	if err != nil || len(offer.Formats) != 2 || offer.Formats[0] != 126 || !offer.hasPCMU() {
		t.Fatalf("our offer: %+v %v", offer, err)
	}
	ok := sipResponse(inv2, 200, "OK", "ph2")
	ok.Add("Contact", "<sip:wacalls@127.0.0.1:18724;transport=tcp>")
	ok.Add("Content-Type", "application/sdp")
	ok.Body = []byte(p.sdp()) // PCMU, as the engine answers
	p.sendMsg(ok)
	if ack := p.read(); ack.Method != "ACK" || ack.Get("cseq") != "2 ACK" || len(ack.Body) != 0 {
		t.Fatalf("ACK: %s", ack.Bytes())
	}
	waitFor(t, "accept", func() bool { defer be.lock()(); return len(be.accepted) == 1 })
	be.mu.Lock()
	rejected := len(be.rejected)
	be.mu.Unlock()
	if rejected != 0 {
		t.Fatal("the WhatsApp call was rejected on the 488")
	}
}

func TestSIPRTPSendsAndEchoesTheEnginesProbes(t *testing.T) {
	sent := make(chan []byte, 256)
	tr := newTunnelRTP(func(_ int, p []byte) bool {
		sent <- append([]byte(nil), p...)
		return true
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := newSIPRTP(tr, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 45536}, codecOpusWB, nil, log)
	r.start()
	defer r.close()
	next := func() []byte {
		select {
		case p := <-sent:
			return p
		case <-time.After(2 * time.Second):
			t.Fatal("nothing sent")
		}
		return nil
	}
	if p := next(); !isProbe(p) { // the very first packet: our probe
		t.Fatalf("first packet % x is not a probe", p)
	}
	// The engine's own probe, as captured on 2026-11-02, comes back as is.
	eng, _ := hex.DecodeString("807f03e8000003e80613e1c6" + hex.EncodeToString([]byte(probePayload)))
	tr.deliver(45536, eng)
	for i := 0; i < 50; i++ {
		if p := next(); string(p) == string(eng) {
			return
		}
	}
	t.Fatal("the engine's probe was not echoed")
}
