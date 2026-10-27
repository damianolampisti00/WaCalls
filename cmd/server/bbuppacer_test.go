package main

import (
	"testing"
	"time"
)

// pacerRun simulates seconds of the phone's mic: a 20 ms frame every 20 ms,
// delivered here at once, except that every `every` the network holds them
// back for `stall` and then hands them over in a burst. The pacer is ticked
// every 20 ms. Returns the samples dropped and the times the buffer ran dry
// after the first `settle`, and the pacer.
func pacerRun(seconds int, stall, every, settle time.Duration) (dropped, rebuffers int, p *bbUpPacer) {
	p = &bbUpPacer{target: bbUpMinTarget}
	t0 := time.Unix(1_700_000_000, 0)
	var held []time.Duration // capture times waiting for the network
	frame := make([]float32, bbUpFrame)
	for now := time.Duration(0); now < time.Duration(seconds)*time.Second; now += 20 * time.Millisecond {
		held = append(held, now)
		stalled := every > 0 && now%every < stall
		if !stalled {
			for range held {
				p.pushAt(frame, t0.Add(now))
			}
			held = held[:0]
		}
		p.next()
		if now == settle {
			p.mu.Lock()
			p.dropped, p.rebuffers = 0, 0
			p.mu.Unlock()
		}
	}
	return p.dropped, p.rebuffers, p
}

// On a good network the pacer is the fixed 100 ms buffer it replaced.
func TestBBUpPacerSteady(t *testing.T) {
	dropped, rebuffers, p := pacerRun(30, 0, 0, time.Second)
	if p.target != bbUpMinTarget || dropped != 0 || rebuffers != 0 {
		t.Fatalf("target %d, dropped %d, rebuffers %d", p.target, dropped, rebuffers)
	}
}

// Stalls of 300 ms every second (2026-10-26, a hotel-grade Wi-Fi): after the
// first ones the buffer has grown to cover them, nothing more is cut.
func TestBBUpPacerBursts(t *testing.T) {
	dropped, rebuffers, p := pacerRun(30, 300*time.Millisecond, time.Second, 3*time.Second)
	t.Logf("target %d ms, dropped %d ms, rebuffers %d", p.target*1000/bbEchoRate, dropped*1000/bbEchoRate, rebuffers)
	if dropped != 0 || rebuffers != 0 {
		t.Fatalf("dropped %d ms, rebuffers %d after the first seconds", dropped*1000/bbEchoRate, rebuffers)
	}
	if p.target > bbUpMaxTarget {
		t.Fatalf("target %d over the maximum", p.target)
	}
}

// Once the network is good again the buffer shrinks back to the minimum.
func TestBBUpPacerRecovers(t *testing.T) {
	_, _, p := pacerRun(10, 300*time.Millisecond, time.Second, time.Second)
	high := p.target
	t0 := time.Unix(1_700_000_000, 0).Add(10 * time.Second)
	frame := make([]float32, bbUpFrame)
	for i := 0; i < 50*30; i++ { // 30 s of a good network
		p.pushAt(frame, t0.Add(time.Duration(i)*20*time.Millisecond))
		p.next()
	}
	t.Logf("target %d ms -> %d ms", high*1000/bbEchoRate, p.target*1000/bbEchoRate)
	if high <= bbUpMinTarget || p.target != bbUpMinTarget {
		t.Fatalf("target %d -> %d", high, p.target)
	}
}
