package mlow

import (
	"encoding/binary"
	"encoding/hex"
	"math"
	"os"
	"testing"
)

// TestE2EDecodeMatchesUseSmpl is the audible milestone: decode the whole captured
// stream and compare against the libopus useSmpl reference PCM
// (ref_usesmpl_expected.raw). The synthesis is not bit-exact (PRNG noise +
// -ffast-math), so the bar is length match + Pearson correlation > 0.95 at lag 0
// (the harmonic postfilter emits the 48-sample group delay). Mirrors
// e2e_decode_matches_usesmpl.
func TestE2EDecodeMatchesUseSmpl(t *testing.T) {
	var frames []string
	loadJSON(t, "inbound_capture_frames.json", &frames)

	raw, err := os.ReadFile("testdata/ref_usesmpl_expected.raw")
	if err != nil {
		t.Fatalf("read ref_usesmpl_expected.raw: %v", err)
	}
	refp := make([]float64, len(raw)/2)
	for i := range refp {
		refp[i] = float64(int16(binary.LittleEndian.Uint16(raw[2*i:]))) / 32768.0
	}

	dec := NewMlowDecoder()
	var out []float64
	for _, hf := range frames {
		fb, err := hex.DecodeString(hf)
		if err != nil {
			t.Fatalf("bad hex frame: %v", err)
		}
		for _, v := range dec.Decode(fb) {
			out = append(out, float64(v))
		}
	}
	if len(out) != len(refp) {
		t.Fatalf("decode length %d != reference %d", len(out), len(refp))
	}

	n := len(refp)
	var mr, mo float64
	for i := 0; i < n; i++ {
		mr += refp[i]
		mo += out[i]
	}
	mr /= float64(n)
	mo /= float64(n)
	var sxy, sxx, syy float64
	for i := 0; i < n; i++ {
		dr := refp[i] - mr
		dz := out[i] - mo
		sxy += dr * dz
		sxx += dr * dr
		syy += dz * dz
	}
	denom := math.Sqrt(sxx * syy)
	if denom == 0 {
		t.Fatalf("correlation undefined: constant signal (sxx=%v syy=%v)", sxx, syy)
	}
	corr := sxy / denom
	t.Logf("e2e lag-0 corr %.4f", corr)
	if corr <= 0.95 {
		t.Errorf("lag-0 corr %.4f <= 0.95 vs useSmpl reference", corr)
	}
}

func TestDecodeMultiFramePayload(t *testing.T) {
	// Packet 5 from video call 005333327FB7E64044E6677C9DA818C6 (2026-11-09):
	// 0x92 0x02 0x13 followed by a 19-byte 60 ms MLow frame and an 18-byte 60 ms MLow frame.
	f1, _ := hex.DecodeString("5046eb5a24ddaffe806b866fc146c8d9556e40")
	f2, _ := hex.DecodeString("5021e161c25d31c771b046c8d9556e401234")
	multi := append([]byte{0x92, 0x02, byte(len(f1))}, f1...)
	multi = append(multi, f2...)

	decMulti := NewMlowDecoder()
	got := decMulti.Decode(multi)
	if len(got) != 2*opusFrameSamps {
		t.Fatalf("multi-frame Decode len = %d, want %d", len(got), 2*opusFrameSamps)
	}

	decSingle := NewMlowDecoder()
	want := append(decSingle.Decode(f1), decSingle.Decode(f2)...)
	var sumSq float64
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d mismatch: got %v, want %v", i, got[i], want[i])
		}
		sumSq += float64(got[i] * got[i])
	}
	if sumSq == 0 {
		t.Fatal("decoded multi-frame audio is completely silent")
	}
}
