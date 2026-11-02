package main

// The peer's video written to a file (-wa-video-dump), through
// media.H264Depacketizer.

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"wacalls/internal/voip/media"
)

// videoDumpDir (-wa-video-dump): where the peer's video of each call is
// written as Annex-B H.264, to check what we receive (ffmpeg -i x.h264).
var videoDumpDir string

type videoDump struct {
	mu                 sync.Mutex
	f                  *os.File
	d                  media.H264Depacketizer
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
	for _, au := range v.d.Push(seq, ts, marker, payload) {
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
