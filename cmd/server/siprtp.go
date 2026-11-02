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
			}
			if media == "audio" && len(f) >= 3 && s.Port == 0 {
				s.Port, _ = strconv.Atoi(f[1])
				for _, p := range f[3:] {
					if pt, err := strconv.Atoi(p); err == nil {
						s.Formats = append(s.Formats, pt)
					}
				}
			}
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
	return s, nil
}

// hasPCMU: payload type 0 is static PCMU even without an rtpmap line.
func (s *sdpAudio) hasPCMU() bool {
	for _, pt := range s.Formats {
		if pt == 0 {
			return true
		}
	}
	return false
}

// buildSDP: one audio stream, PCMU, sendrecv. rejectVideo adds a refused
// video stream (port 0), for an answer to an offer that had one.
func buildSDP(ip string, port int, sessID int64, rejectVideo bool) []byte {
	lines := []string{
		"v=0",
		fmt.Sprintf("o=wacalls %d %d IN IP4 %s", sessID, sessID, ip),
		"s=WaCalls",
		"c=IN IP4 " + ip,
		"t=0 0",
		fmt.Sprintf("m=audio %d RTP/AVP 0", port),
		"a=rtpmap:0 PCMU/8000",
		"a=ptime:20",
		"a=sendrecv",
	}
	if rejectVideo {
		lines = append(lines, "m=video 0 RTP/AVP 109")
	}
	return []byte(strings.Join(lines, "\r\n") + "\r\n")
}

// ---- RTP session -----------------------------------------------------------

const (
	rtpFrame       = 160 // 20 ms at 8 kHz
	rtpPrebuffer   = 2 * rtpFrame
	rtpMaxBuffered = 20 * rtpFrame // 400 ms: beyond this the oldest audio goes
)

// sipRTP is one call's media: a UDP socket on our side, the phone's address
// from its SDP (then whatever address its packets really come from).
type sipRTP struct {
	conn   *net.UDPConn
	log    *slog.Logger
	remote atomic.Pointer[net.UDPAddr]
	ssrc   uint32

	mu      sync.Mutex
	down    []float32 // 8 kHz audio waiting to be sent
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
func listenRTP(ip string, lo, hi int) (*net.UDPConn, error) {
	for p := lo &^ 1; p <= hi; p += 2 {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(ip), Port: p})
		if err == nil {
			return c, nil
		}
	}
	return nil, fmt.Errorf("rtp: no free port in %d-%d on %s", lo, hi, ip)
}

func newSIPRTP(conn *net.UDPConn, remote *net.UDPAddr, onUp func([]float32), log *slog.Logger) *sipRTP {
	r := &sipRTP{conn: conn, log: log, onUp: onUp, stop: make(chan struct{})}
	var b [4]byte
	_, _ = rand.Read(b[:])
	r.ssrc = binary.BigEndian.Uint32(b[:])
	r.remote.Store(remote)
	return r
}

func (r *sipRTP) localPort() int { return r.conn.LocalAddr().(*net.UDPAddr).Port }

func (r *sipRTP) start() {
	r.wg.Add(2)
	go r.sender()
	go r.receiver()
}

func (r *sipRTP) close() {
	r.stopOnce.Do(func() {
		close(r.stop)
		_ = r.conn.Close()
		r.wg.Wait()
		r.log.Info("sip rtp closed", "sent", r.sent.Load(), "received", r.recv.Load(),
			"underruns", r.underruns.Load(), "dropped_ms", r.dropped.Load()/8)
	})
}

// WritePCM queues the peer's 16 kHz audio for the phone (mediaBridge).
func (r *sipRTP) WritePCM(pcm []float32) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.down = append(r.down, r.ds.process(pcm)...)
	if over := len(r.down) - rtpMaxBuffered; over > 0 {
		r.down = r.down[over:]
		r.dropped.Add(int64(over))
	}
	return nil
}

func (r *sipRTP) Close() {}

// sender sends one packet every 20 ms from the start: audio when there is
// some, silence otherwise. Sending first also opens the server's stateful
// firewall for the phone's packets.
func (r *sipRTP) sender() {
	defer r.wg.Done()
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	var seq uint16 = uint16(r.ssrc)
	var ts uint32 = r.ssrc ^ 0x5a5a5a5a
	pkt := make([]byte, 12+rtpFrame)
	frame := make([]float32, rtpFrame)
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
		}
		r.mu.Lock()
		if !r.started && len(r.down) >= rtpPrebuffer {
			r.started = true
		}
		if r.started && len(r.down) >= rtpFrame {
			copy(frame, r.down[:rtpFrame])
			r.down = r.down[rtpFrame:]
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
		pkt[0] = 0x80
		pkt[1] = 0 // PCMU
		binary.BigEndian.PutUint16(pkt[2:], seq)
		binary.BigEndian.PutUint32(pkt[4:], ts)
		binary.BigEndian.PutUint32(pkt[8:], r.ssrc)
		for i, v := range frame {
			pkt[12+i] = ulawEncode(floatToInt16(v))
		}
		if dst := r.remote.Load(); dst != nil {
			if _, err := r.conn.WriteToUDP(pkt, dst); err == nil {
				r.sent.Add(1)
			}
		}
		seq++
		ts += rtpFrame
	}
}

func (r *sipRTP) receiver() {
	defer r.wg.Done()
	buf := make([]byte, 2048)
	var latched bool
	for {
		n, from, err := r.conn.ReadFromUDP(buf)
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
		payload, pt, ok := rtpPayload(buf[:n])
		if !ok || pt != 0 {
			continue // e.g. the engine's firewall probes (PT 127) or RTCP
		}
		if !latched { // symmetric RTP: answer where the phone really sends from
			latched = true
			if cur := r.remote.Load(); cur == nil || cur.String() != from.String() {
				r.log.Info("sip rtp remote latched", "sdp", cur, "actual", from)
				r.remote.Store(from)
			}
		}
		r.recv.Add(1)
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
