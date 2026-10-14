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
}

const (
	bbUpHistKeep = 4 * bbEchoRate
	bbUpEchoLead = 320 // 20 ms: the reference may lead the echo a little, never trail it
)

func newBBUpEcho() *bbUpEcho {
	e := newBBEcho()
	e.aligned = true
	e.lead = bbUpEchoLead
	e.hasLag = true // lag 0 until the search finds the real one
	return &bbUpEcho{echo: e}
}

func (u *bbUpEcho) close() { u.echo.close() }

func (u *bbUpEcho) lagMs() int { return u.echo.lagMs() }

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
	u.mu.Unlock()
}

// mic cancels the phone's echo in a mic frame, in place. played is the
// phone's playback position when the frame was captured.
func (u *bbUpEcho) mic(pcm []float32, played uint16) {
	ref := make([]float32, len(pcm))
	u.mu.Lock()
	var p int64
	if !u.hasPos {
		// The first frame (of a connection): line the phone's position up
		// with the newest audio sent. Whatever is still in flight to the
		// phone or in its buffer makes the echo come later than that, a
		// positive lag the search finds.
		p = u.histEnd
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
	u.mu.Unlock()

	u.echo.sent(ref)
	u.echo.process(pcm)
}
