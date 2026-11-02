package main

// SIP gateway: a BlackBerry 10 phone's own call engine (EBMService, the BBM
// Video engine, run against this server instead of BlackBerry's dead SIP
// service) becomes the terminal of this server's WhatsApp calls. The phone
// rings and talks in its native Phone app; this side bridges SIP + RTP to
// the WhatsApp call core.
//
// - The phone registers over one TCP connection (REGISTER, any credentials:
//   the listener is limited to -sip-allow networks). Calls to the phone and
//   from it run over that same connection.
// - A WhatsApp call coming in rings the phone: INVITE with an audio-only SDP
//   offer (PCMU). The phone's 200 OK claims and accepts the WhatsApp call;
//   a 4xx/6xx declines it. If the caller gives up first: CANCEL.
// - An INVITE from the phone places a WhatsApp call to the number in its
//   Request-URI (sip:393331234567@...): 180 while it rings, 200 when answered.
// - Hang up from either side: BYE (or CANCEL / an error response).
// - Audio: siprtp.go (PCMU 20 ms <-> the call core's 16 kHz PCM). The phone
//   cancels its own echo.
//
// The protocol details of the phone's engine are in the BBM Video research
// notes (bbmvideo-re/RAPPORTO.md on the server).

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"wacalls/internal/voip/core"

	"go.mau.fi/whatsmeow/types"
)

const (
	sipOwner = "sip"
	// registration lifetime granted over the Berry Bridge link, seconds
	sipTunnelExpires = 600
)

// sipBackend is what the gateway needs from the WhatsApp side (sessionBackend
// in production, a fake in tests).
type sipBackend interface {
	SessionID() string
	PhoneOf(peerJID string) string
	StartCall(ctx context.Context, phone string) (callID string, err error)
	Accept(ctx context.Context, callID string) error
	Reject(ctx context.Context, callID string)
	Hangup(ctx context.Context, callID string)
	Attach(callID string, b mediaBridge)
	Feed(callID string, pcm []float32)
}

type sipConfig struct {
	Domain  string // our SIP domain, e.g. "wacalls.local"
	MediaIP string // IP for RTP; empty: the phone connection's local address
	RTPLo   int    // RTP port range
	RTPHi   int
	Allow   []*net.IPNet // who may connect
	// StateFile keeps the registration made over the Berry Bridge link
	// across restarts (empty: memory only). After a restart the engine
	// still holds its connection to the relay and won't register again for
	// up to sipTunnelExpires, so without it incoming calls miss the phone.
	StateFile string
	// Opus: offer and accept Opus (the engine's OPUS-WB, 16 kHz) before PCMU.
	Opus bool
	// DelayedOffer (with Opus): ring the phone with an INVITE without SDP.
	// Answering an offer, the engine always picks PCMU (its "lawful access"
	// codec choice, seen 2026-11-02, even with Opus offered first), while
	// its own offers put OPUS-WB first: so it offers in the 200 OK and we
	// answer in the ACK. Refused (4xx/6xx): we ring again with our offer.
	DelayedOffer bool
}

// sipTunnelState is StateFile's content.
type sipTunnelState struct {
	Contact string    `json:"contact"`
	AOR     string    `json:"aor"`
	At      time.Time `json:"at"` // of the REGISTER
}

type sipGateway struct {
	be     sipBackend
	broker *Broker
	cfg    sipConfig
	log    *slog.Logger

	mu    sync.Mutex
	phone *sipPhone
	// the last registration over the Berry Bridge link (serveTunnel resume)
	tunnelContact, tunnelAOR string
	calls                    map[string]*sipCall // by WhatsApp call ID
	dialog                   map[string]*sipCall // by SIP Call-ID
}

type sipPhone struct {
	conn net.Conn
	// newMedia, for a phone reaching us through the Berry Bridge link
	// (serveTunnel), opens a call's RTP on that link; nil: UDP sockets.
	newMedia func() (rtpTransport, error)
	wmu      sync.Mutex
	contact  string // the phone's registered Contact URI
	aor      string // its address of record (To of the REGISTER)
}

func (p *sipPhone) send(m *sipMsg) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_ = p.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := p.conn.Write(m.Bytes())
	return err
}

type sipCallState int

const (
	sipRinging sipCallState = iota // INVITE pending (either direction)
	sipActive                      // answered
	sipEnded
)

