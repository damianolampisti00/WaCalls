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
