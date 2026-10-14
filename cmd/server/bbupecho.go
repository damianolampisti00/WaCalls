package main

import "sync"

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
}

const (
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
	return &bbUpEcho{echo: e}
}

func (u *bbUpEcho) close() { u.echo.close() }

// lagMs is the lag the envelope search finds (for the log; not applied).
func (u *bbUpEcho) lagMs() int { return u.echo.foundMs() }

// report: mic frames that carried a playback position since the last call,
// and the lag search's last correlation.
func (u *bbUpEcho) report() (frames int, corr float64) {
	u.mu.Lock()
	frames, u.frames = u.frames, 0
	u.mu.Unlock()
	return frames, u.echo.lastCorr()
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
}
