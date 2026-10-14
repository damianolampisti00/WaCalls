package main

// Echo cancelling for the Berry Bridge phone. When the peer talks
// hands-free, their phone may not cancel the phone user's voice coming out
// of its loudspeaker, and the user hears themselves. The gateway has both
// halves: what it sent to the peer (the reference, from the call core's
// OnSentPCM) and what comes back. It finds the round-trip lag between the
// two streams itself, then runs WebRTC's AEC3 (bbaec.cpp) on the peer's
// audio with the reference fed slightly ahead of the echo.
//
// The streams are aligned by sample count, not wall clock: both run at
// 16 kHz in frames (sent: the codec frames incl. keep-alive silence;
// received: one decoded packet each), so network jitter doesn't move them.
// A lost packet shifts them; the lag is re-estimated every 2 s.

/*
#cgo pkg-config: webrtc-audio-processing-1
#cgo CXXFLAGS: -std=c++17
#include "bbaec.h"
*/
import "C"

import (
	"math"
	"sync"
	"unsafe"
)

const (
	bbEchoRate      = 16000
	bbEchoChunk     = 160 // AEC frame: 10 ms
	bbEchoBlock     = 320 // envelope block for the lag search: 20 ms
	bbEchoRefKeep   = 4 * bbEchoRate
	bbEchoEnvBlocks = 400  // 8 s of envelope
	bbEchoMinLag    = -50  // blocks (-1 s): the streams start at different moments
	bbEchoMaxLag    = 150  // blocks (+3 s)
	bbEchoLead      = 1600 // samples (100 ms) the reference is fed ahead of the estimate
	bbEchoEvery     = 100  // blocks between lag estimates (2 s)
	bbEchoMinCorr   = 0.35
)

type bbEcho struct {
	mu  sync.Mutex
	aec *C.bb_aec

	ref      []int16 // the last bbEchoRefKeep sent samples
	refStart int64   // absolute index of ref[0]
	refEnd   int64   // absolute index after the last sent sample

	capCount int64 // received samples processed so far

	refEnv, capEnv     []float64 // log-energy per block, by absolute block index
	refEnvAt, capEnvAt int64     // block index of refEnv[0] / capEnv[0]
	refAcc, capAcc     float64   // the block being summed
	refAccN, capAccN   int
	sinceEstimate      int

	lag    int64 // capture index c holds the echo of reference index c-lag
	hasLag bool
	cand   int64 // a new lag seen once, confirmed by the next estimate

	lead int64 // samples the reference is fed ahead of the estimated echo
	// aligned: reference and capture arrive in lockstep (bbupecho.go), so
	// no reference exists past the current capture frame: never feed later
	// than that.
	aligned bool
}

func newBBEcho() *bbEcho {
	e := &bbEcho{aec: C.bb_aec_new(bbEchoRate), lead: bbEchoLead}
	return e
}

// lagMs is the round trip found between sent and received audio, -1 if none yet.
func (e *bbEcho) lagMs() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.hasLag {
		return -1
	}
	return int(e.lag * 1000 / bbEchoRate)
}

func (e *bbEcho) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.aec != nil {
		C.bb_aec_free(e.aec)
		e.aec = nil
	}
}

func toInt16(v float32) int16 {
	x := v * 32768
	if x > 32767 {
		return 32767
	}
	if x < -32768 {
		return -32768
	}
	return int16(x)
}

// sent records what went to the peer (the call core's frames, in order).
func (e *bbEcho) sent(pcm []float32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range pcm {
		v := toInt16(s)
		e.ref = append(e.ref, v)
		e.refAcc += float64(v) * float64(v)
		e.refAccN++
		if e.refAccN == bbEchoBlock {
			e.refEnv = append(e.refEnv, math.Log10(e.refAcc/bbEchoBlock+1))
			e.refAcc, e.refAccN = 0, 0
		}
	}
	e.refEnd += int64(len(pcm))
	if drop := len(e.ref) - bbEchoRefKeep; drop > 0 {
		e.ref = append(e.ref[:0], e.ref[drop:]...)
		e.refStart += int64(drop)
	}
	if drop := len(e.refEnv) - 2*bbEchoEnvBlocks; drop > 0 {
		e.refEnv = append(e.refEnv[:0], e.refEnv[drop:]...)
		e.refEnvAt += int64(drop)
	}
}

