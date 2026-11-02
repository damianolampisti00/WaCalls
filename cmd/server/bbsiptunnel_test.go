package main

import (
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// tunnelPhone is the Berry Bridge app relaying for the phone's engine.
type tunnelPhone struct {
	t   *testing.T
	ws  *websocket.Conn
	ctx context.Context
	sip []byte      // SIP stream received so far
	rtp []tunnelPkt // RTP datagrams received
}

func (p *tunnelPhone) frame(kind byte, port int, payload []byte) {
	f := make([]byte, bbHeaderLen, bbHeaderLen+len(payload))
	f[0] = kind
	binary.LittleEndian.PutUint16(f[2:], uint16(port))
	if err := p.ws.Write(p.ctx, websocket.MessageBinary, append(f, payload...)); err != nil {
		p.t.Fatal(err)
	}
}

func (p *tunnelPhone) text(s string) {
	if err := p.ws.Write(p.ctx, websocket.MessageText, []byte(s)); err != nil {
		p.t.Fatal(err)
	}
}

func (p *tunnelPhone) sendSIP(m *sipMsg) { p.frame(bbKindSIP, 0, m.Bytes()) }

// readSIP returns the next SIP message, keeping RTP that arrives meanwhile.
func (p *tunnelPhone) readSIP() *sipMsg {
	p.t.Helper()
	for {
		if m, n, err := parseSIP(p.sip); err == nil {
			p.sip = p.sip[n:]
			return m
		}
		p.read()
	}
}

func (p *tunnelPhone) read() {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(p.ctx, 3*time.Second)
	defer cancel()
	typ, data, err := p.ws.Read(ctx)
	if err != nil {
		p.t.Fatalf("link read: %v", err)
	}
	if typ != websocket.MessageBinary || len(data) < bbHeaderLen {
		return
	}
	switch data[0] {
	case bbKindSIP:
		p.sip = append(p.sip, data[bbHeaderLen:]...)
	case bbKindRTP:
		p.rtp = append(p.rtp, tunnelPkt{int(binary.LittleEndian.Uint16(data[2:4])), data[bbHeaderLen:]})
	}
}

func dialTunnelPhone(t *testing.T, srvURL string) *tunnelPhone {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	h := http.Header{}
	h.Set("Authorization", "Bearer "+testToken)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srvURL, "http")+"/ws", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	if _, _, err := ws.Read(ctx); err != nil { // hello
		t.Fatal(err)
	}
	return &tunnelPhone{t: t, ws: ws, ctx: ctx}
}

func TestSIPOverTheBerryBridgeLink(t *testing.T) {
	be := &fakeBackend{}
	broker := NewBroker()
	sipGW := newSIPGateway(be, broker, sipConfig{}, slog.Default())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gctx, gcancel := context.WithCancel(context.Background())
	t.Cleanup(gcancel)
	go sipGW.serve(gctx, ln)
	bb := newBBGateway(nil, broker, testToken, "", slog.Default())
	bb.sipGW, bb.sipPhone = sipGW, sipGW.registered
	srv := httptest.NewServer(bb.routes())
	t.Cleanup(srv.Close)

	p := dialTunnelPhone(t, srv.URL)
	p.text(`{"type":"sip.open"}`)
	reg := &sipMsg{Method: "REGISTER", URI: "sip:wacalls.local"}
	reg.Add("Via", "SIP/2.0/TCP 127.0.0.1:18724;branch=z9hG4bKr1")
	reg.Add("From", "<sip:wacalls@wacalls.local>;tag=1")
	reg.Add("To", "<sip:wacalls@wacalls.local>")
	reg.Add("Call-ID", "reg@127.0.0.1")
	reg.Add("CSeq", "1 REGISTER")
	reg.Add("Contact", "<sip:wacalls@127.0.0.1:18724;transport=tcp>")
	reg.Add("Expires", "43200")
	p.sendSIP(reg)
	if r := p.readSIP(); r.Code != 200 || r.Get("expires") != "600" {
		t.Fatalf("REGISTER over the link: %s", r.Bytes())
	}
	waitFor(t, "registration", sipGW.registered)

	broker.upsertCall(CallRecord{SessionID: "s1", CallID: "waT", Direction: "inbound", Status: StatusRinging})
	broker.emitIncoming("s1", "waT", "393331234567@s.whatsapp.net")
	inv := p.readSIP()
	offer, err := parseSDP(inv.Body)
	if inv.Method != "INVITE" || err != nil || offer.IP != "127.0.0.1" || offer.Port != bbTunnelRTPPort {
		t.Fatalf("INVITE must point the engine at the relay: %s", inv.Bytes())
	}
	ok := sipResponse(inv, 200, "OK", "ph")
	ok.Add("Contact", "<sip:wacalls@127.0.0.1:18724;transport=tcp>")
	ok.Add("Content-Type", "application/sdp")
	ok.Body = []byte("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 45536 RTP/AVP 0\r\n")
	p.sendSIP(ok)
	if ack := p.readSIP(); ack.Method != "ACK" {
		t.Fatalf("ACK: %s", ack.Bytes())
	}
	waitFor(t, "accept", func() bool { defer be.lock()(); return len(be.accepted) == 1 })

	// downlink RTP reaches the engine's port (after our connectivity probes)
	audio := func() int {
		for i, pk := range p.rtp {
			if !isProbe(pk.data) && !isSTUN(pk.data) {
				return i
			}
		}
		return -1
	}
	for audio() < 0 {
		p.read()
	}
	if a := p.rtp[audio()]; a.port != 45536 || len(a.data) != 12+160 || p.rtp[0].port != 45536 {
		t.Fatalf("RTP to the phone: port %d len %d", a.port, len(a.data))
	}
	// uplink RTP reaches the call core
	for i := 0; i < 15; i++ {
		pkt := make([]byte, 12+160)
		pkt[0] = 0x80
		binary.BigEndian.PutUint16(pkt[2:], uint16(i))
		for k, v := range sipTone(400, 8000, 160) {
			pkt[12+k] = ulawEncode(floatToInt16(v))
		}
		p.frame(bbKindRTP, 45536, pkt)
		time.Sleep(20 * time.Millisecond)
	}
	waitFor(t, "uplink over the link", func() bool { defer be.lock()(); return be.fed > 1600 && be.fedLevel > 0.2 })

	// The link drops and comes back; the engine kept its relay connection.
	p.ws.CloseNow()
	waitFor(t, "unregistered when the link drops", func() bool { return !sipGW.registered() })
	p2 := dialTunnelPhone(t, srv.URL)
	p2.text(`{"type":"sip.open","resume":true}`)
	waitFor(t, "registration resumed", sipGW.registered)
}

func TestTunnelLagExcessOverMinimum(t *testing.T) {
	var l tunnelLag
	// Phone clock 1000 ms behind ours, first frame 30 ms in transit.
	l.note(nil, 0, 1030, 0)
	l.note(nil, 20, 1040, 2) // faster: the new minimum
	l.note(nil, 40, 1060, 1)
	if l.lag != 0 || l.maxLag != 0 {
		t.Fatalf("lag %d max %d, want 0 and 0 (each frame against the minimum known then)", l.lag, l.maxLag)
	}
	l.note(nil, 60, 3080, 5) // 2 s queued on the way
	if l.lag != 2000 || l.maxLag != 2000 || l.maxOutQ != 5 {
		t.Fatalf("lag %d max %d outQ max %d, want 2000 2000 5", l.lag, l.maxLag, l.maxOutQ)
	}
}