type sipCall struct {
	waID     string
	sipID    string // Call-ID
	fromUs   bool   // we sent the INVITE (WhatsApp call coming in)
	state    sipCallState
	phone    *sipPhone
	localTag string
	// Our INVITE (fromUs) or the phone's (!fromUs).
	invite     *sipMsg
	remoteTo   string // To/From of the phone's side with its tag, once known
	target     string // where in-dialog requests go (the phone's Contact)
	cseq       int    // our next CSeq in this dialog
	offer      *sdpAudio
	codec      sipCodec // the call's audio, from the SDP exchange
	delayed    bool     // our INVITE carried no SDP: the phone offers in its 200 OK
	user       string   // our INVITE's From (calls to the phone)
	display    string
	rtp        *sipRTP
	pacer      *bbUpPacer
	mediaIP    string
	sessionID  int64
	pendingRTP rtpTransport
}

func newSIPGateway(be sipBackend, broker *Broker, cfg sipConfig, log *slog.Logger) *sipGateway {
	if cfg.Domain == "" {
		cfg.Domain = "wacalls.local"
	}
	if cfg.RTPLo == 0 {
		cfg.RTPLo, cfg.RTPHi = 40000, 40099
	}
	g := &sipGateway{be: be, broker: broker, cfg: cfg, log: log.With("component", "sipgateway"),
		calls: map[string]*sipCall{}, dialog: map[string]*sipCall{}}
	g.loadTunnelState(time.Now())
	return g
}

// offerCodecs: what we offer the phone, in preference order.
func (g *sipGateway) offerCodecs() []sipCodec {
	if g.cfg.Opus {
		return []sipCodec{codecOpusWB, codecPCMU}
	}
	return []sipCodec{codecPCMU}
}

// loadTunnelState restores the last registration over the link if it is
// still within its lifetime.
func (g *sipGateway) loadTunnelState(now time.Time) {
	if g.cfg.StateFile == "" {
		return
	}
	b, err := os.ReadFile(g.cfg.StateFile)
	if err != nil {
		return
	}
	var st sipTunnelState
	if json.Unmarshal(b, &st) != nil || st.Contact == "" {
		return
	}
	if age := now.Sub(st.At); age < 0 || age > sipTunnelExpires*time.Second {
		return
	}
	g.tunnelContact, g.tunnelAOR = st.Contact, st.AOR
	g.log.Info("tunnel registration restored", "contact", st.Contact, "age", now.Sub(st.At).Round(time.Second))
}

// saveTunnelState writes (contact != "") or removes the state file.
func (g *sipGateway) saveTunnelState(contact, aor string, at time.Time) {
	f := g.cfg.StateFile
	if f == "" {
		return
	}
	if contact == "" {
		_ = os.Remove(f)
		return
	}
	b, _ := json.Marshal(sipTunnelState{Contact: contact, AOR: aor, At: at})
	tmp := f + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		g.log.Warn("tunnel registration not saved", "err", err)
		return
	}
	if err := os.Rename(tmp, f); err != nil {
		g.log.Warn("tunnel registration not saved", "err", err)
	}
}

// registered reports whether a phone is registered now (bbGateway then leaves
// incoming calls to it).
func (g *sipGateway) registered() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.phone != nil
}

// owns reports whether the gateway handles this WhatsApp call.
func (g *sipGateway) owns(waID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.calls[waID]
	return ok
}

func (g *sipGateway) serve(ctx context.Context, ln net.Listener) error {
	sub := g.broker.subscribe("sip-gateway")
	defer g.broker.unsubscribe(sub)
	go g.events(ctx, sub)
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if !g.allowed(c.RemoteAddr()) {
			g.log.Warn("sip connection refused", "from", c.RemoteAddr())
			_ = c.Close()
			continue
		}
		go g.handleConn(ctx, c)
	}
}

func (g *sipGateway) allowed(a net.Addr) bool {
	if len(g.cfg.Allow) == 0 {
		return true
	}
	ta, ok := a.(*net.TCPAddr)
	if !ok {
		return false
	}
	for _, n := range g.cfg.Allow {
		if n.Contains(ta.IP) {
			return true
		}
	}
	return false
}

func (g *sipGateway) handleConn(ctx context.Context, c net.Conn) {
	g.log.Info("sip connection", "from", c.RemoteAddr())
	g.readLoop(ctx, &sipPhone{conn: c})
}

