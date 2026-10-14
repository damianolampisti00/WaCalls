package main

// Berry Bridge gateway: a BlackBerry 10 phone (the Berry Bridge app's
// headless service) acts as the audio + UI terminal of this server's calls,
// over ONE authenticated WebSocket. Everything WhatsApp-specific (signaling,
// MLow, SRTP, relays) stays here; the phone only moves 16 kHz PCM and simple
// JSON commands/events.
//
// It listens on its own address (-bb-addr), separate from the unauthenticated
// browser API, so only this endpoint needs to be reachable from outside.
//
// Wire protocol, version 1:
//
//   Authentication: "Authorization: Bearer <token>" on the upgrade request.
//
//   Text frames, JSON. Server -> phone:
//     {"type":"hello","version":1,"session":..,"paired":bool,"calls":[...]}
//     {"type":"call.incoming","callId":..,"peer":<jid>,"phone":"+39.."}
//     {"type":"call.state","callId":..,"state":"starting|ringing|connected","direction":"inbound|outbound","peer":..,"phone":..}
//     {"type":"call.ended","callId":..,"reason":"user_ended|declined|busy|timeout|..."}
//     {"type":"call.started","callId":..}         (answer to call.start)
//     {"type":"pong","t":<echoed>}
//     {"type":"error","for":<request type>,"message":..}
//   Phone -> server:
//     {"type":"call.start","phone":"+39..."}
//     {"type":"call.accept","callId":..}   {"type":"call.reject","callId":..}
//     {"type":"call.hangup","callId":..}   {"type":"call.mute","muted":true}
//     {"type":"test.echo","on":true}       audio frames come straight back (no call needed)
//     {"type":"ping","t":<any>}
//
//   Binary frames, audio, both directions:
//     [0] kind: 0x01 PCM, 0x03 Opus  [1] flags (0)  [2:4] reserved
//     [4:8] sequence number, uint32 LE (per sender, +1 per frame)
//     [8:12] sender clock in ms, uint32 LE (round-trip measurements)
//     [12:] 0x01: PCM, signed 16-bit LE, 16 kHz, mono (any length; the
//           server sends the codec's 60 ms frames, the phone ~20 ms blocks)
//           0x03: one Opus packet, 16 kHz mono, 20 ms (bbopus.go)
//
//   Codec: PCM until the phone sends {"type":"audio.codec","codec":"opus"};
//   the server answers the same message and from then on sends Opus (and
//   takes both kinds from the phone). ~24 kbit/s a way instead of 256.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"

	"github.com/coder/websocket"
	"go.mau.fi/whatsmeow/types"
)

const (
	bbOwner        = "berrybridge"
	bbHeaderLen    = 12
	bbKindAudio    = 0x01
	bbKindOpus     = 0x03
	bbQueueFrames  = 64 // writer queue; audio beyond it is dropped, never queued up
	bbWriteTimeout = 5 * time.Second
)

type bbGateway struct {
	sessions  *SessionManager
	broker    *Broker
	log       *slog.Logger
	tokenHash [32]byte
	sessionID string // empty: the first paired session

	mu     sync.Mutex
	conn   *bbConn // the phone connected now (a new connection replaces it)
	callID string  // the call whose audio goes to/from the phone
	muted  atomic.Bool

	agc      *bbAGC    // peer -> phone levelling, see bbagc.go
	echo     *bbEcho   // the user's voice coming back from a hands-free peer, see bbecho.go
	upAgc    *bbAGC    // phone mic -> peer levelling
	upEcho   *bbUpEcho // the phone's own loudspeaker in its mic, see bbupecho.go
	upRawSum float64   // mic level before the canceller, for the report
	upRawN   int
	// Uplink timing since the last report: mic frames arriving more than
	// 80 ms after the previous one (the phone sends every 20 ms), the
	// longest gap, and silence the call core sent because no mic audio
	// came for 120 ms. Any of these is a gap the peer hears.
	upLastAt    time.Time
	upLate      int
	upMaxGap    time.Duration
	upKeepalive int
	lastReport  time.Time
}

func newBBGateway(sessions *SessionManager, broker *Broker, token, sessionID string, log *slog.Logger) *bbGateway {
	return &bbGateway{
		sessions:  sessions,
		broker:    broker,
		log:       log.With("component", "bbgateway"),
		tokenHash: sha256.Sum256([]byte(strings.TrimSpace(token))),
		sessionID: sessionID,
	}
}

