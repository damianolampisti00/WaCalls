package main

// RTP audio for the SIP gateway: G.711 mu-law (PCMU, payload type 0) at 8 kHz
// in 20 ms packets, the codec BlackBerry's BBM Video engine (EBMService)
// accepts from an unknown peer. The call core speaks 16 kHz float PCM, so
// this file also resamples 16 <-> 8 kHz.
//
// The phone's engine runs its own echo canceller, so none is needed here.

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"wacalls/internal/voip/media"
)

// ---- G.711 mu-law ----------------------------------------------------------

func ulawEncode(s int16) byte {
	const bias, clip = 0x84, 32635
	sample := int(s)
	sign := 0
	if sample < 0 {
		sign = 0x80
		sample = -sample
	}
	if sample > clip {
		sample = clip
	}
	sample += bias
	exp := 7
	for mask := 0x4000; exp > 0 && sample&mask == 0; mask >>= 1 {
		exp--
	}
	mant := (sample >> (exp + 3)) & 0x0f
	return ^byte(sign | exp<<4 | mant)
}

func ulawDecode(u byte) int16 {
	u = ^u
	sign := u & 0x80
	exp := int(u>>4) & 0x07
	mant := int(u & 0x0f)
	v := ((mant << 3) + 0x84) << exp
	v -= 0x84
	if sign != 0 {
		return int16(-v)
	}
	return int16(v)
}

// ---- 16 <-> 8 kHz ----------------------------------------------------------

// A windowed-sinc low-pass at 3.6 kHz for 16 kHz audio: the anti-alias filter
// before dropping every other sample, and the image filter after inserting zeros.
var halfbandTaps = func() []float32 {
	const n = 31
	const fc = 3600.0 / 16000.0
	taps := make([]float32, n)
	var sum float64
	for i := 0; i < n; i++ {
		x := float64(i - n/2)
		v := 2 * fc
		if x != 0 {
			v = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
		}
		v *= 0.54 - 0.46*math.Cos(2*math.Pi*float64(i)/float64(n-1)) // Hamming
		taps[i] = float32(v)
		sum += v
	}
	for i := range taps {
		taps[i] /= float32(sum)
	}
	return taps
}()

type firState struct{ hist []float32 }

func (f *firState) run(in []float32) []float32 {
	n := len(halfbandTaps)
	if f.hist == nil {
		f.hist = make([]float32, n-1)
	}
	buf := append(append([]float32(nil), f.hist...), in...)
	out := make([]float32, len(in))
	for i := range in {
		var acc float32
		for k, t := range halfbandTaps {
			acc += t * buf[i+n-1-k]
		}
		out[i] = acc
	}
	f.hist = append(f.hist[:0], buf[len(buf)-(n-1):]...)
	return out
}

type downsampler struct {
	fir   firState
	phase int
}

// process 16 kHz -> 8 kHz.
func (d *downsampler) process(in []float32) []float32 {
	f := d.fir.run(in)
	out := make([]float32, 0, len(in)/2+1)
	for _, v := range f {
		if d.phase == 0 {
			out = append(out, v)
		}
		d.phase ^= 1
	}
	return out
}

type upsampler struct{ fir firState }

// process 8 kHz -> 16 kHz.
func (u *upsampler) process(in []float32) []float32 {
	z := make([]float32, 2*len(in))
	for i, v := range in {
		z[2*i] = 2 * v
	}
	return u.fir.run(z)
}

// ---- SDP -------------------------------------------------------------------

type sdpAudio struct {
	IP      string
	Port    int
	Formats []int          // payload types of m=audio, in order
	RTPMap  map[int]string // "PCMU/8000"
	Video   bool           // the offer also had a video stream
	// ICE bits of the phone's SDP (its connectivity checks, sipstun.go)
	Ufrag, Pwd string
	RTCPPort   int // a=rtcp; 0: RTP port + 1
	// the phone's own ports (host candidates, components 1 and 2): with a
	// STUN server the engine puts its public address in c=/m=, so through
	// the Berry Bridge relay (same phone) only these reach it
	HostPort, HostRTCPPort int
	// the video stream (m=video), when the phone has one
	VideoPort, VideoPT, VideoRTCPPort, VideoHostPort, VideoHostRTCPPort int
	Raw                                                                 string
}