// serveTunnel runs a phone that reaches us through the Berry Bridge link
// (bbgateway.go, sip.open): c carries its SIP stream, newMedia its RTP.
// resume: the link came back while the phone's engine kept its own
// connection to the relay, so it won't register again for a while: it is
// taken as registered with the contact of its last REGISTER.
func (g *sipGateway) serveTunnel(ctx context.Context, c net.Conn, newMedia func() (rtpTransport, error), resume bool) {
	p := &sipPhone{conn: c, newMedia: newMedia}
	g.mu.Lock()
	if resume && g.tunnelContact != "" {
		p.contact, p.aor = g.tunnelContact, g.tunnelAOR
		g.phone = p
		g.log.Info("phone registration resumed over the Berry Bridge link", "contact", p.contact)
	}
	g.mu.Unlock()
	g.log.Info("sip over the Berry Bridge link")
	g.readLoop(ctx, p)
}

func (g *sipGateway) readLoop(ctx context.Context, p *sipPhone) {
	c := p.conn
	defer func() {
		_ = c.Close()
		g.mu.Lock()
		if g.phone == p {
			g.phone = nil
		}
		g.mu.Unlock()
		g.log.Info("sip connection closed", "from", c.RemoteAddr())
	}()
	r := bufio.NewReaderSize(c, 64*1024)
	var buf []byte
	tmp := make([]byte, 16*1024)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for {
				// RFC 5626 keepalive: CRLFCRLF ping, CRLF pong.
				trimmed := 0
				for trimmed+1 < len(buf) && buf[trimmed] == '\r' && buf[trimmed+1] == '\n' {
					trimmed += 2
				}
				if trimmed >= 4 {
					p.wmu.Lock()
					_, _ = c.Write([]byte("\r\n"))
					p.wmu.Unlock()
				}
				buf = buf[trimmed:]
				m, used, perr := parseSIP(buf)
				if errors.Is(perr, errSIPIncomplete) {
					break
				}
				if perr != nil {
					g.log.Warn("sip parse error, dropping connection", "err", perr)
					return
				}
				buf = buf[used:]
				g.handle(ctx, p, m)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				g.log.Info("sip read", "err", err)
			}
			return
		}
	}
}

func (g *sipGateway) handle(ctx context.Context, p *sipPhone, m *sipMsg) {
	if !m.isRequest() {
		g.onResponse(ctx, p, m)
		return
	}
	g.log.Debug("sip request", "method", m.Method, "call_id", m.Get("call-id"))
	switch m.Method {
	case "REGISTER":
		g.onRegister(p, m)
	case "INVITE":
		g.onInvite(ctx, p, m)
	case "ACK":
	case "BYE":
		_ = p.send(sipResponse(m, 200, "OK", ""))
		g.onPhoneHangup(ctx, m.Get("call-id"), "bye")
	case "CANCEL":
		_ = p.send(sipResponse(m, 200, "OK", ""))
		g.onPhoneCancel(ctx, p, m)
	default: // OPTIONS, SUBSCRIBE, PUBLISH, MESSAGE, NOTIFY, INFO, UPDATE...
		r := sipResponse(m, 200, "OK", sipRandom(4))
		if m.Method == "SUBSCRIBE" {
			r.Add("Expires", "3600")
		}
		_ = p.send(r)
	}
}

func (g *sipGateway) onRegister(p *sipPhone, m *sipMsg) {
	contact := m.Get("contact")
	exp := m.Get("expires")
	if e := sipParam(contact, "expires"); e != "" {
		exp = e
	}
	if exp == "" {
		exp = "3600"
	}
	if n, err := strconv.Atoi(exp); p.newMedia != nil && err == nil && n > sipTunnelExpires {
		// Over the link: refresh often, so a server restart (which loses
		// the registration) costs minutes, not the engine's 12 hours.
		exp = strconv.Itoa(sipTunnelExpires)
	}
	r := sipResponse(m, 200, "OK", sipRandom(4))
	if contact != "" && contact != "*" {
		r.Add("Contact", contact+";expires="+exp)
	}
	r.Add("Expires", exp)
	_ = p.send(r)
	g.mu.Lock()
	defer g.mu.Unlock()
	if exp == "0" {
		if g.phone == p {
			g.phone = nil
		}
		if p.newMedia != nil {
			g.tunnelContact, g.tunnelAOR = "", ""
			g.saveTunnelState("", "", time.Time{})
		}
		g.log.Info("phone unregistered")
		return
	}
	p.contact = sipAddrURI(contact)
	p.aor = m.Get("to")
	if p.newMedia != nil {
		g.tunnelContact, g.tunnelAOR = p.contact, p.aor
		g.saveTunnelState(p.contact, p.aor, time.Now())
	}
	if g.phone != p {
		g.log.Info("phone registered", "contact", p.contact, "from", p.conn.RemoteAddr(), "agent", m.Get("user-agent"))
	}
	g.phone = p
}