type bbConn struct {
	ws     *websocket.Conn
	out    chan bbMsg
	ctx    context.Context
	cancel context.CancelFunc
	echo   atomic.Bool
	seq    atomic.Uint32
	start  time.Time
	sub    *subscriber

	// Opus, once the phone asked for it (audio.codec). enc and pending are
	// used by the call core's writer goroutine, dec by the reader.
	opus    atomic.Bool
	codecMu sync.Mutex
	enc     *bbOpusEncoder
	dec     *bbOpusDecoder
	pending []int16
}

type bbMsg struct {
	typ  websocket.MessageType
	data []byte
}

func (g *bbGateway) authorized(r *http.Request) bool {
	tok := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if tok == "" {
		return false
	}
	got := sha256.Sum256([]byte(tok))
	return subtle.ConstantTimeCompare(got[:], g.tokenHash[:]) == 1
}

func (g *bbGateway) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", g.handleWS)
	return mux
}

func (g *bbGateway) handleWS(w http.ResponseWriter, r *http.Request) {
	if !g.authorized(r) {
		time.Sleep(500 * time.Millisecond) // slows down guessing
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true}) // not a browser: no Origin
	if err != nil {
		return
	}
	ws.SetReadLimit(1 << 16)
	ctx, cancel := context.WithCancel(context.Background())
	c := &bbConn{ws: ws, out: make(chan bbMsg, bbQueueFrames), ctx: ctx, cancel: cancel, start: time.Now()}

	g.mu.Lock()
	old := g.conn
	g.conn = c
	if g.upEcho != nil {
		g.upEcho.resync() // mid-call: audio sent to the old connection was lost
	}
	g.mu.Unlock()
	if old != nil {
		old.cancel()
		_ = old.ws.Close(websocket.StatusPolicyViolation, "replaced by a newer connection")
	}
	g.log.Info("phone connected", "remote", r.RemoteAddr)

	go g.writer(c)
	c.sub = g.broker.subscribe("berrybridge-gateway")
	go g.forwardEvents(c)
	g.sendHello(c)
	g.reader(c)

	cancel()
	g.broker.unsubscribe(c.sub)
	c.codecMu.Lock()
	c.enc.close()
	c.dec.close()
	c.codecMu.Unlock()
	g.mu.Lock()
	if g.conn == c {
		g.conn = nil
	}
	g.mu.Unlock()
	_ = ws.Close(websocket.StatusNormalClosure, "")
	g.log.Info("phone disconnected")
}

func (g *bbGateway) writer(c *bbConn) {
	for {
		select {
		case <-c.ctx.Done():
			return
		case m := <-c.out:
			wctx, cancel := context.WithTimeout(c.ctx, bbWriteTimeout)
			err := c.ws.Write(wctx, m.typ, m.data)
			cancel()
			if err != nil {
				c.cancel()
				return
			}
		}
	}
}

// enqueue never blocks: audio that can't be queued is dropped (it would be
// stale anyway); control messages get a short grace period.
func (c *bbConn) enqueue(m bbMsg) bool {
	select {
	case c.out <- m:
		return true
	default:
	}
	if m.typ == websocket.MessageText {
		select {
		case c.out <- m:
			return true
		case <-time.After(time.Second):
		case <-c.ctx.Done():
		}
	}
	return false
}

func (c *bbConn) sendJSON(v any) {
	data, err := json.Marshal(v)
	if err == nil {
		c.enqueue(bbMsg{websocket.MessageText, data})
	}
}

func (g *bbGateway) current() *bbConn {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.conn
}

// session picks the WhatsApp account the phone works with.
func (g *bbGateway) session() (*Session, error) {
	if g.sessions == nil {
		return nil, errors.New("no session manager")
	}
	if g.sessionID != "" {
		if s, ok := g.sessions.Get(g.sessionID); ok {
			return s, nil
		}
		return nil, errors.New("session not found")
	}
	for _, info := range g.sessions.infos() {
		if info.Paired {
			if s, ok := g.sessions.Get(info.ID); ok {
				return s, nil
			}
		}
	}
	return nil, errors.New("no paired WhatsApp session")
}

// phoneOf turns a peer JID into "+<number>" when it can: directly for a
// phone-number JID, through whatsmeow's LID map for a LID (privacy) JID.
func (g *bbGateway) phoneOf(sess *Session, jid string) string {
	j, err := types.ParseJID(jid)
	if err != nil {
		return ""
	}
	if j.Server == types.HiddenUserServer && sess != nil && sess.client.Store.LIDs != nil {
		if pn, err := sess.client.Store.LIDs.GetPNForLID(context.Background(), j); err == nil && !pn.IsEmpty() {
			j = pn
		}
	}
	if j.Server == types.DefaultUserServer && j.User != "" {
		return "+" + j.User
	}
	return ""
}

