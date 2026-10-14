package main

import (
	"math"
	"math/rand"
	"testing"
)

// The phone's echo as the gateway sees it: the peer's audio arrives in 60 ms
// frames with pauses (nothing sent in silence) and some jitter, the phone
// plays it through a 120 ms jitter buffer that drains and refills (silence
// meanwhile), and its mic hears the playback 60 ms later through a short
// room response, plus the user talking now and then. Each mic frame reports
// the playback position. The echo must be cut by at least 10 dB.
func TestBBUpEchoCancels(t *testing.T) {
	u := newBBUpEcho()
	defer u.close()
	rng := rand.New(rand.NewSource(2))

	const total = 40 * bbEchoRate
	const frame = 320 // the phone's 20 ms mic frames
	const gwFrame = 960
	const cushion = 1920 // 120 ms
	const devLat = 960   // 60 ms from playback to the mic
	room := []float64{0.3, 0.15, -0.08, 0.04}

	// The peer's audio: talk spurts and silences, as the gateway sends it.
	var peer []float32
	for len(peer) < total {
		talk := 8000 + rng.Intn(24000)
		for i := 0; i < talk; i++ {
			peer = append(peer, float32(rng.NormFloat64()*0.12))
		}
		peer = append(peer, make([]float32, 8000+rng.Intn(24000))...) // marker only: silence isn't sent
	}
	// Which gateway frames are sent (speech) and when they reach the phone.
	type arrival struct {
		at  int // phone sample clock
		pcm []float32
	}
	var arrivals []arrival
	for start := 0; start+gwFrame <= total; start += gwFrame {
		f := peer[start : start+gwFrame]
		silent := true
		for _, v := range f {
			if v != 0 {
				silent = false
				break
			}
		}
		if silent {
			continue
		}
		arrivals = append(arrivals, arrival{at: start + 1600 + rng.Intn(1200), pcm: f}) // 100..175 ms in flight
	}

	var ring []float32 // the phone's jitter buffer
	var taken int64
	prebuffering := true
	played := make([]float32, 0, total+frame) // what the speaker emitted
	near := make([]float64, total)
	on, left := false, 0
	for i := range near {
		if left == 0 {
			on = rng.Float64() < 0.25
			left = 4000 + rng.Intn(12000)
		}
		left--
		if on {
			near[i] = rng.NormFloat64() * 0.1
		}
	}

	next := 0
	var echoIn, echoOut float64
	for clock := 0; clock+frame <= total; clock += frame {
		// Packets that have arrived by now; the gateway records them as sent.
		for next < len(arrivals) && arrivals[next].at <= clock {
			u.toPhone(arrivals[next].pcm)
			ring = append(ring, arrivals[next].pcm...)
			next++
		}
		// Playback of one block, as CallAudioOutput does.
		if prebuffering && len(ring) >= cushion {
			prebuffering = false
		}
		block := make([]float32, frame)
		if !prebuffering {
			n := copy(block, ring)
			ring = ring[n:]
			taken += int64(n)
			if n < frame {
				prebuffering = true
			}
		}
		played = append(played, block...)

		// The mic block captured meanwhile: the echo of earlier playback.
		mic := make([]float32, frame)
		echoOnly := make([]bool, frame)
		for i := range mic {
			c := clock + i
			var echo float64
			for k, h := range room {
				if r := c - devLat - k*8; r >= 0 && r < len(played) {
					echo += h * float64(played[r])
				}
			}
			mic[i] = float32(echo + near[c])
			echoOnly[i] = near[c] == 0 && echo != 0
		}
		in := append([]float32(nil), mic...)
		u.mic(mic, uint16(taken))
		if clock > 25*bbEchoRate {
			for i := range mic {
				if echoOnly[i] {
					echoIn += float64(in[i]) * float64(in[i])
					echoOut += float64(mic[i]) * float64(mic[i])
				}
			}
		}
	}
	erle := 10 * math.Log10(echoIn/(echoOut+1e-12))
	t.Logf("lag %d ms, echo reduced by %.1f dB", u.lagMs(), erle)
	if erle < 10 {
		t.Fatalf("echo reduced by only %.1f dB", erle)
	}
}
