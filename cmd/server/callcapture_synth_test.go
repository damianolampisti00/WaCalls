package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"wacalls/internal/voip/call"
	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"
)

// TestSynthCapture writes a capture of a fake call whose peer sends the
// H.264 in $SYNTH_H264 (Annex-B, e.g. from ffmpeg testsrc) the way WhatsApp
// does (STAP-A for SPS/PPS, FU-A for big slices, SRTP), into
// $SYNTH_CAPTURE_DIR, to check cmd/videodiag end to end:
//
//	ffmpeg -f lavfi -i testsrc=size=480x640:rate=15 -t 3 -c:v libx264 -profile:v baseline -bf 0 -g 15 x.h264
//	SYNTH_H264=x.h264 SYNTH_CAPTURE_DIR=/tmp/cap go test -run TestSynthCapture ./cmd/server
//	go run ./cmd/videodiag /tmp/cap/<dir>
func TestSynthCapture(t *testing.T) {
	src, out := os.Getenv("SYNTH_H264"), os.Getenv("SYNTH_CAPTURE_DIR")
	if src == "" || out == "" {
		t.Skip("SYNTH_H264 and SYNTH_CAPTURE_DIR not set")
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	captureDir = out
	defer func() { captureDir = "" }()
	cap, err := newCallCapture("SYNTH")
	if err != nil {
		t.Fatal(err)
	}
	km := core.SrtpKeyingMaterial{MasterKey: bytes.Repeat([]byte{7}, 16), MasterSalt: bytes.Repeat([]byte{9}, 14)}
	ctx, err := media.NewSrtpContext(km, core.SRTPRecvAuthTagLen)
	if err != nil {
		t.Fatal(err)
	}
	var nals [][]byte
	for _, n := range bytes.Split(raw, []byte{0, 0, 1}) {
		n = bytes.TrimRight(n, "\x00")
		if len(n) > 0 {
			nals = append(nals, n)
		}
	}
	const ssrc = 0x82e31fd2
	seq, ts := uint16(1000), uint32(90000)
	send := func(pay []byte, marker bool) {
		h := &media.RtpHeader{Version: 2, PayloadType: core.PayloadTypeWhatsAppH264, SequenceNumber: seq, Timestamp: ts, Ssrc: ssrc, Marker: marker}
		p, err := ctx.Protect(&media.RtpPacket{Header: h, Payload: pay})
		if err != nil {
			t.Fatal(err)
		}
		cap.media(false, p)
		seq++
	}
	var params []byte
	for _, n := range nals {
		switch n[0] & 0x1f {
		case 7, 8: // SPS/PPS: aggregated into a STAP-A before the next slice
			params = append(params, byte(len(n)>>8), byte(len(n)))
			params = append(params, n...)
		case 1, 5:
			if params != nil {
				send(append([]byte{24}, params...), false)
				params = nil
			}
			if len(n) <= 960 {
				send(n, true)
			} else {
				for off := 1; off < len(n); off += 958 {
					end := min(off+958, len(n))
					fu := n[0]&0x1f | map[bool]byte{true: 0x80}[off == 1] | map[bool]byte{true: 0x40}[end == len(n)]
					send(append([]byte{n[0]&0xe0 | 28, fu}, n[off:end]...), end == len(n))
				}
			}
			ts += 6000 // 15 fps
		}
	}
	b, _ := json.MarshalIndent(call.CallKeyInfo{CallID: "SYNTH", RecvKey: km.MasterKey, RecvSalt: km.MasterSalt,
		PeerDevice: "1@lid", OurDevice: "2@lid"}, "", "  ")
	dir := cap.close()
	if err := os.WriteFile(filepath.Join(dir, "keys.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("capture in %s (%d NAL units)", dir, len(nals))
}
