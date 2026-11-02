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
	frames, idr, bytes int
	path               string
	waiting            bool
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
	return &videoDump{f: f, path: p, waiting: true}, nil
}

// write appends one frame, from the first keyframe on.
func (v *videoDump) write(au media.AccessUnit) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.f == nil || v.waiting && !au.IDR {
		return
	}
	v.waiting = false
	_, _ = v.f.Write(au.Data)
	v.frames++
	v.bytes += len(au.Data)
	if au.IDR {
		v.idr++
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
	log.Info("peer video written", "path", v.path, "frames", v.frames, "idr", v.idr, "bytes", v.bytes)
}