func parseSDP(body []byte) (*sdpAudio, error) {
	s := &sdpAudio{RTPMap: map[int]string{}}
	media := ""
	for _, l := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "c=IN IP4 "):
			ip := strings.Fields(l[len("c=IN IP4 "):])
			if len(ip) > 0 && (s.IP == "" || media == "audio") {
				s.IP = strings.Split(ip[0], "/")[0]
			}
		case strings.HasPrefix(l, "m="):
			f := strings.Fields(l[2:])
			media = f[0]
			if media == "video" && len(f) > 1 && f[1] != "0" {
				s.Video = true
				s.VideoPort, _ = strconv.Atoi(f[1])
				if len(f) >= 4 {
					s.VideoPT, _ = strconv.Atoi(f[3])
				}
			}
			if media == "audio" && len(f) >= 3 && s.Port == 0 {
				s.Port, _ = strconv.Atoi(f[1])
				for _, p := range f[3:] {
					if pt, err := strconv.Atoi(p); err == nil {
						s.Formats = append(s.Formats, pt)
					}
				}
			}
		case strings.HasPrefix(l, "a=ice-ufrag:") && (media == "" || media == "audio"):
			s.Ufrag = strings.TrimSpace(l[len("a=ice-ufrag:"):])
		case strings.HasPrefix(l, "a=ice-pwd:") && (media == "" || media == "audio"):
			s.Pwd = strings.TrimSpace(l[len("a=ice-pwd:"):])
		case strings.HasPrefix(l, "a=candidate:") && (media == "audio" || media == "video"):
			// a=candidate:<foundation> <component> UDP <prio> <ip> <port> typ host
			f := strings.Fields(l[len("a=candidate:"):])
			if len(f) >= 8 && f[6] == "typ" && f[7] == "host" && strings.EqualFold(f[2], "udp") {
				if port, err := strconv.Atoi(f[5]); err == nil {
					rtp, rtcp := &s.HostPort, &s.HostRTCPPort
					if media == "video" {
						rtp, rtcp = &s.VideoHostPort, &s.VideoHostRTCPPort
					}
					switch {
					case f[1] == "1" && *rtp == 0:
						*rtp = port
					case f[1] == "2" && *rtcp == 0:
						*rtcp = port
					}
				}
			}
		case strings.HasPrefix(l, "a=rtcp:") && media == "audio":
			s.RTCPPort, _ = strconv.Atoi(strings.Fields(l[len("a=rtcp:"):] + " ")[0])
		case strings.HasPrefix(l, "a=rtcp:") && media == "video":
			s.VideoRTCPPort, _ = strconv.Atoi(strings.Fields(l[len("a=rtcp:"):] + " ")[0])
		case strings.HasPrefix(l, "a=rtpmap:") && media == "audio":
			pt, enc, ok := strings.Cut(l[len("a=rtpmap:"):], " ")
			if n, err := strconv.Atoi(pt); ok && err == nil {
				s.RTPMap[n] = strings.TrimSpace(enc)
			}
		}
	}
	if s.IP == "" || s.Port == 0 {
		return nil, errors.New("sdp: no audio stream")
	}
	s.Raw = strings.ReplaceAll(strings.TrimSpace(string(body)), "\r\n", " | ")
	return s, nil
}

// sipCodec is the audio of one call: PCMU (G.711 mu-law, 8 kHz) or Opus.
// The phone's engine names Opus its own way: "126 OPUS/16000" (OPUS-WB),
// the RTP clock being the sampling rate; RFC 7587's "opus/48000/2" is
// understood too (clock 48 kHz). Our Opus is 16 kHz mono, 20 ms, 24 kbit/s
// (bbopus.go): about a quarter of PCMU's bytes, for a weak link.
type sipCodec struct {
	PT    int
	Opus  bool
	Clock int // RTP timestamp rate
}

var (
	codecPCMU   = sipCodec{PT: 0, Clock: 8000}
	codecOpusWB = sipCodec{PT: 126, Opus: true, Clock: 16000}
)

func (c sipCodec) String() string {
	if c.Opus {
		return fmt.Sprintf("opus/%d pt%d", c.Clock, c.PT)
	}
	return "pcmu"
}

// rtpmap is the codec's a=rtpmap value.
func (c sipCodec) rtpmap() string {
	if c.Opus {
		if c.Clock == 48000 {
			return "opus/48000/2"
		}
		return fmt.Sprintf("OPUS/%d", c.Clock)
	}
	return "PCMU/8000"
}