// openMedia opens a call's RTP for this phone.
func (g *sipGateway) openMedia(p *sipPhone) (rtpTransport, error) {
	if p.newMedia != nil {
		return p.newMedia()
	}
	return listenRTP(g.mediaIPFor(p), g.cfg.RTPLo, g.cfg.RTPHi)
}

func (g *sipGateway) mediaIPFor(p *sipPhone) string {
	if p.newMedia != nil {
		return "127.0.0.1" // the relay on the phone itself
	}
	if g.cfg.MediaIP != "" {
		return g.cfg.MediaIP
	}
	if ta, ok := p.conn.LocalAddr().(*net.TCPAddr); ok {
		return ta.IP.String()
	}
	return "127.0.0.1"
}

// ---- WhatsApp call coming in: we ring the phone ----------------------------

func (g *sipGateway) events(ctx context.Context, sub *subscriber) {
	for data := range sub.ch {
		var ev map[string]any
		if json.Unmarshal(data, &ev) != nil || ev["sessionId"] != g.be.SessionID() {
			continue
		}
		id, _ := ev["id"].(string)
		switch ev["type"] {
		case "incoming":
			peer, _ := ev["peer"].(string)
			g.ringPhone(id, peer)
		case "incoming-claimed":
			if owner, _ := ev["owner"].(string); owner != sipOwner {
				g.waEnded(id, "claimed")
			}
		case "call-status":
			if ev["status"] == string(StatusConnected) {
				g.waConnected(id)
			}
		case "call-ended":
			reason, _ := ev["reason"].(string)
			g.waEnded(id, reason)
		}
	}
}

func (g *sipGateway) ringPhone(waID, peerJID string) {
	g.mu.Lock()
	p := g.phone
	busy := len(g.calls) > 0
	g.mu.Unlock()
	if p == nil || busy {
		return
	}
	num := strings.TrimPrefix(g.be.PhoneOf(peerJID), "+")
	user, display := num, "+"+num
	if num == "" {
		user, display = "whatsapp", "WhatsApp"
	}
	ip := g.mediaIPFor(p)
	conn, err := g.openMedia(p)
	if err != nil {
		g.log.Error("cannot ring the phone", "err", err)
		return
	}
	c := &sipCall{waID: waID, sipID: sipRandom(12) + "@" + g.cfg.Domain, fromUs: true, phone: p,
		localTag: sipRandom(6), target: p.contact, cseq: 1, mediaIP: ip, sessionID: time.Now().Unix(), pendingRTP: conn}
	c.user, c.display = user, display
	c.delayed = g.cfg.Opus && g.cfg.DelayedOffer
	g.mu.Lock()
	g.calls[waID] = c
	g.dialog[c.sipID] = c
	g.mu.Unlock()
	g.log.Info("ringing the phone", "call_id", waID, "from", display, "delayed_offer", c.delayed)
	if err := g.sendInvite(c); err != nil {
		g.log.Warn("INVITE to the phone failed", "err", err)
		g.forget(c)
	}
}

// sendInvite sends (or, with the next CSeq, sends again) our INVITE to the
// phone: with our SDP offer, or none when c.delayed.
func (g *sipGateway) sendInvite(c *sipCall) error {
	p, ip := c.phone, c.mediaIP
	inv := &sipMsg{Method: "INVITE", URI: p.contact}
	inv.Add("Via", "SIP/2.0/TCP "+ip+";branch="+sipBranch())
	inv.Add("Max-Forwards", "70")
	inv.Add("From", fmt.Sprintf("\"%s\" <sip:%s@%s>;tag=%s", c.display, c.user, g.cfg.Domain, c.localTag))
	to := p.aor
	if to == "" {
		to = "<" + p.contact + ">"
	}
	if i := strings.Index(to, ";tag="); i >= 0 {
		to = to[:i]
	}
	inv.Add("To", to)
	inv.Add("Call-ID", c.sipID)
	inv.Add("CSeq", fmt.Sprintf("%d INVITE", c.cseq))
	inv.Add("Contact", fmt.Sprintf("<sip:%s@%s;transport=tcp>", c.user, ip))
	inv.Add("Allow", "INVITE, ACK, CANCEL, BYE, OPTIONS")
	inv.Add("User-Agent", "WaCalls")
	if !c.delayed {
		inv.Add("Content-Type", "application/sdp")
		inv.Body = buildSDP(ip, c.pendingRTP.LocalPort(), c.sessionID, false, g.offerCodecs()...)
	}
	c.invite = inv
	return p.send(inv)
}

