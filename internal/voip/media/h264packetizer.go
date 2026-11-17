package media

import (
	"errors"

	"wacalls/internal/voip/core"
)

const (
	whatsAppH264MTU = 1200
	whatsAppH264FU  = 28
)

// H264Packetizer converts one Annex-B access unit to WhatsApp's RFC 6184
// RTP format. The sequence counter is independent of the audio stream.
type H264Packetizer struct {
	ssrc uint32
	seq  uint16
}

func NewH264Packetizer(ssrc uint32, initialSequence uint16) *H264Packetizer {
	return &H264Packetizer{ssrc: ssrc, seq: initialSequence}
}

// Packetize returns RTP packets for one Annex-B access unit. timestamp is the
// source's 90 kHz RTP timestamp and is shared by every packet in the frame.
func (p *H264Packetizer) Packetize(annexB []byte, timestamp uint32, rotation ...int) ([]*RtpPacket, error) {
	nals := splitAnnexBNALs(annexB)
	if len(nals) == 0 {
		return nil, errors.New("h264 packetizer: no NAL units")
	}
	var packets []*RtpPacket
	for _, nal := range nals {
		if len(nal) == 0 {
			continue
		}
		if len(nal) <= whatsAppH264MTU {
			packets = append(packets, p.packet(nal, timestamp, false, rotation...))
			continue
		}
		indicator := nal[0]&0xe0 | whatsAppH264FU
		typ := nal[0] & 0x1f
		body := nal[1:]
		for off, first := 0, true; off < len(body); first = false {
			end := min(off+whatsAppH264MTU-2, len(body))
			fuHeader := typ
			if first {
				fuHeader |= 0x80
			}
			if end == len(body) {
				fuHeader |= 0x40
			}
			payload := make([]byte, 2, 2+end-off)
			payload[0], payload[1] = indicator, fuHeader
			payload = append(payload, body[off:end]...)
			packets = append(packets, p.packet(payload, timestamp, false, rotation...))
			off = end
		}
	}
	if len(packets) == 0 {
		return nil, errors.New("h264 packetizer: empty access unit")
	}
	packets[len(packets)-1].Header.Marker = true
	return packets, nil
}

func (p *H264Packetizer) packet(payload []byte, timestamp uint32, marker bool, rotation ...int) *RtpPacket {
	h := NewRtpHeader(core.PayloadTypeWhatsAppH264, p.seq, timestamp, p.ssrc)
	h.Marker = marker
	h.Extension = true
	h.ExtensionProfile = 0xdebe
	if len(rotation) > 0 && rotation[0] != 0 {
		h.ExtensionData = []byte{0x30, byte((rotation[0] / 90) & 0x03), 0, 0}
	}
	p.seq++
	return &RtpPacket{Header: h, Payload: payload}
}

func splitAnnexBNALs(data []byte) [][]byte {
	start := func(from int) (int, int) {
		for i := from; i+3 <= len(data); i++ {
			if i+4 <= len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
				return i, 4
			}
			if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
				return i, 3
			}
		}
		return -1, 0
	}
	var nals [][]byte
	pos, code := start(0)
	if pos < 0 {
		return nil
	}
	for {
		begin := pos + code
		next, nextCode := start(begin)
		end := len(data)
		if next >= 0 {
			end = next
		}
		for end > begin && data[end-1] == 0 { // Annex-B trailing_zero_8bits
			end--
		}
		if end > begin {
			nals = append(nals, data[begin:end])
		}
		if next < 0 {
			break
		}
		pos, code = next, nextCode
	}
	return nals
}