// pickCodec chooses our codec from the other side's formats: Opus at 16 or
// 48 kHz when allowed and present (in the order they are listed), else PCMU
// (payload type 0 is static PCMU even without an rtpmap line).
func (s *sdpAudio) pickCodec(opus bool) (sipCodec, bool) {
	pcmu := false
	for _, pt := range s.Formats {
		if pt == 0 {
			pcmu = true
			continue
		}
		name, rate, _ := strings.Cut(strings.ToLower(s.RTPMap[pt]), "/")
		rate, _, _ = strings.Cut(rate, "/")
		if opus && name == "opus" && (rate == "16000" || rate == "48000") {
			clock, _ := strconv.Atoi(rate)
			return sipCodec{PT: pt, Opus: true, Clock: clock}, true
		}
	}
	return codecPCMU, pcmu
}

// hasPCMU: payload type 0 (static PCMU) is among the formats.
func (s *sdpAudio) hasPCMU() bool {
	_, ok := s.pickCodec(false)
	return ok
}

// buildSDP: one audio stream with the given codecs (in preference order),
// 20 ms, sendrecv. rejectVideo adds a refused video stream (port 0), for an
// answer to an offer that had one.
func buildSDP(ip string, port int, sessID int64, rejectVideo bool, codecs ...sipCodec) []byte {
	return buildSDPFull(ip, port, sessID, rejectVideo, false, codecs...)
}

// sipVideoPT: H.264 toward the phone's engine, as its own offers name it.
const sipVideoPT = 109

// buildSDPFull: buildSDP, plus (video) an H.264 stream for the phone's
// engine on the same port as the audio (through the Berry Bridge relay the
// phone reaches only that one; RTP, RTCP and video are told apart by
// payload type). The m= line is the one EBMService accepted on 2026-11-02.
func buildSDPFull(ip string, port int, sessID int64, rejectVideo, video bool, codecs ...sipCodec) []byte {
	if len(codecs) == 0 {
		codecs = []sipCodec{codecPCMU}
	}
	pts := make([]string, len(codecs))
	maps := make([]string, len(codecs))
	for i, c := range codecs {
		pts[i] = strconv.Itoa(c.PT)
		maps[i] = fmt.Sprintf("a=rtpmap:%d %s", c.PT, c.rtpmap())
	}
	lines := []string{
		"v=0",
		fmt.Sprintf("o=wacalls %d %d IN IP4 %s", sessID, sessID, ip),
		"s=WaCalls",
		"c=IN IP4 " + ip,
		"t=0 0",
		fmt.Sprintf("m=audio %d RTP/AVP %s", port, strings.Join(pts, " ")),
	}
	lines = append(lines, maps...)
	// RTCP on the RTP port: the engine starts its audio only once the
	// connectivity check of both its components (RTP and RTCP) succeeds, and
	// at port+1 nothing listens (the Berry Bridge relay has one port), so
	// the RTCP one only timed out after ~5 s. There it reaches us and its
	// probes are echoed (it knows no rtcp-mux, but reads a=rtcp).
	lines = append(lines, fmt.Sprintf("a=rtcp:%d", port), "a=ptime:20", "a=sendrecv")
	switch {
	case video:
		lines = append(lines, fmt.Sprintf("m=video %d RTP/AVP %d", port, sipVideoPT),
			fmt.Sprintf("a=rtpmap:%d H264/90000", sipVideoPT),
			fmt.Sprintf("a=fmtp:%d profile-level-id=42e01f;packetization-mode=1", sipVideoPT),
			fmt.Sprintf("a=rtcp:%d", port), "a=sendrecv")
	case rejectVideo:
		lines = append(lines, "m=video 0 RTP/AVP 109")
	}
	return []byte(strings.Join(lines, "\r\n") + "\r\n")
}

// ---- RTP session -----------------------------------------------------------

const (
	rtpFrame = 160 // 20 ms at 8 kHz (PCMU); Opus frames are bbOpusFrame at 16 kHz
	// WhatsApp's audio comes in 60 ms blocks (or 120 ms two-frame blocks in
	// video calls): start (and restart after a gap) with 140 ms queued, or
	// every block boundary is an underrun.
	rtpPrebufferFrames   = 7
	rtpMaxBufferedFrames = 20 // 400 ms: beyond this the oldest audio goes
)

