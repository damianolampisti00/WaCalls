package media

import (
	"bytes"
	"testing"

	"wacalls/internal/voip/core"
)

func TestH264PacketizerSingleAndMultipleNALs(t *testing.T) {
	p := NewH264Packetizer(0x12345678, 65535)
	packets, err := p.Packetize([]byte{0, 0, 0, 1, 0x67, 1, 2, 0, 0, 1, 0x68, 3}, 9000, 90)
	if err != nil {
		t.Fatal(err)
	}
	if len(packets) != 2 {
		t.Fatalf("got %d packets, want 2", len(packets))
	}
	wantPayloads := [][]byte{{0x67, 1, 2}, {0x68, 3}}
	for i, pkt := range packets {
		if !bytes.Equal(pkt.Payload, wantPayloads[i]) {
			t.Errorf("packet %d payload %x, want %x", i, pkt.Payload, wantPayloads[i])
		}
		if pkt.Header.PayloadType != core.PayloadTypeWhatsAppH264 || pkt.Header.Ssrc != 0x12345678 || pkt.Header.Timestamp != 9000 {
			t.Errorf("packet %d header: %+v", i, pkt.Header)
		}
		if pkt.Header.SequenceNumber != uint16(65535+i) {
			t.Errorf("packet %d sequence %d", i, pkt.Header.SequenceNumber)
		}
		if pkt.Header.Marker != (i == len(packets)-1) {
			t.Errorf("packet %d marker %v", i, pkt.Header.Marker)
		}
		if !pkt.Header.Extension || pkt.Header.ExtensionProfile != 0xdebe {
			t.Errorf("packet %d missing DEBE extension", i)
		}
		if deg, ok := CVORotation(pkt.Header); !ok || deg != 90 {
			t.Errorf("packet %d CVO rotation %d, %v", i, deg, ok)
		}
	}
}

func TestH264PacketizerFragmentsLargeNAL(t *testing.T) {
	nal := make([]byte, 2501)
	nal[0] = 0x65
	for i := 1; i < len(nal); i++ {
		nal[i] = byte(i)
	}
	input := append([]byte{0, 0, 1}, nal...)
	packets, err := NewH264Packetizer(7, 10).Packetize(input, 1234)
	if err != nil {
		t.Fatal(err)
	}
	if len(packets) != 3 {
		t.Fatalf("got %d packets, want 3", len(packets))
	}
	var rebuilt []byte
	for i, pkt := range packets {
		if len(pkt.Payload) > whatsAppH264MTU || pkt.Payload[0]&0x1f != whatsAppH264FU {
			t.Fatalf("packet %d not a bounded FU-A payload", i)
		}
		fu := pkt.Payload[1]
		if i == 0 && (fu&0x80 == 0 || fu&0x40 != 0) {
			t.Fatalf("bad first FU header: %#x", fu)
		}
		if i == 1 && (fu&0xc0 != 0) {
			t.Fatalf("bad middle FU header: %#x", fu)
		}
		if i == 2 && (fu&0x40 == 0 || fu&0x80 != 0) {
			t.Fatalf("bad end FU header: %#x", fu)
		}
		if pkt.Header.Marker != (i == 2) || pkt.Header.Timestamp != 1234 {
			t.Fatalf("bad packet marker/timestamp: %+v", pkt.Header)
		}
		if i == 0 {
			rebuilt = append(rebuilt, pkt.Payload[0]&0xe0|(fu&0x1f))
		}
		rebuilt = append(rebuilt, pkt.Payload[2:]...)
	}
	if !bytes.Equal(rebuilt, nal) {
		t.Fatal("FU-A payloads did not reconstruct the source NAL")
	}
}

func TestH264PacketizerRejectsMalformedInput(t *testing.T) {
	for _, in := range [][]byte{nil, {1, 2, 3}, {0, 0, 0, 1}} {
		if _, err := NewH264Packetizer(1, 1).Packetize(in, 1); err == nil {
			t.Errorf("input %x: expected an error", in)
		}
	}
}
