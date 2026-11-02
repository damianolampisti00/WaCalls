package media

// H264Depacketizer turns RTP H.264 (RFC 6184: single NAL units, STAP-A,
// FU-A; WhatsApp's video, payload type 97) back into Annex-B access units,
// one per frame (RTP timestamp; the marker bit ends one). A frame with a
// missing packet is dropped whole: a decoder copes with a missing frame
// better than with a broken one.

var annexB = []byte{0, 0, 0, 1}

type H264Depacketizer struct {
	au      []byte
	ts      uint32
	open    bool // a frame is being assembled
	broken  bool // it lost a packet
	inFU    bool
	lastSeq uint16
	seqSeen bool
	idr     bool // the frame holds an IDR slice
}

// AccessUnit is one complete frame (Annex-B).
type AccessUnit struct {
	Data []byte
	TS   uint32
	IDR  bool
}

func (d *H264Depacketizer) flush(out []AccessUnit) []AccessUnit {
	if d.open && !d.broken && len(d.au) > 0 {
		out = append(out, AccessUnit{Data: d.au, TS: d.ts, IDR: d.idr})
	}
	d.au, d.open, d.broken, d.inFU, d.idr = nil, false, false, false, false
	return out
}

func (d *H264Depacketizer) nal(n []byte) {
	if len(n) == 0 {
		return
	}
	if n[0]&0x1f == 5 {
		d.idr = true
	}
	d.au = append(d.au, annexB...)
	d.au = append(d.au, n...)
}

// Push takes one packet; it returns the frames it completed.
func (d *H264Depacketizer) Push(seq uint16, ts uint32, marker bool, p []byte) []AccessUnit {
	var out []AccessUnit
	if d.open && ts != d.ts {
		out = d.flush(out) // the previous frame lost its marker packet
	}
	if d.seqSeen && seq != d.lastSeq+1 {
		// A hole inside the frame being built breaks it; before a new
		// one, a lost first fragment shows up as an FU-A without start.
		if d.open {
			d.broken = true
		}
		d.inFU = false
	}
	d.lastSeq, d.seqSeen = seq, true
	if !d.open {
		d.open, d.ts = true, ts
	}
	if len(p) > 0 {
		switch typ := p[0] & 0x1f; {
		case typ >= 1 && typ <= 23:
			d.nal(p)
		case typ == 24: // STAP-A: 16-bit sizes, then NAL units
			for off := 1; off+2 <= len(p); {
				n := int(p[off])<<8 | int(p[off+1])
				off += 2
				if off+n > len(p) {
					d.broken = true
					break
				}
				d.nal(p[off : off+n])
				off += n
			}
		case typ == 28 && len(p) >= 2: // FU-A
			fu := p[1]
			switch {
			case fu&0x80 != 0:
				hdr := p[0]&0xe0 | fu&0x1f
				if fu&0x1f == 5 {
					d.idr = true
				}
				d.au = append(d.au, annexB...)
				d.au = append(d.au, hdr)
				d.au = append(d.au, p[2:]...)
				d.inFU = true
			case d.inFU:
				d.au = append(d.au, p[2:]...)
			default:
				d.broken = true // a middle or end fragment without its start
			}
			if fu&0x40 != 0 {
				d.inFU = false
			}
		}
	}
	if marker {
		out = d.flush(out)
	}
	return out
}