// rtpTransport carries RTP datagrams: a UDP socket (udpTransport) or the
// Berry Bridge link when the phone reaches us through it (tunnelRTP).
type rtpTransport interface {
	WriteTo(p []byte, dst *net.UDPAddr) (int, error)
	ReadFrom(p []byte) (int, *net.UDPAddr, error) // blocks; an error once closed
	Close() error
	LocalPort() int
}

type udpTransport struct{ *net.UDPConn }

func (u udpTransport) WriteTo(p []byte, dst *net.UDPAddr) (int, error) { return u.WriteToUDP(p, dst) }
func (u udpTransport) ReadFrom(p []byte) (int, *net.UDPAddr, error)    { return u.ReadFromUDP(p) }
func (u udpTransport) LocalPort() int                                  { return u.LocalAddr().(*net.UDPAddr).Port }

// sipRTP is one call's media: a socket on our side, the phone's address
// from its SDP (then whatever address its packets really come from).
type sipRTP struct {
	conn   rtpTransport
	log    *slog.Logger
	remote atomic.Pointer[net.UDPAddr]
	ssrc   uint32

	codec sipCodec
	// the phone's ICE credentials and RTCP port, for our connectivity checks
	iceUfrag, icePwd string
	rtcpPort         int
	extraChecks      []int     // more of the phone's ports for ICE checks (its video's)
	video            *sipVideo // the peer's video to the phone, when negotiated
	phoneVideoPT     int
	phoneVideoDepack media.H264Depacketizer
	phoneVideoRot    int
	onPhoneVideo     func(media.AccessUnit)
	frame            int // samples per 20 ms packet in down: 160 (PCMU, 8 kHz) or 320 (Opus, 16 kHz)
	enc              *bbOpusEncoder
	dec              *bbOpusDecoder

	mu      sync.Mutex
	down    []float32 // audio waiting to be sent, at the codec's rate
	started bool
	ds      downsampler
	us      upsampler
	onUp    func(pcm16 []float32)

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	sent, recv, underruns, dropped atomic.Int64
}

// listenRTP opens a UDP socket on ip at the first free even port of [lo, hi].
func listenRTP(ip string, lo, hi int) (rtpTransport, error) {
	for p := lo &^ 1; p <= hi; p += 2 {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(ip), Port: p})
		if err == nil {
			return udpTransport{c}, nil
		}
	}
	return nil, fmt.Errorf("rtp: no free port in %d-%d on %s", lo, hi, ip)
}

func newSIPRTP(conn rtpTransport, remote *net.UDPAddr, codec sipCodec, onUp func([]float32), log *slog.Logger) *sipRTP {
	r := &sipRTP{conn: conn, log: log, onUp: onUp, stop: make(chan struct{}), codec: codec, frame: rtpFrame}
	if codec.Opus {
		var err error
		if r.enc, err = newBBOpusEncoder(); err == nil {
			r.dec, err = newBBOpusDecoder()
		}
		if err != nil { // no libopus: G.711 would not match the SDP either, so say so
			log.Error("sip rtp: opus unavailable", "err", err)
			r.enc.close()
			r.enc = nil
		}
		r.frame = bbOpusFrame
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	r.ssrc = binary.BigEndian.Uint32(b[:])
	r.remote.Store(remote)
	return r
}

func (r *sipRTP) localPort() int { return r.conn.LocalPort() }

func (r *sipRTP) start() {
	r.wg.Add(2)
	go r.sender()
	go r.receiver()
}

func (r *sipRTP) close() {
	r.stopOnce.Do(func() {
		close(r.stop)
		if r.video != nil {
			r.video.close()
		}
		_ = r.conn.Close()
		r.wg.Wait()
		r.enc.close()
		if r.dec != nil {
			r.dec.close()
		}
		r.log.Info("sip rtp closed", "codec", r.codec.String(), "sent", r.sent.Load(), "received", r.recv.Load(),
			"underruns", r.underruns.Load(), "dropped_ms", r.dropped.Load()*20/int64(r.frame))
	})
}

// WritePCM queues the peer's 16 kHz audio for the phone (mediaBridge).
func (r *sipRTP) WritePCM(pcm []float32) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.codec.Opus {
		r.down = append(r.down, pcm...) // already 16 kHz
	} else {
		r.down = append(r.down, r.ds.process(pcm)...)
	}
	if over := len(r.down) - rtpMaxBufferedFrames*r.frame; over > 0 {
		r.down = r.down[over:]
		r.dropped.Add(int64(over))
	}
	return nil
}

func (r *sipRTP) Close() {}

