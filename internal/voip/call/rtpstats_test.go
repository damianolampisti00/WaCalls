package call

import (
	"reflect"
	"testing"
)

func TestRTPExtensionIDsOfWhatsAppVideoHeader(t *testing.T) {
	// A 28-byte video header as whatsapp-rust builds it: profile 0xdebe, 3 words,
	// elements id3 (frame info), id5 (bandwidth, 2 bytes), id6 (offset, 2), id9 (tseq, 2).
	p := []byte{0x90, 97, 0, 1, 0, 0, 0, 0, 0x11, 0x22, 0x33, 0x44,
		0xde, 0xbe, 0, 3,
		0x30, 0x08, 0x51, 0x01, 0x02, 0x61, 0x00, 0x10, 0x91, 0x00, 0x05, 0x00,
		0x65} // NAL
	if got := rtpExtensionIDs(p); !reflect.DeepEqual(got, []uint8{3, 5, 6, 9}) {
		t.Fatalf("ids %v", got)
	}
	var s rtpStats
	s.add(p)
	if st := s.streams[uint64(97)<<32|0x11223344]; st == nil || st.packets != 1 || st.exts[9] != 1 {
		t.Fatalf("stats %+v", s.streams)
	}
}
