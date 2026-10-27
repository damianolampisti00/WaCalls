package main

import (
	"math"
	"sync"
	"time"
)

// bbUpPacer feeds the phone's mic audio to the call at a steady 20 ms pace,
// out of a small buffer. The phone sends on time, but on its way here (Wi-Fi,
// the tunnel) the audio stalls now and then for 0.3-1.2 s and then arrives in
// a burst. Fed as it came, the call core first sent comfort silence for the
// gap and then the whole burst at once, both stamped as consecutive audio:
// the peer heard crackles and jumps. Paced, a short stall is covered by the
// buffer; a long one becomes plain silence, and the late audio that piles up
// behind it is cut back to the buffer's size, so the delay doesn't grow.
//
// The buffer's size follows the network (2026-10-26: on a hotel-grade Wi-Fi a
// fixed 100 ms buffer cut 7.5 s of the user's voice out of 10): the target is
// the recent peak of how late the phone's audio comes against its own steady
// schedule, plus a margin, between bbUpMinTarget and bbUpMaxTarget. It
// follows a burst at once and falls back over ~10 s; on a good network it
// stays at the minimum, the fixed size it replaces.
type bbUpPacer struct {
	mu      sync.Mutex
	buf     []float32
	playing bool // false: (re)filling the buffer, silence goes out meanwhile

	// The arrival schedule (see pushAt): when sample 0 was due, the samples
	// received since, the last arrival, and the recent peak of lateness.
	started bool
	anchor  time.Time
	samples int64
	last    time.Time
	jitter  float64 // seconds
	target  int     // samples

	rebuffers int // times the buffer ran dry, since the last report
	dropped   int // samples cut after a burst, since the last report

	stop     chan struct{}
	stopOnce sync.Once
}

const (
	bbUpFrame     = 320  // 20 ms at 16 kHz
	bbUpMinTarget = 1600 // 100 ms buffered before audio goes out again, at least
	bbUpMaxTarget = 8000 // 500 ms at most: beyond this the conversation lags too much
	bbUpOverflow  = 3200 // 200 ms over the target: the oldest audio is cut back to it
	bbUpTick      = 20 * time.Millisecond

	bbUpJitterDecay  = 0.998                 // per frame received: ~10 s to forget a burst
	bbUpJitterMargin = 0.060                 // s
	bbUpResetGap     = 2 * time.Second       // a longer stall (or a reconnection): a new schedule
	bbUpCreep        = 20 * time.Microsecond // per frame: the schedule follows a slow clock drift
)

// newBBUpPacer starts feeding: feed gets one 20 ms frame every 20 ms, audio
// or silence.
func newBBUpPacer(feed func([]float32)) *bbUpPacer {
	p := &bbUpPacer{stop: make(chan struct{}), target: bbUpMinTarget}
	go p.run(feed)
	return p
}

func (p *bbUpPacer) close() { p.stopOnce.Do(func() { close(p.stop) }) }

// push queues mic audio as it arrives.
func (p *bbUpPacer) push(pcm []float32) { p.pushAt(pcm, time.Now()) }

func (p *bbUpPacer) pushAt(pcm []float32, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.track(len(pcm), now)
	p.buf = append(p.buf, pcm...)
	if len(p.buf) > p.target+bbUpOverflow {
		drop := len(p.buf) - p.target
		p.buf = append(p.buf[:0], p.buf[drop:]...)
		p.dropped += drop
	}
}

// track measures how late n samples arriving now are against the steady
// schedule of the phone's mic, and sets the target from it. Called with p.mu
// held.
func (p *bbUpPacer) track(n int, now time.Time) {
	if !p.started || now.Sub(p.last) > bbUpResetGap {
		p.started, p.anchor, p.samples = true, now, 0
	}
	due := p.anchor.Add(time.Duration(p.samples) * time.Second / bbEchoRate)
	late := now.Sub(due)
	if late < 0 { // earlier than ever: the schedule moves up
		p.anchor = p.anchor.Add(late)
		late = 0
	} else {
		p.anchor = p.anchor.Add(bbUpCreep)
	}
	p.samples += int64(n)
	p.last = now
	p.jitter = math.Max(late.Seconds(), p.jitter*bbUpJitterDecay)
	t := int((p.jitter + bbUpJitterMargin) * bbEchoRate)
	p.target = min(max(t, bbUpMinTarget), bbUpMaxTarget)
}

// next is the frame to send now.
func (p *bbUpPacer) next() []float32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]float32, bbUpFrame)
	if !p.playing && len(p.buf) >= p.target {
		p.playing = true
	}
	if !p.playing {
		return out
	}
	if len(p.buf) < bbUpFrame {
		p.playing = false // ran dry: silence until the buffer is full again
		p.rebuffers++
		return out
	}
	copy(out, p.buf)
	p.buf = append(p.buf[:0], p.buf[bbUpFrame:]...)
	return out
}

// report returns and resets the counters, and the audio buffered now and
// the target (ms).
func (p *bbUpPacer) report() (rebuffers, droppedMs, bufferedMs, targetMs int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rebuffers, droppedMs = p.rebuffers, p.dropped*1000/bbEchoRate
	p.rebuffers, p.dropped = 0, 0
	return rebuffers, droppedMs, len(p.buf) * 1000 / bbEchoRate, p.target * 1000 / bbEchoRate
}

func (p *bbUpPacer) run(feed func([]float32)) {
	t := time.NewTicker(bbUpTick)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			feed(p.next())
		}
	}
}