func (g *sipGateway) onResponse(ctx context.Context, p *sipPhone, m *sipMsg) {
	g.mu.Lock()
	c := g.dialog[m.Get("call-id")]
	g.mu.Unlock()
	if c == nil {
		return
	}
	_, method := m.CSeq()
	if method != "INVITE" || !c.fromUs {
		return // BYE/CANCEL answers
	}
	if m.Code < 200 {
		if sipParam(m.Get("to"), "tag") != "" {
			c.remoteTo = m.Get("to")
		}
		return
	}
	c.remoteTo = m.Get("to")
	if m.Code >= 300 && c.delayed && (m.Code == 400 || m.Code == 415 || m.Code == 488 || m.Code == 606) {
		// The engine wants an offer after all: ring again with ours.
		g.ack(c, m, c.invite.All("via")[0], nil)
		g.mu.Lock()
		ended := c.state == sipEnded
		g.mu.Unlock()
		if !ended {
			g.log.Info("phone refused the INVITE without SDP, offering", "call_id", c.waID, "code", m.Code)
			c.delayed = false
			c.cseq++
			c.remoteTo = ""
			if err := g.sendInvite(c); err == nil {
				return
			}
		}
	}
	if m.Code >= 300 {
		g.ack(c, m, c.invite.All("via")[0], nil)
		g.mu.Lock()
		ended := c.state == sipEnded
		c.state = sipEnded
		g.mu.Unlock()
		g.forget(c)
		if !ended {
			g.log.Info("phone declined", "call_id", c.waID, "code", m.Code, "reason", m.Get("reason"))
			g.be.Reject(ctx, c.waID)
		}
		return
	}
	// 200 OK: the phone's answer to our offer, or (c.delayed) its offer,
	// which we answer in the ACK.
	if ct := sipAddrURI(m.Get("contact")); ct != "" {
		c.target = ct
	}
	ans, err := parseSDP(m.Body)
	codec, ok := codecPCMU, false
	if err == nil {
		codec, ok = ans.pickCodec(g.cfg.Opus)
		g.log.Info("phone sdp", "call_id", c.waID, "offer", c.delayed, "formats", ans.Formats, "rtpmap", ans.RTPMap, "codec", codec.String())
	}
	var ackSDP []byte
	if c.delayed && ok {
		ackSDP = buildSDP(c.mediaIP, c.pendingRTP.LocalPort(), c.sessionID, ans.Video, codec)
	}
	g.ack(c, m, "SIP/2.0/TCP "+c.mediaIP+";branch="+sipBranch(), ackSDP)
	g.mu.Lock()
	if c.state == sipEnded { // the caller hung up meanwhile
		g.mu.Unlock()
		g.sendBye(c)
		g.forget(c)
		return
	}
	c.state = sipActive
	g.mu.Unlock()
	if !ok {
		g.log.Warn("phone answered without a codec we offered", "err", err)
		g.sendBye(c)
		g.forget(c)
		g.be.Hangup(ctx, c.waID)
		return
	}
	c.codec = codec
	g.startMedia(c, ans)
	if err := g.be.Accept(ctx, c.waID); err != nil {
		g.log.Warn("accepting the WhatsApp call failed", "call_id", c.waID, "err", err)
		g.sendBye(c)
		g.forget(c)
		return
	}
	g.log.Info("phone answered", "call_id", c.waID)
}