// WriteVideo takes one of the peer's video frames (session.go).
func (r *sipRTP) WriteVideo(au media.AccessUnit) {
	if v := r.video; v != nil {
		v.feed(au)
	}
}

// sender sends one packet every 20 ms from the start: audio when there is
// some, silence otherwise. Sending first also opens the server's stateful
// firewall for the phone's packets.
func (r *sipRTP) sender() {
	defer r.wg.Done()
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	var seq uint16 = uint16(r.ssrc)
	var ts uint32 = r.ssrc ^ 0x5a5a5a5a
	pkt := make([]byte, 12, 12+bbOpusMaxPkt)
	frame := make([]float32, r.frame)
	pcm := make([]int16, r.frame)
	tsStep := uint32(r.codec.Clock / 50)
	start, tick := time.Now(), 0
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
		}
		if (tick < 15 || tick%probeEvery == 0) && time.Since(start) < probeFor {
			if dst := r.remote.Load(); dst != nil {
				probe := probePacket(uint32(time.Since(start).Milliseconds()))
				stunReq := stunBindingRequestFor(r.iceUfrag, r.icePwd)
				if r.recv.Load() == 0 {
					_, _ = r.conn.WriteTo(probe, dst)
				}
				// ICE checks on both components (sipstun.go)
				_, _ = r.conn.WriteTo(stunReq, dst)
				rtcp := r.rtcpPort
				if rtcp == 0 {
					rtcp = dst.Port + 1
				}
				rtcpAddr := &net.UDPAddr{IP: dst.IP, Port: rtcp}
				_, _ = r.conn.WriteTo(probe, rtcpAddr)
				_, _ = r.conn.WriteTo(stunReq, rtcpAddr)
				for _, p := range r.extraChecks {
					extraAddr := &net.UDPAddr{IP: dst.IP, Port: p}
					_, _ = r.conn.WriteTo(probe, extraAddr)
					_, _ = r.conn.WriteTo(stunReq, extraAddr)
				}
			}
		}
		tick++
		r.mu.Lock()
		if !r.started && len(r.down) >= rtpPrebufferFrames*r.frame {
			r.started = true
		}
		if r.started && len(r.down) >= r.frame {
			copy(frame, r.down[:r.frame])
			r.down = r.down[r.frame:]
		} else {
			if r.started {
				r.underruns.Add(1)
				r.started = false // refill the prebuffer after a gap
			}
			for i := range frame {
				frame[i] = 0
			}
		}
		r.mu.Unlock()
		pkt = pkt[:12]
		pkt[0] = 0x80
		pkt[1] = byte(r.codec.PT)
		binary.BigEndian.PutUint16(pkt[2:], seq)
		binary.BigEndian.PutUint32(pkt[4:], ts)
		binary.BigEndian.PutUint32(pkt[8:], r.ssrc)
		ok := true
		if r.codec.Opus {
			for i, v := range frame {
				pcm[i] = floatToInt16(v)
			}
			var err error
			if r.enc == nil {
				ok = false
			} else if pkt, err = r.enc.encode(pcm, pkt); err != nil {
				ok = false // a lost packet: seq and ts still move on
			}
		} else {
			for _, v := range frame {
				pkt = append(pkt, ulawEncode(floatToInt16(v)))
			}
		}
		if dst := r.remote.Load(); ok && dst != nil {
			if _, err := r.conn.WriteTo(pkt, dst); err == nil {
				r.sent.Add(1)
			}
		}
		seq++
		ts += tsStep
	}
}

