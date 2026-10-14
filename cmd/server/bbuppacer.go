package main

import (
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
type bbUpPacer struct {
	mu      sync.Mutex
	buf     []float32
	playing bool // false: (re)filling the buffer, silence goes out meanwhile

	rebuffers int // times the buffer ran dry, since the last report
	dropped   int // samples cut after a burst, since the last report

	stop     chan struct{}
	stopOnce sync.Once
}

const (
	bbUpFrame  = 320  // 20 ms at 16 kHz
	bbUpTarget = 1600 // 100 ms buffered before audio goes out again
	bbUpMax    = 4800 // 300 ms: beyond this, the oldest audio is cut back to the target
	bbUpTick   = 20 * time.Millisecond
)

// newBBUpPacer starts feeding: feed gets one 20 ms frame every 20 ms, audio
// or silence.
func newBBUpPacer(feed func([]float32)) *bbUpPacer {
	p := &bbUpPacer{stop: make(chan struct{})}
	go p.run(feed)
	return p
}

func (p *bbUpPacer) close() { p.stopOnce.Do(func() { close(p.stop) }) }

// push queues mic audio as it arrives.
func (p *bbUpPacer) push(pcm []float32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, pcm...)
	if len(p.buf) > bbUpMax {
		drop := len(p.buf) - bbUpTarget
		p.buf = append(p.buf[:0], p.buf[drop:]...)
		p.dropped += drop
	}
}

// next is the frame to send now.
func (p *bbUpPacer) next() []float32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]float32, bbUpFrame)
	if !p.playing && len(p.buf) >= bbUpTarget {
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

// report returns and resets the counters, and the audio buffered now (ms).
func (p *bbUpPacer) report() (rebuffers, droppedMs, bufferedMs int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rebuffers, droppedMs = p.rebuffers, p.dropped*1000/bbEchoRate
	p.rebuffers, p.dropped = 0, 0
	return rebuffers, droppedMs, len(p.buf) * 1000 / bbEchoRate
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