func (g *bbGateway) sendHello(c *bbConn) {
	sess, err := g.session()
	hello := map[string]any{"type": "hello", "version": 1, "paired": err == nil}
	calls := []map[string]any{}
	if sess != nil {
		hello["session"] = sess.id
		g.broker.mu.RLock()
		for _, rec := range g.broker.calls {
			if rec.SessionID == sess.id && rec.Status != StatusEnded {
				calls = append(calls, map[string]any{
					"callId": rec.CallID, "state": rec.Status, "direction": rec.Direction,
					"peer": rec.Peer, "phone": g.phoneOf(sess, rec.Peer),
				})
			}
		}
		g.broker.mu.RUnlock()
	}
	hello["calls"] = calls
	c.sendJSON(hello)
}

// forwardEvents turns the broker's call events (the ones the browser gets over
// SSE) into the phone's protocol.
func (g *bbGateway) forwardEvents(c *bbConn) {
	for data := range c.sub.ch {
		var ev map[string]any
		if json.Unmarshal(data, &ev) != nil {
			continue
		}
		sess, _ := g.session()
		if sess == nil || ev["sessionId"] != sess.id {
			continue
		}
		id, _ := ev["id"].(string)
		peer, _ := ev["peer"].(string)
		switch ev["type"] {
		case "incoming":
			c.sendJSON(map[string]any{"type": "call.incoming", "callId": id, "peer": peer, "phone": g.phoneOf(sess, peer)})
		case "call-status":
			c.sendJSON(map[string]any{"type": "call.state", "callId": id, "state": ev["status"],
				"direction": ev["direction"], "peer": peer, "phone": g.phoneOf(sess, peer)})
		case "call-ended":
			g.mu.Lock()
			if g.callID == id {
				g.callID = ""
			}
			g.mu.Unlock()
			c.sendJSON(map[string]any{"type": "call.ended", "callId": id, "reason": ev["reason"]})
		}
	}
}

func (g *bbGateway) reader(c *bbConn) {
	for {
		typ, data, err := c.ws.Read(c.ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			g.onAudio(c, data)
			continue
		}
		var msg map[string]any
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		g.onControl(c, msg)
	}
}

func (g *bbGateway) onAudio(c *bbConn, frame []byte) {
	if len(frame) < bbHeaderLen || (frame[0] != bbKindAudio && frame[0] != bbKindOpus) {
		return
	}
	if c.echo.Load() {
		c.enqueue(bbMsg{websocket.MessageBinary, append([]byte(nil), frame...)})
		return
	}
	if g.muted.Load() {
		return // the call core sends comfort silence by itself after 120 ms
	}
	g.mu.Lock()
	callID := g.callID
	g.mu.Unlock()
	if callID == "" {
		return
	}
	sess, err := g.session()
	if err != nil {
		return
	}
	if ac, ok := sess.reg.get(callID); ok {
		var pcm []float32
		if frame[0] == bbKindOpus {
			c.codecMu.Lock()
			dec := c.dec
			var samples []int16
			if dec != nil {
				samples, err = dec.decode(frame[bbHeaderLen:])
			}
			c.codecMu.Unlock()
			if dec == nil || err != nil {
				return
			}
			pcm = make([]float32, len(samples))
			for i, v := range samples {
				pcm[i] = float32(v) / 32768
			}
		} else {
			pcm = media.PCMInt16LEToFloat32(frame[bbHeaderLen:])
		}
		g.mu.Lock()
		now := time.Now()
		if !g.upLastAt.IsZero() {
			gap := now.Sub(g.upLastAt)
			if gap > 80*time.Millisecond {
				g.upLate++
			}
			if gap > g.upMaxGap {
				g.upMaxGap = gap
			}
		}
		g.upLastAt = now
		for _, s := range pcm {
			g.upRawSum += float64(s) * float64(s)
		}
		g.upRawN += len(pcm)
		upEcho := g.upEcho
		g.mu.Unlock()
		// Flag 0x01: [2:4] holds the phone's playback position.
		if upEcho != nil && frame[1]&0x01 != 0 {
			upEcho.mic(pcm, binary.LittleEndian.Uint16(frame[2:4]))
		}
		g.mu.Lock()
		if g.upAgc == nil {
			g.upAgc = newBBAGCUp()
		}
		g.upAgc.process(pcm)
		g.mu.Unlock()
		ac.cm.FeedCapturedPCM(pcm)
	}
}