// process cancels the echo in the peer's audio, in place.
func (e *bbEcho) process(pcm []float32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.aec == nil {
		return
	}
	var capFrame, refFrame [bbEchoChunk]int16
	for off := 0; off+bbEchoChunk <= len(pcm); off += bbEchoChunk {
		for i := 0; i < bbEchoChunk; i++ {
			v := toInt16(pcm[off+i])
			capFrame[i] = v
			e.capAcc += float64(v) * float64(v)
			e.capAccN++
			if e.capAccN == bbEchoBlock {
				e.capEnv = append(e.capEnv, math.Log10(e.capAcc/bbEchoBlock+1))
				e.capAcc, e.capAccN = 0, 0
				e.sinceEstimate++
			}
		}
		if e.hasLag {
			// The reference this chunk's echo is made of, bbEchoLead early.
			r0 := e.capCount - e.lag + e.lead
			if last := e.refEnd - bbEchoChunk; e.aligned && r0 > last {
				r0 = last
			}
			for i := 0; i < bbEchoChunk; i++ {
				idx := r0 + int64(i) - e.refStart
				if idx >= 0 && idx < int64(len(e.ref)) {
					refFrame[i] = e.ref[idx]
				} else {
					refFrame[i] = 0
				}
			}
			C.bb_aec_render(e.aec, (*C.int16_t)(unsafe.Pointer(&refFrame[0])))
			C.bb_aec_capture(e.aec, (*C.int16_t)(unsafe.Pointer(&capFrame[0])))
			for i := 0; i < bbEchoChunk; i++ {
				pcm[off+i] = float32(capFrame[i]) / 32768
			}
		}
		e.capCount += bbEchoChunk
	}
	if drop := len(e.capEnv) - 2*bbEchoEnvBlocks; drop > 0 {
		e.capEnv = append(e.capEnv[:0], e.capEnv[drop:]...)
		e.capEnvAt += int64(drop)
	}
	if e.sinceEstimate >= bbEchoEvery {
		e.sinceEstimate = 0
		e.estimate()
	}
}

// estimate finds the lag (in blocks) at which the received envelope best
// follows the sent one, over the last 8 s. Only a clear peak counts (the
// user talked and the echo came back); a change needs two estimates.
func (e *bbEcho) estimate() {
	n := bbEchoEnvBlocks
	capEnd := e.capEnvAt + int64(len(e.capEnv))
	capFrom := capEnd - int64(n)
	if capFrom < e.capEnvAt {
		return
	}
	capWin := e.capEnv[capFrom-e.capEnvAt:]
	capMean, capVar := meanVar(capWin)
	if capVar < 1e-3 {
		return
	}
	best, bestLag := -1.0, int64(0)
	for lag := int64(bbEchoMinLag); lag <= bbEchoMaxLag; lag++ {
		from := capFrom - lag
		if from < e.refEnvAt || from+int64(n) > e.refEnvAt+int64(len(e.refEnv)) {
			continue
		}
		ref := e.refEnv[from-e.refEnvAt : from-e.refEnvAt+int64(n)]
		refMean, refVar := meanVar(ref)
		if refVar < 1e-2 {
			continue // the user hardly talked: nothing to find
		}
		var cov float64
		for i := 0; i < n; i++ {
			cov += (capWin[i] - capMean) * (ref[i] - refMean)
		}
		c := cov / float64(n) / math.Sqrt(capVar*refVar)
		if c > best {
			best, bestLag = c, lag
		}
	}
	if best < bbEchoMinCorr {
		return
	}
	lag := bestLag * bbEchoBlock
	switch {
	case !e.hasLag:
		e.lag, e.hasLag = lag, true
	case lag == e.lag:
		e.cand = 0
	case lag == e.cand:
		e.lag, e.cand = lag, 0
	default:
		e.cand = lag
	}
}

func meanVar(x []float64) (float64, float64) {
	var s, s2 float64
	for _, v := range x {
		s += v
		s2 += v * v
	}
	n := float64(len(x))
	m := s / n
	return m, s2/n - m*m
}
