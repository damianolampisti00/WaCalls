package main

import (
	"math"
	"math/rand"
	"testing"
)

// A speech-like reference (noise in bursts), its echo coming back 400 ms
// later through a short room response, plus the peer's own voice now and
// then: the lag must be found and the echo cut by at least 10 dB.
func TestBBEchoCancels(t *testing.T) {
	e := newBBEcho()
	defer e.close()
	rng := rand.New(rand.NewSource(1))

	const total = 30 * bbEchoRate
	const lag = 6400 // 400 ms
	burst := func(n int, p float64) []float64 {
		out := make([]float64, n)
		on := false
		left := 0
		for i := range out {
			if left == 0 {
				on = rng.Float64() < p
				left = 4000 + rng.Intn(12000) // 0.25 to 1 s
			}
			left--
			if on {
				out[i] = rng.NormFloat64() * 0.15
			}
		}
		return out
	}
	ref := burst(total, 0.5)
	near := burst(total, 0.2)
	room := []float64{0.35, 0.2, -0.1, 0.05} // a few taps of echo path

	frame := 960
	var echoIn, echoOut float64
	for start := 0; start+frame <= total; start += frame {
		sent := make([]float32, frame)
		for i := range sent {
			sent[i] = float32(ref[start+i])
		}
		e.sent(sent)

		got := make([]float32, frame)
		echoOnly := make([]bool, frame)
		for i := range got {
			c := start + i
			var echo float64
			for k, h := range room {
				if r := c - lag - k*8; r >= 0 {
					echo += h * ref[r]
				}
			}
			got[i] = float32(echo + near[c])
			echoOnly[i] = near[c] == 0 && echo != 0
		}
		in := append([]float32(nil), got...)
		e.process(got)
		if start > 20*bbEchoRate { // after convergence
			for i := range got {
				if echoOnly[i] {
					echoIn += float64(in[i]) * float64(in[i])
					echoOut += float64(got[i]) * float64(got[i])
				}
			}
		}
	}
	if !e.hasLag {
		t.Fatal("no lag found")
	}
	if d := e.lag - lag; d < -bbEchoBlock || d > bbEchoBlock {
		t.Fatalf("lag %d samples, want about %d", e.lag, lag)
	}
	erle := 10 * math.Log10(echoIn/(echoOut+1e-12))
	t.Logf("lag %d samples, echo reduced by %.1f dB", e.lag, erle)
	if erle < 10 {
		t.Fatalf("echo reduced by only %.1f dB", erle)
	}
}