func (g *bbGateway) onControl(c *bbConn, msg map[string]any) {
	typ, _ := msg["type"].(string)
	str := func(k string) string { s, _ := msg[k].(string); return strings.TrimSpace(s) }
	fail := func(err error) {
		c.sendJSON(map[string]any{"type": "error", "for": typ, "message": err.Error()})
	}
	switch typ {
	case "ping":
		c.sendJSON(map[string]any{"type": "pong", "t": msg["t"]})
	case "audio.codec":
		if str("codec") != "opus" {
			c.opus.Store(false)
			c.sendJSON(map[string]any{"type": "audio.codec", "codec": "pcm"})
			return
		}
		c.codecMu.Lock()
		var err error
		if c.enc == nil {
			c.enc, err = newBBOpusEncoder()
		}
		if err == nil && c.dec == nil {
			c.dec, err = newBBOpusDecoder()
		}
		c.codecMu.Unlock()
		if err != nil {
			g.log.Warn("opus unavailable, staying on PCM", "err", err)
			c.sendJSON(map[string]any{"type": "audio.codec", "codec": "pcm"})
			return
		}
		c.opus.Store(true)
		g.log.Info("phone audio codec", "codec", "opus")
		c.sendJSON(map[string]any{"type": "audio.codec", "codec": "opus"})
	case "test.echo":
		on, _ := msg["on"].(bool)
		c.echo.Store(on)
	case "call.mute":
		m, _ := msg["muted"].(bool)
		g.muted.Store(m)
	case "call.start":
		sess, err := g.session()
		if err != nil {
			fail(err)
			return
		}
		phone := normalizePhone(str("phone"))
		if phone == "" {
			fail(errors.New("phone required"))
			return
		}
		peer := types.NewJID(phone, types.DefaultUserServer)
		callID, err := sess.startOutgoing(c.ctx, peer, false)
		if err != nil {
			fail(err)
			return
		}
		owner := bbOwner
		g.broker.upsertCall(CallRecord{SessionID: sess.id, CallID: callID, Owner: &owner, Direction: "outbound",
			Peer: peer.String(), StartedAt: time.Now().UnixMilli(), Status: StatusRinging})
		g.attach(sess, callID)
		c.sendJSON(map[string]any{"type": "call.started", "callId": callID})
	case "call.accept":
		sess, err := g.session()
		if err != nil {
			fail(err)
			return
		}
		id := str("callId")
		ac, ok := sess.reg.get(id)
		if !ok {
			fail(errors.New("no such call"))
			return
		}
		if !g.broker.setOwner(id, bbOwner) {
			fail(errors.New("claimed by another client"))
			return
		}
		g.broker.emitIncomingClaimed(sess.id, id, bbOwner)
		g.attach(sess, id)
		if err := ac.cm.AcceptCall(c.ctx, id); err != nil {
			fail(err)
		}
	case "call.reject":
		sess, err := g.session()
		if err != nil {
			fail(err)
			return
		}
		id := str("callId")
		if ac, ok := sess.reg.get(id); ok {
			_ = ac.cm.RejectCall(c.ctx, id, core.EndCallReasonDeclined)
		}
		sess.removeCall(id)
		g.broker.endCall(id, string(core.EndCallReasonDeclined))
	case "call.hangup":
		sess, err := g.session()
		if err != nil {
			fail(err)
			return
		}
		id := str("callId")
		if ac, ok := sess.reg.get(id); ok {
			_ = ac.cm.EndCall(c.ctx, core.EndCallReasonUserEnded)
		}
		sess.removeCall(id)
		g.broker.endCall(id, string(core.EndCallReasonUserEnded))
	}
}

// attach routes a call's audio through the phone: whatever connection is
// current when audio flows (it survives the phone reconnecting mid-call).
func (g *bbGateway) attach(sess *Session, callID string) {
	g.mu.Lock()
	g.callID = callID
	g.agc = newBBAGC()
	if g.echo != nil {
		g.echo.close()
	}
	g.echo = newBBEcho()
	if g.upEcho != nil {
		g.upEcho.close()
	}
	g.upEcho = newBBUpEcho()
	g.upAgc = newBBAGCUp()
	g.upRawSum, g.upRawN = 0, 0
	g.upLastAt, g.upLate, g.upMaxGap, g.upKeepalive = time.Time{}, 0, 0, 0
	g.lastReport = time.Now()
	g.mu.Unlock()
	g.muted.Store(false)
	sess.setBridge(callID, &bbBridge{g: g})
}

// bbBridge is the call core's view of the phone (mediaBridge).
type bbBridge struct{ g *bbGateway }

