package main

import (
	"math"
	"testing"
)

func TestBBOpusRoundTrip(t *testing.T) {
	enc, err := newBBOpusEncoder()
	if err != nil {
		t.Fatal(err)
	}
	defer enc.close()
	dec, err := newBBOpusDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer dec.close()

	// 1 s of a 440 Hz tone at -12 dBFS, 20 ms at a time.
	var inPow, outPow float64
	bytes := 0
	for f := 0; f < 50; f++ {
		pcm := make([]int16, bbOpusFrame)
		for i := range pcm {
			n := f*bbOpusFrame + i
			pcm[i] = int16(8192 * math.Sin(2*math.Pi*440*float64(n)/bbOpusRate))
		}
		pkt, err := enc.encode(pcm, nil)
		if err != nil {
			t.Fatal(err)
		}
		bytes += len(pkt)
		out, err := dec.decode(pkt)
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != bbOpusFrame {
			t.Fatalf("decoded %d samples, want %d", len(out), bbOpusFrame)
		}
		if f >= 10 { // past the codec's start-up
			for i := range pcm {
				inPow += float64(pcm[i]) * float64(pcm[i])
				outPow += float64(out[i]) * float64(out[i])
			}
		}
	}
	if kbps := float64(bytes*8) / 1000; kbps > 40 {
		t.Errorf("bitrate %.1f kbit/s, want about 24", kbps)
	}
	if ratio := outPow / inPow; ratio < 0.5 || ratio > 2 {
		t.Errorf("decoded power ratio %.2f, want about 1", ratio)
	}
}
