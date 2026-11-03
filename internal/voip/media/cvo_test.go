package media

import "testing"

func TestCVORotation(t *testing.T) {
	// a real WhatsApp video packet's extension (2026-11-03): id3=0x09
	// (back camera, R=1), id5, id6, id9, padding
	ext := []byte{0x30, 0x09, 0x51, 0x3c, 0x3b, 0x61, 0x00, 0x2e, 0x91, 0x08, 0xdc, 0x00}
	h := &RtpHeader{Extension: true, ExtensionProfile: 0xDEBE, ExtensionData: ext}
	if deg, ok := CVORotation(h); !ok || deg != 90 {
		t.Fatalf("got %d %t, want 90", deg, ok)
	}
	h.ExtensionData = []byte{0x51, 0x3c, 0x3b, 0x30, 0x03, 0x00, 0x00, 0x00}
	if deg, ok := CVORotation(h); !ok || deg != 270 {
		t.Fatalf("got %d %t, want 270", deg, ok)
	}
	h.ExtensionData = []byte{0x51, 0x3c, 0x3b, 0x00}
	if _, ok := CVORotation(h); ok {
		t.Fatal("no id 3: want !ok")
	}
	if _, ok := CVORotation(&RtpHeader{}); ok {
		t.Fatal("no extension: want !ok")
	}
}