// ack acknowledges a final answer to our INVITE; sdp (delayed offer only)
// is our answer to the offer in the phone's 200 OK.
func (g *sipGateway) ack(c *sipCall, resp *sipMsg, via string, sdp []byte) {
	uri := c.target
	if resp.Code >= 300 {
		uri = c.invite.URI
	}
	a := &sipMsg{Method: "ACK", URI: uri}
	a.Add("Via", via)
	a.Add("Max-Forwards", "70")
	a.Add("From", c.invite.Get("from"))
	a.Add("To", resp.Get("to"))
	a.Add("Call-ID", c.sipID)
	n, _ := c.invite.CSeq()
	a.Add("CSeq", fmt.Sprintf("%d ACK", n))
	if sdp != nil {
		a.Add("Content-Type", "application/sdp")
		a.Body = sdp
	}
	_ = c.phone.send(a)
}

func (g *sipGateway) sendBye(c *sipCall) {
	c.cseq++
	b := &sipMsg{Method: "BYE", URI: c.target}
	b.Add("Via", "SIP/2.0/TCP "+c.mediaIP+";branch="+sipBranch())
	b.Add("Max-Forwards", "70")
	if c.fromUs {
		b.Add("From", c.invite.Get("from"))
		b.Add("To", c.remoteTo)
	} else { // the phone's INVITE: its From is the remote side, our To-tag is ours
		b.Add("From", c.invite.Get("to")+";tag="+c.localTag)
		b.Add("To", c.invite.Get("from"))
	}
	b.Add("Call-ID", c.sipID)
	b.Add("CSeq", fmt.Sprintf("%d BYE", c.cseq))
	_ = c.phone.send(b)
}

func (g *sipGateway) sendCancel(c *sipCall) {
	x := &sipMsg{Method: "CANCEL", URI: c.invite.URI}
	x.Add("Via", c.invite.All("via")[0])
	x.Add("Max-Forwards", "70")
	x.Add("From", c.invite.Get("from"))
	x.Add("To", c.invite.Get("to"))
	x.Add("Call-ID", c.sipID)
	n, _ := c.invite.CSeq()
	x.Add("CSeq", fmt.Sprintf("%d CANCEL", n))
	_ = c.phone.send(x)
}

// waEnded: the WhatsApp call is over (or someone else took it).
func (g *sipGateway) waEnded(waID, reason string) {
	g.mu.Lock()
	c := g.calls[waID]
	if c == nil || c.state == sipEnded {
		g.mu.Unlock()
		return
	}
	prev := c.state
	c.state = sipEnded
	g.mu.Unlock()
	g.log.Info("whatsapp call ended", "call_id", waID, "reason", reason)
	switch {
	case c.fromUs && prev == sipRinging:
		g.sendCancel(c) // the phone answers 487; onResponse cleans up
		time.AfterFunc(10*time.Second, func() { g.forget(c) })
	case !c.fromUs && prev == sipRinging:
		code, text := sipFailureFor(reason)
		r := sipResponse(c.invite, code, text, c.localTag)
		_ = c.phone.send(r)
		g.forget(c)
	default:
		g.sendBye(c)
		g.forget(c)
	}
}

func sipFailureFor(reason string) (int, string) {
	switch core.EndCallReason(reason) {
	case core.EndCallReasonDeclined:
		return 603, "Decline"
	case core.EndCallReasonBusy:
		return 486, "Busy Here"
	case core.EndCallReasonTimeout:
		return 480, "Temporarily Unavailable"
	}
	return 480, "Temporarily Unavailable"
}

// ---- call from the phone: we place a WhatsApp call -------------------------