func (b *bbBridge) WritePCM(pcm []float32) error {
	c := b.g.current()
	if c == nil || len(pcm) == 0 {
		return nil
	}
	b.g.mu.Lock()
	echo := b.g.echo
	b.g.mu.Unlock()
	if echo != nil {
		echo.process(pcm)
	}
	b.g.levelAndReport(pcm)
	b.g.mu.Lock()
	upEcho := b.g.upEcho
	b.g.mu.Unlock()
	if upEcho != nil {
		upEcho.toPhone(pcm) // as sent: the canceller's reference
	}
	if c.opus.Load() {
		b.g.writeOpus(c, pcm)
		return nil
	}
	frame := make([]byte, bbHeaderLen, bbHeaderLen+2*len(pcm))
	frame[0] = bbKindAudio
	binary.LittleEndian.PutUint32(frame[4:], c.seq.Add(1))
	binary.LittleEndian.PutUint32(frame[8:], uint32(time.Since(c.start).Milliseconds()))
	frame = append(frame, media.PCMFloat32ToInt16LE(pcm)...)
	c.enqueue(bbMsg{websocket.MessageBinary, frame})
	return nil
}

func (b *bbBridge) Close() {}

// SentPCM is the call core's audio to the peer: the echo canceller's reference.
func (b *bbBridge) SentPCM(pcm []float32) {
	b.g.mu.Lock()
	echo := b.g.echo
	if isSilence(pcm) {
		b.g.upKeepalive++
	}
	b.g.mu.Unlock()
	if echo != nil {
		echo.sent(pcm)
	}
}

func isSilence(pcm []float32) bool {
	for _, s := range pcm {
		if s != 0 {
			return false
		}
	}
	return true
}

// writeOpus cuts the call core's 60 ms frames into 20 ms Opus packets.
func (g *bbGateway) writeOpus(c *bbConn, pcm []float32) {
	c.codecMu.Lock()
	defer c.codecMu.Unlock()
	if c.enc == nil {
		return
	}
	for _, s := range pcm {
		v := s * 32768
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		c.pending = append(c.pending, int16(v))
	}
	for len(c.pending) >= bbOpusFrame {
		frame := make([]byte, bbHeaderLen, bbHeaderLen+128)
		frame[0] = bbKindOpus
		binary.LittleEndian.PutUint32(frame[4:], c.seq.Add(1))
		binary.LittleEndian.PutUint32(frame[8:], uint32(time.Since(c.start).Milliseconds()))
		var err error
		frame, err = c.enc.encode(c.pending[:bbOpusFrame], frame)
		c.pending = c.pending[bbOpusFrame:]
		if err != nil {
			continue
		}
		c.enqueue(bbMsg{websocket.MessageBinary, frame})
	}
	if len(c.pending) == 0 {
		c.pending = nil // let the backing array go
	}
}

// levelAndReport runs the peer audio through the AGC and logs the call's
// levels every 10 s (peer in/out of the AGC, phone mic) for tuning.
func (g *bbGateway) levelAndReport(pcm []float32) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.agc == nil {
		g.agc = newBBAGC()
		g.lastReport = time.Now()
	}
	g.agc.process(pcm)
	if time.Since(g.lastReport) < 10*time.Second {
		return
	}
	g.lastReport = time.Now()
	if in, out, gain, ok := g.agc.report(); ok {
		raw, up, toPeer, micGain := -120.0, -120.0, -120.0, 0.0
		if g.upRawN > 0 {
			raw = 10 * math.Log10(g.upRawSum/float64(g.upRawN)+1e-12)
		}
		g.upRawSum, g.upRawN = 0, 0
		if g.upAgc != nil {
			if i, o, gn, ok := g.upAgc.report(); ok {
				up, toPeer, micGain = i, o, gn
			}
		}
		defer func() { g.upLate, g.upMaxGap, g.upKeepalive = 0, 0, 0 }()
		upLag, upFrames, upCorr := -1, 0, 0.0
		if g.upEcho != nil {
			upLag = g.upEcho.lagMs()
			upFrames, upCorr = g.upEcho.report()
		}
		echoLag := -1
		if g.echo != nil {
			echoLag = g.echo.lagMs()
		}
		g.log.Info("call levels", "peer_dbfs", int(in), "to_phone_dbfs", int(out), "agc_db", int(gain), "phone_mic_dbfs", int(raw),
			"mic_after_aec_dbfs", int(up), "to_peer_dbfs", int(toPeer), "mic_agc_db", int(micGain),
			"phone_echo_lag_ms", upLag, "phone_echo_corr", math.Round(upCorr*100)/100, "pos_frames", upFrames,
			"mic_late", g.upLate, "mic_max_gap_ms", g.upMaxGap.Milliseconds(), "keepalive_frames", g.upKeepalive,
			"echo_lag_ms", echoLag)
	}
}
