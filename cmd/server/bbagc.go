package main

import "math"

// bbAGC levels the peer's voice before it goes to the phone. WhatsApp call
// audio decoded from MLow often arrives quiet (speech around -25..-30 dBFS),
// and the BlackBerry's speakerphone has little headroom of its own: on the
// first real call the speaker was "molto basso". Boost only (up to +18 dB),
// fast attack / slow release, a noise gate so silence and background hiss are
// never pumped up, and a soft limiter instead of hard clipping.
type bbAGC struct {
	gain  float64 // current linear gain
	level float64 // smoothed RMS of the input

	// stats since the last report
	inSum, outSum float64
	n             int
}

const (
	agcTarget  = 0.20       // ~ -14 dBFS RMS: loud, with room for peaks
	agcMaxGain = 8.0        // +18 dB
	agcGate    = 0.004      // ~ -48 dBFS: below this, hold the gain
	agcAttack  = 0.5        // per frame, when the level rises
	agcRelease = 0.05       // per frame, when it falls
	agcGainUp  = 1.0 + 0.05 // max gain increase per 60 ms frame (~+0.4 dB)
)

func newBBAGC() *bbAGC { return &bbAGC{gain: 1, level: agcTarget} }

func (a *bbAGC) process(pcm []float32) {
	if len(pcm) == 0 {
		return
	}
	var sum float64
	for _, s := range pcm {
		sum += float64(s) * float64(s)
	}
	rms := math.Sqrt(sum / float64(len(pcm)))
	// A pause (this frame below the gate): neither the speech level nor the
	// gain moves, so the background is never pumped up between phrases.
	if rms > agcGate {
		if rms > a.level {
			a.level += (rms - a.level) * agcAttack
		} else {
			a.level += (rms - a.level) * agcRelease
		}
		want := math.Min(agcMaxGain, math.Max(1, agcTarget/a.level))
		if want > a.gain {
			a.gain = math.Min(want, a.gain*agcGainUp)
		} else {
			a.gain = want // drop at once: never let a loud phrase clip
		}
	}
	var outSum float64
	for i, s := range pcm {
		v := float64(s) * a.gain
		// soft limiter above 0.7: smooth knee into +-1
		if v > 0.7 {
			v = 0.7 + 0.3*math.Tanh((v-0.7)/0.3)
		} else if v < -0.7 {
			v = -0.7 - 0.3*math.Tanh((-v-0.7)/0.3)
		}
		pcm[i] = float32(v)
		outSum += v * v
	}
	a.inSum += sum
	a.outSum += outSum
	a.n += len(pcm)
}

// report returns the input/output RMS (dBFS) since the last call and the gain (dB).
func (a *bbAGC) report() (inDB, outDB, gainDB float64, ok bool) {
	if a.n == 0 {
		return 0, 0, 0, false
	}
	db := func(sum float64) float64 { return 10 * math.Log10(sum/float64(a.n)+1e-12) }
	inDB, outDB = db(a.inSum), db(a.outSum)
	gainDB = 20 * math.Log10(a.gain)
	a.inSum, a.outSum, a.n = 0, 0, 0
	return inDB, outDB, gainDB, true
}