func (g *sipGateway) onInvite(ctx context.Context, p *sipPhone, m *sipMsg) {
	g.mu.Lock()
	existing := g.dialog[m.Get("call-id")]
	busy := len(g.calls) > 0
	g.mu.Unlock()
	if existing != nil { // re-INVITE (hold, video upgrade...): keep our audio as it is
		r := sipResponse(m, 200, "OK", existing.localTag)
		r.Add("Contact", fmt.Sprintf("<sip:wacalls@%s;transport=tcp>", existing.mediaIP))
		r.Add("Content-Type", "application/sdp")
		port := 0
		if existing.rtp != nil {
			port = existing.rtp.localPort()
		} else if existing.pendingRTP != nil {
			port = existing.pendingRTP.LocalPort()
		}
		offer, _ := parseSDP(m.Body)
		r.Body = buildSDP(existing.mediaIP, port, existing.sessionID+1, offer != nil && offer.Video, existing.codec)
		_ = p.send(r)
		return
	}
	_ = p.send(sipResponse(m, 100, "Trying", ""))
	if busy {
		_ = p.send(sipResponse(m, 486, "Busy Here", sipRandom(4)))
		return
	}
	offer, err := parseSDP(m.Body)
	codec, ok := codecPCMU, false
	if err == nil {
		codec, ok = offer.pickCodec(g.cfg.Opus)
		g.log.Info("phone sdp offer", "formats", offer.Formats, "rtpmap", offer.RTPMap, "codec", codec.String())
	}
	if !ok {
		_ = p.send(sipResponse(m, 488, "Not Acceptable Here", sipRandom(4)))
		return
	}
	num := normalizePhone(sipURIUser(m.URI))
	if num == "" {
		num = normalizePhone(sipURIUser(sipAddrURI(m.Get("to"))))
	}
	if len(num) < 6 {
		_ = p.send(sipResponse(m, 404, "Not Found", sipRandom(4)))
		return
	}
	ip := g.mediaIPFor(p)
	conn, err := g.openMedia(p)
	if err != nil {
		_ = p.send(sipResponse(m, 503, "Service Unavailable", sipRandom(4)))
		return
	}
	c := &sipCall{sipID: m.Get("call-id"), phone: p, localTag: sipRandom(6), invite: m,
		target: sipAddrURI(m.Get("contact")), cseq: 1, offer: offer, codec: codec, mediaIP: ip,
		sessionID: time.Now().Unix(), pendingRTP: conn}
	g.mu.Lock()
	g.dialog[c.sipID] = c
	g.mu.Unlock()
	waID, err := g.be.StartCall(ctx, num)
	if err != nil {
		g.log.Warn("placing the WhatsApp call failed", "to", num, "err", err)
		_ = p.send(sipResponse(m, 480, "Temporarily Unavailable", c.localTag))
		g.forget(c)
		return
	}
	g.mu.Lock()
	c.waID = waID
	g.calls[waID] = c
	g.mu.Unlock()
	g.log.Info("phone calls out", "to", num, "call_id", waID)
	_ = p.send(sipResponse(m, 180, "Ringing", c.localTag))
}

// waConnected: the WhatsApp call the phone placed was answered.
func (g *sipGateway) waConnected(waID string) {
	g.mu.Lock()
	c := g.calls[waID]
	if c == nil || c.fromUs || c.state != sipRinging {
		g.mu.Unlock()
		return
	}
	c.state = sipActive
	g.mu.Unlock()
	port := c.pendingRTP.LocalPort()
	r := sipResponse(c.invite, 200, "OK", c.localTag)
	r.Add("Contact", fmt.Sprintf("<sip:wacalls@%s;transport=tcp>", c.mediaIP))
	r.Add("Allow", "INVITE, ACK, CANCEL, BYE, OPTIONS")
	r.Add("Content-Type", "application/sdp")
	r.Body = buildSDP(c.mediaIP, port, c.sessionID, c.offer.Video, c.codec)
	g.startMedia(c, c.offer)
	_ = c.phone.send(r)
	g.log.Info("whatsapp peer answered", "call_id", waID)
}

func (g *sipGateway) onPhoneCancel(ctx context.Context, p *sipPhone, m *sipMsg) {
	g.mu.Lock()
	c := g.dialog[m.Get("call-id")]
	if c == nil || c.fromUs || c.state != sipRinging {
		g.mu.Unlock()
		return
	}
	c.state = sipEnded
	g.mu.Unlock()
	_ = p.send(sipResponse(c.invite, 487, "Request Terminated", c.localTag))
	g.forget(c)
	if c.waID != "" {
		g.be.Hangup(ctx, c.waID)
	}
}

func (g *sipGateway) onPhoneHangup(ctx context.Context, sipID, why string) {
	g.mu.Lock()
	c := g.dialog[sipID]
	if c == nil || c.state == sipEnded {
		g.mu.Unlock()
		return
	}
	c.state = sipEnded
	g.mu.Unlock()
	g.log.Info("phone hung up", "call_id", c.waID, "how", why)
	g.forget(c)
	if c.waID != "" {
		g.be.Hangup(ctx, c.waID)
	}
}

// ---- media and bookkeeping -------------------------------------------------