func (r *sipRTP) receiver() {
	defer r.wg.Done()
	buf := make([]byte, 2048)
	var latched bool
	start := time.Now()
	others, stuns := 0, 0
	for {
		n, from, err := r.conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-r.stop:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if isSTUN(buf[:n]) {
			typ := stunType(buf[:n])
			if typ == stunBindingRequest {
				_, _ = r.conn.WriteTo(stunBindingResponse(buf[:n], from), from)
			}
			if stuns < 8 {
				stuns++
				r.log.Info("sip stun from the phone", "after_ms", time.Since(start).Milliseconds(),
					"type", fmt.Sprintf("%#04x", typ), "from", from, "len", n)
			}
			continue
		}
		if isProbe(buf[:n]) {
			_, _ = r.conn.WriteTo(append([]byte(nil), buf[:n]...), from) // echo: the check's answer
		}
		payload, pt, ok := rtpPayload(buf[:n])
		if ok && r.phoneVideoPT != 0 && pt == r.phoneVideoPT {
			h, err := media.DecodeRtpHeader(buf[:n])
			if err != nil {
				continue
			}
			if rotation, ok := media.CVORotation(h); ok {
				r.phoneVideoRot = rotation
			}
			for _, au := range r.phoneVideoDepack.Push(h.SequenceNumber, h.Timestamp, h.Marker, payload) {
				au.Rotation = r.phoneVideoRot
				if r.onPhoneVideo != nil {
					r.onPhoneVideo(au)
				}
			}
			continue
		}
		if !ok || pt != r.codec.PT {
			// e.g. the engine's firewall probes (PT 127) or RTCP. The first
			// few are logged: the engine starts its audio only ~5 s after
			// the answer, once its connectivity check gives up or succeeds.
			if others < 12 {
				others++
				r.log.Info("sip rtp other packet", "after_ms", time.Since(start).Milliseconds(), "pt", pt,
					"len", n, "from", from, "head", fmt.Sprintf("% x", buf[:min(n, 32)]))
			}
			continue
		}
		if !latched { // symmetric RTP: answer where the phone really sends from
			latched = true
			r.log.Info("sip rtp first audio", "after_ms", time.Since(start).Milliseconds())
			if cur := r.remote.Load(); cur == nil || cur.String() != from.String() {
				r.log.Info("sip rtp remote latched", "sdp", cur, "actual", from)
				r.remote.Store(from)
			}
		}
		r.recv.Add(1)
		if r.codec.Opus {
			if r.dec == nil {
				continue
			}
			pcm, err := r.dec.decode(payload)
			if err != nil {
				continue
			}
			up := make([]float32, len(pcm))
			for i, v := range pcm {
				up[i] = float32(v) / 32768
			}
			if r.onUp != nil {
				r.onUp(up)
			}
			continue
		}
		pcm8 := make([]float32, len(payload))
		for i, u := range payload {
			pcm8[i] = float32(ulawDecode(u)) / 32768
		}
		r.mu.Lock()
		pcm16 := r.us.process(pcm8)
		r.mu.Unlock()
		if r.onUp != nil {
			r.onUp(pcm16)
		}
	}
}

// The engine's connectivity probe (Eyeball AnyFirewall), seen 2026-11-02:
// RTP version 2, payload type 127, seq 1000, timestamp 1000, then a field
// counting milliseconds where the SSRC would be, and "12345678901234567890".
// Two engines send these to each other and start their audio once one
// arrives; from us none came, so the phone stayed on "connecting" for ~5 s
// after the answer (its check timing out) while the caller already talked.
// So we send our own at the start and echo the phone's.
const (
	probePT      = 127
	probeEvery   = 5 // sender ticks (100 ms)
	probeFor     = 3 * time.Second
	probePayload = "12345678901234567890"
)

func isProbe(p []byte) bool {
	return len(p) == 12+len(probePayload) && p[0] == 0x80 && p[1]&0x7f == probePT && string(p[12:]) == probePayload
}

func probePacket(ms uint32) []byte {
	p := make([]byte, 12, 12+len(probePayload))
	p[0], p[1] = 0x80, probePT
	binary.BigEndian.PutUint16(p[2:], 1000)
	binary.BigEndian.PutUint32(p[4:], 1000)
	binary.BigEndian.PutUint32(p[8:], ms)
	return append(p, probePayload...)
}

// rtpPayload strips the RTP header (CSRCs, extension, padding).
func rtpPayload(p []byte) ([]byte, int, bool) {
	if len(p) < 12 || p[0]>>6 != 2 {
		return nil, 0, false
	}
	pt := int(p[1] & 0x7f)
	if pt >= 72 && pt <= 76 { // RTCP
		return nil, pt, false
	}
	off := 12 + 4*int(p[0]&0x0f)
	if p[0]&0x10 != 0 {
		if len(p) < off+4 {
			return nil, pt, false
		}
		off += 4 + 4*int(binary.BigEndian.Uint16(p[off+2:]))
	}
	end := len(p)
	if p[0]&0x20 != 0 && end > off {
		end -= int(p[end-1])
	}
	if off > end {
		return nil, pt, false
	}
	return p[off:end], pt, true
}

func floatToInt16(v float32) int16 {
	if v > 1 {
		v = 1
	} else if v < -1 {
		v = -1
	}
	return int16(v * 32767)
}
