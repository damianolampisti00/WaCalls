package main

import (
	"math"
	"sync"
)

// The phone's own echo: the peer's voice coming out of the BlackBerry's
// earpiece or loudspeaker and back into its microphone. The phone has no
// canceller of its own, and with the mic levelled up (bbagc.go) peers heard
// themselves. The gateway cancels it with a second AEC3 (bbecho.go), the
// audio sent to the phone being the reference.
//
// The two streams don't share a clock at the gateway: the peer's audio has
// gaps (nothing is sent in silence) and arrives in bursts, the phone plays it
// through a jitter buffer that drains and refills, while the mic runs without
// a break. So every mic frame carries the phone's playback position (the
// downlink samples it had played when the frame was captured, mod 65536).
// For each mic frame the reference is what the phone played meanwhile, gaps
// as silence: both streams then advance together, sample for sample, and
// bbEcho's lag search only has to find the (fairly constant) remaining delay:
// the audio in flight and the device's own latency.
type bbUpEcho struct {
	echo *bbEcho

	mu      sync.Mutex
	hist    []float32 // the last bbUpHistKeep samples sent to the phone
	histEnd int64     // absolute index after the last one

	hasPos bool
	pos    int64  // playback position at the previous mic frame, in hist's index
	pos16  uint16 // as the phone reported it
	frames int    // mic frames with a position since the last report

	resynced bool // the phone reconnected during the call

	// Residual echo suppressor, after AEC3 (see suppress).
	refLevel  float64 // what the phone played lately (RMS, decaying peak)
	supGain   float64 // gain applied to the mic now
	supFrames int     // mic frames attenuated since the last report
}

const (
	// Residual echo suppression: a mic frame more than 14 dB below what the
	// phone was playing is taken for echo and turned down 14 dB. Measured
	// 2026-10-16 (phone at the ear): the peer's voice played at -19 dBFS
	// came back at -42 dBFS, the user's own voice is -20..-24 dBFS.
	bbSupRatio = 0.2  // mic RMS / played RMS below this: echo
	bbSupGain  = 0.2  // -14 dB on echo
	bbSupFloor = 0.003 // played below ~-50 dBFS: nothing to suppress
	bbSupDecay = 0.85  // per mic frame (20 ms): the played level's tail

	bbUpHistKeep = 4 * bbEchoRate
	bbUpEchoLead = 320 // 20 ms: the reference may lead the echo a little, never trail it
)

func newBBUpEcho() *bbUpEcho {
	e := newBBEcho()
	e.aligned = true
	e.lead = bbUpEchoLead
	// The playback position lines the reference up with the mic to within
	// the device's latency, which AEC3's own delay estimator handles. Our
	// envelope search isn't applied: the earpiece's echo is weak next to
	// the user's voice and it latched onto wrong lags (1.9 s, 3 s) in a real
	// call, while the fixed alignment cancelled the echo.
	e.hasLag = true
	e.fixedLag = true
	return &bbUpEcho{echo: e, supGain: 1}
}

func (u *bbUpEcho) close() { u.echo.close() }

// lagMs is the lag the envelope search finds (for the log; not applied).
func (u *bbUpEcho) lagMs() int { return u.echo.foundMs() }

// report: mic frames that carried a playback position since the last call,
// the lag search's last correlation and the share of frames the suppressor
// turned down (percent).
func (u *bbUpEcho) report() (frames int, corr float64, supPct int) {
	u.mu.Lock()
	frames, u.frames = u.frames, 0
	if frames > 0 {
		supPct = u.supFrames * 100 / frames
	}
	u.supFrames = 0
	u.mu.Unlock()
	return frames, u.echo.lastCorr(), supPct
}

func rmsOf(pcm []float32) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var s float64
	for _, v := range pcm {
		s += float64(v) * float64(v)
	}
	return math.Sqrt(s / float64(len(pcm)))
}

// suppress turns the mic down where AEC3 left echo: the peer's voice
// playing and the mic well below it. AEC3 needs seconds of the peer talking
// before it cancels anything (2026-10-16: nothing removed in the first 10 s
// of a call), and the mic AGC after it lifted that echo by +12 dB back to the
// peer. Down in ~3 frames, back up at once (ramped over the frame) when the
// user talks. Called with u.mu held.
func (u *bbUpEcho) suppress(pcm []float32, playedRMS float64) {
	u.refLevel = math.Max(playedRMS, u.refLevel*bbSupDecay)
	target := 1.0
	if u.refLevel > bbSupFloor && rmsOf(pcm) < u.refLevel*bbSupRatio {
		target = bbSupGain
		u.supFrames++
	}
	from := u.supGain
	to := target
	if to < from {
		to = math.Max(target, from*0.5)
	}
	n := len(pcm)
	for i := range pcm {
		g := from + (to-from)*float64(i+1)/float64(n)
		pcm[i] = float32(float64(pcm[i]) * g)
	}
	u.supGain = to
}

// toPhone records the audio sent to the phone, as sent.
func (u *bbUpEcho) toPhone(pcm []float32) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.hist = append(u.hist, pcm...)
	u.histEnd += int64(len(pcm))
	if drop := len(u.hist) - bbUpHistKeep; drop > 0 {
		u.hist = append(u.hist[:0], u.hist[drop:]...)
	}
}

// resync forgets the playback position: a new connection from the phone
// (audio sent to the old one meanwhile was lost, the positions no longer
// match).
func (u *bbUpEcho) resync() {
	u.mu.Lock()
	u.hasPos = false
	u.resynced = true
	u.mu.Unlock()
}

// mic cancels the phone's echo in a mic frame, in place. played is the
// phone's playback position when the frame was captured.
func (u *bbUpEcho) mic(pcm []float32, played uint16) {
	ref := make([]float32, len(pcm))
	u.mu.Lock()
	var p int64
	if !u.hasPos {
		// The first frame. At the start of a call the phone's position and
		// the audio sent count from the same point (the phone's playback
		// buffer and the gateway's history both begin with the call), so
		// the position is the index itself. After a reconnection
		// (resync) audio sent meanwhile was lost: line the position up with
		// the newest audio sent instead; what is still in flight then makes
		// the echo come a little later, which AEC3 copes with.
		if u.resynced {
			p = u.histEnd
		} else {
			p = int64(played)
		}
		u.hasPos = true
		u.pos = p - int64(len(pcm))
	} else {
		p = u.pos + int64(uint16(played-u.pos16))
	}
	// What was played since the previous frame; when the phone played less
	// than a frame's worth (its buffer ran dry) the rest was silence.
	from, n := u.pos, p-u.pos
	if n > int64(len(pcm)) {
		from, n = p-int64(len(pcm)), int64(len(pcm))
	}
	histStart := u.histEnd - int64(len(u.hist))
	for i := int64(0); i < n; i++ {
		if idx := from + i - histStart; idx >= 0 && idx < int64(len(u.hist)) {
			ref[i] = u.hist[idx]
		}
	}
	u.pos, u.pos16 = p, played
	u.frames++
	u.mu.Unlock()

	u.echo.sent(ref)
	u.echo.process(pcm)

	u.mu.Lock()
	u.suppress(pcm, rmsOf(ref))
	u.mu.Unlock()
}