func (g *sipGateway) startMedia(c *sipCall, remote *sdpAudio) {
	dst := &net.UDPAddr{IP: net.ParseIP(remote.IP), Port: remote.Port}
	waID := c.waID
	pacer := newBBUpPacer(func(pcm []float32) { g.be.Feed(waID, pcm) })
	rtp := newSIPRTP(c.pendingRTP, dst, c.codec, pacer.push, g.log.With("call_id", waID))
	g.mu.Lock()
	c.rtp, c.pacer, c.pendingRTP = rtp, pacer, nil
	g.mu.Unlock()
	rtp.start()
	g.be.Attach(waID, rtp)
	g.log.Info("sip media", "call_id", waID, "codec", c.codec.String(), "local_port", rtp.localPort(), "phone", dst, "tunnel", c.phone.newMedia != nil)
}

func (g *sipGateway) forget(c *sipCall) {
	g.mu.Lock()
	if g.calls[c.waID] == c {
		delete(g.calls, c.waID)
	}
	if g.dialog[c.sipID] == c {
		delete(g.dialog, c.sipID)
	}
	rtp, pacer, pending := c.rtp, c.pacer, c.pendingRTP
	c.rtp, c.pacer, c.pendingRTP = nil, nil, nil
	g.mu.Unlock()
	if pacer != nil {
		pacer.close()
	}
	if rtp != nil {
		rtp.close()
	}
	if pending != nil {
		_ = pending.Close()
	}
}

// ---- production backend ----------------------------------------------------

// sessionBackend runs the gateway's calls on a WhatsApp session, like the
// Berry Bridge gateway does (bbGateway.onControl).
type sessionBackend struct {
	sessions  *SessionManager
	broker    *Broker
	sessionID string // empty: the first paired session
}

func (b *sessionBackend) session() (*Session, error) {
	return pickSession(b.sessions, b.sessionID)
}

func (b *sessionBackend) SessionID() string {
	s, err := b.session()
	if err != nil {
		return ""
	}
	return s.id
}

func (b *sessionBackend) PhoneOf(peer string) string {
	s, _ := b.session()
	return phoneOfJID(s, peer)
}

func (b *sessionBackend) StartCall(ctx context.Context, phone string) (string, error) {
	s, err := b.session()
	if err != nil {
		return "", err
	}
	peer := types.NewJID(phone, types.DefaultUserServer)
	callID, err := s.startOutgoing(ctx, peer, false)
	if err != nil {
		return "", err
	}
	owner := sipOwner
	b.broker.upsertCall(CallRecord{SessionID: s.id, CallID: callID, Owner: &owner, Direction: "outbound",
		Peer: peer.String(), StartedAt: time.Now().UnixMilli(), Status: StatusRinging})
	return callID, nil
}

func (b *sessionBackend) Accept(ctx context.Context, callID string) error {
	s, err := b.session()
	if err != nil {
		return err
	}
	ac, ok := s.reg.get(callID)
	if !ok {
		return errors.New("no such call")
	}
	if !b.broker.setOwner(callID, sipOwner) {
		return errors.New("claimed by another client")
	}
	b.broker.emitIncomingClaimed(s.id, callID, sipOwner)
	return ac.cm.AcceptCall(ctx, callID)
}

func (b *sessionBackend) Reject(ctx context.Context, callID string) {
	s, err := b.session()
	if err != nil {
		return
	}
	if ac, ok := s.reg.get(callID); ok {
		_ = ac.cm.RejectCall(ctx, callID, core.EndCallReasonDeclined)
	}
	s.removeCall(callID)
	b.broker.endCall(callID, string(core.EndCallReasonDeclined))
}

func (b *sessionBackend) Hangup(ctx context.Context, callID string) {
	s, err := b.session()
	if err != nil {
		return
	}
	if ac, ok := s.reg.get(callID); ok {
		_ = ac.cm.EndCall(ctx, core.EndCallReasonUserEnded)
	}
	s.removeCall(callID)
	b.broker.endCall(callID, string(core.EndCallReasonUserEnded))
}

func (b *sessionBackend) Attach(callID string, br mediaBridge) {
	if s, err := b.session(); err == nil {
		s.setBridge(callID, br)
	}
}

func (b *sessionBackend) Feed(callID string, pcm []float32) {
	s, err := b.session()
	if err != nil {
		return
	}
	if ac, ok := s.reg.get(callID); ok {
		ac.cm.FeedCapturedPCM(pcm)
	}
}

// parseCIDRs parses "a/b,c/d" (bare IPs become /32).
func parseCIDRs(s string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.Contains(p, "/") {
			p += "/32"
		}
		_, n, err := net.ParseCIDR(p)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}
