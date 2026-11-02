package main

// The peer's video (WhatsApp, H.264 on payload type 97, RFC 6184 packets:
// single NAL units, STAP-A, FU-A) back into Annex-B access units, one per
// frame (RTP timestamp; the marker bit ends one). A frame with a missing
// packet is dropped whole: a decoder copes with a missing frame better
// than with a broken one.

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

var annexB = []byte{0, 0, 0, 1}

type h264Depack struct {
	au      []byte
	ts      uint32
	open    bool // a frame is being assembled
	broken  bool // it lost a packet
	inFU    bool
	lastSeq uint16
	seqSeen bool
	idr     bool // the frame holds an IDR slice
}

// h264AU is one complete frame.
type h264AU struct {
	Data []byte
	TS   uint32
	IDR  bool
}

func (d *h264Depack) flush(out []h264AU) []h264AU {
	if d.open && !d.broken && len(d.au) > 0 {
		out = append(out, h264AU{Data: d.au, TS: d.ts, IDR: d.idr})
	}
	d.au, d.open, d.broken, d.inFU, d.idr = nil, false, false, false, false
	return out
}

func (d *h264Depack) nal(n []byte) {
	if len(n) == 0 {
		return
	}
	if n[0]&0x1f == 5 {
		d.idr = true
	}
	d.au = append(d.au, annexB...)
	d.au = append(d.au, n...)
}

// push takes one packet; it returns the frames it completed.
func (d *h264Depack) push(seq uint16, ts uint32, marker bool, p []byte) []h264AU {
	var out []h264AU
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

// videoDumpDir (-wa-video-dump): where the peer's video of each call is
// written as Annex-B H.264, to check what we receive (ffmpeg -i x.h264).
var videoDumpDir string

type videoDump struct {
	mu                 sync.Mutex
	f                  *os.File
	d                  h264Depack
	frames, idr, bytes int
	path               string
	// diagnostics: packets by NAL/packet type, frames seen, first heads
	pkts, seen, skipped int
	types               [32]int
	heads               []string
}

func newVideoDump(callID string, ssrc uint32) (*videoDump, error) {
	if err := os.MkdirAll(videoDumpDir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(videoDumpDir, fmt.Sprintf("%s-%08x.h264", callID, ssrc))
	f, err := os.Create(p)
	if err != nil {
		return nil, err
	}
	return &videoDump{f: f, path: p}, nil
}

func (v *videoDump) push(seq uint16, ts uint32, marker bool, payload []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.f == nil {
		return
	}
	v.pkts++
	if len(payload) > 0 {
		v.types[payload[0]&0x1f]++
	}
	if len(v.heads) < 12 {
		v.heads = append(v.heads, fmt.Sprintf("seq=%d ts=%d m=%t len=%d % x", seq, ts, marker, len(payload), payload[:min(len(payload), 6)]))
	}
	for _, au := range v.d.push(seq, ts, marker, payload) {
		v.seen++
		if v.frames == 0 && !au.IDR {
			v.skipped++
			continue // a decoder needs an IDR first
		}
		_, _ = v.f.Write(au.Data)
		v.frames++
		v.bytes += len(au.Data)
		if au.IDR {
			v.idr++
		}
	}
}

func (v *videoDump) close(log *slog.Logger) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.f == nil {
		return
	}
	_ = v.f.Close()
	v.f = nil
	types := map[int]int{}
	for t, n := range v.types {
		if n > 0 {
			types[t] = n
		}
	}
	log.Info("peer video written", "path", v.path, "frames", v.frames, "idr", v.idr, "bytes", v.bytes,
		"packets", v.pkts, "frames_complete", v.seen, "skipped_before_idr", v.skipped, "nal_types", types)
	for _, h := range v.heads {
		log.Info("peer video packet", "head", h)
	}
}
