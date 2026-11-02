package media

import (
	"bytes"
	"testing"
)

func TestH264DepackSingleSTAPAndFU(t *testing.T) {
	var d H264Depacketizer
	sps, pps := []byte{0x67, 1, 2}, []byte{0x68, 3}
	stap := []byte{24, 0, 3, 0x67, 1, 2, 0, 2, 0x68, 3}
	idr := append([]byte{0x65}, bytes.Repeat([]byte{9}, 10)...)
	// IDR split in three FU-A fragments
	fu1 := append([]byte{0x7c, 0x85}, idr[1:4]...)
	fu2 := append([]byte{0x7c, 0x05}, idr[4:8]...)
	fu3 := append([]byte{0x7c, 0x45}, idr[8:]...)
	if out := d.Push(1, 100, false, stap); len(out) != 0 {
		t.Fatal("frame closed before its marker")
	}
	d.Push(2, 100, false, fu1)
	d.Push(3, 100, false, fu2)
	out := d.Push(4, 100, true, fu3)
	if len(out) != 1 || !out[0].IDR {
		t.Fatalf("frames %d", len(out))
	}
	want := append(append(append(append(append([]byte(nil), annexB...), sps...), annexB...), pps...), append(annexB, idr...)...)
	if !bytes.Equal(out[0].Data, want) {
		t.Fatalf("access unit\n% x\nwant\n% x", out[0].Data, want)
	}
	// a P frame in one packet
	if out := d.Push(5, 200, true, []byte{0x41, 7, 7}); len(out) != 1 || out[0].IDR {
		t.Fatal("single NAL frame")
	}
}

func TestH264DepackDropsAFrameWithAHole(t *testing.T) {
	var d H264Depacketizer
	d.Push(1, 100, false, []byte{0x7c, 0x85, 1})
	// seq 2 lost
	if out := d.Push(3, 100, true, []byte{0x7c, 0x45, 3}); len(out) != 0 {
		t.Fatal("broken frame delivered")
	}
	// the next frame is fine again
	if out := d.Push(4, 200, true, []byte{0x41, 5}); len(out) != 1 {
		t.Fatal("next frame lost")
	}
	// a frame whose start fragment is lost, after a complete one
	if out := d.Push(6, 300, true, []byte{0x7c, 0x45, 3}); len(out) != 0 {
		t.Fatal("frame without its FU-A start delivered")
	}
}

// WhatsApp's keyframe (captured 2026-11-03): SPS, PPS and IDR with start
// codes inside one FU-A whose header names the SPS.
func TestH264DepackWhatsAppKeyframeInOneFU(t *testing.T) {
	var d H264Depacketizer
	blob := []byte{0x4d, 0x4c, 0x0c, 0, 0, 0, 1, 0x68, 0xee, 0x3c, 0x80, 0, 0, 0, 1, 0x65, 0xb8, 0, 4}
	d.Push(1, 100, false, append([]byte{0x7c, 0x87}, blob[:6]...))
	out := d.Push(2, 100, true, append([]byte{0x1c, 0x47}, blob[6:]...))
	if len(out) != 1 || !out[0].IDR {
		t.Fatalf("keyframe not recognised: %+v", out)
	}
	if !bytes.HasPrefix(out[0].Data, []byte{0, 0, 0, 1, 0x67, 0x4d, 0x4c, 0x0c, 0, 0, 0, 1, 0x68}) {
		t.Fatalf("frame % x", out[0].Data)
	}
}
