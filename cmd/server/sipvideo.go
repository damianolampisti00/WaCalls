package main

// The peer's WhatsApp video to the phone's call engine (EBMService). What
// WhatsApp sends (H.264 Main, 320x212 on 2026-11-03, keyframes every few
// seconds) is not what the engine decodes: it wants one slice per frame and
// takes 512x288 (its own size), and an unknown peer's H.264 in Eyeball's
// "legacy" packets. So the server transcodes (ffmpeg: decode, turn upright
// (CVO), scale with black bars to 512x288 or, portrait, 288x512, x264
// baseline, one slice, a keyframe a second) and
// packetizes like the experiment the engine decoded on 2026-11-02
// (bbmvideo-re/server/mediaprobe.py, variant legacy): SPS and PPS as single
// NAL packets, FU-A whose first fragment keeps the NAL header byte.

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"wacalls/internal/voip/media"
)

const (
	videoMTU   = 1200 // payload bytes per packet (the experiment's)
	videoClock = 90000
)

// ffmpegArgs: raw H.264 in on stdin, the engine's H.264 out on stdout, for
// the peer's orientation (CVO degrees, media.CVORotation): the picture is
// turned upright and, held upright (90/270), sent portrait 288x512 instead
// of a small pillarboxed picture in 512x288 (2026-11-03: the Q10 showed the
// caller's portrait video lying on its side).
func ffmpegArgs(rotation int) []string {
	w, h, turn := 512, 288, ""
	switch rotation {
	case 90:
		w, h, turn = 288, 512, "transpose=cclock,"
	case 270:
		w, h, turn = 288, 512, "transpose=clock,"
	case 180:
		turn = "hflip,vflip,"
	}
	vf := fmt.Sprintf("%sscale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,format=yuv420p",
		turn, w, h, w, h)
	return []string{
		"-hide_banner", "-loglevel", "error",
		// (no -fflags nobuffer: with it 50 of the 122 frames of a real call
		// were lost)
		"-flags", "low_delay", "-probesize", "32", "-analyzeduration", "0",
		// steady input timestamps whatever the arrival (bursts included): the
		// RTP timestamps toward the phone come from the wall clock (send)
		"-framerate", "15", "-f", "h264", "-i", "pipe:0",
		"-vf", vf,
		// every frame out, also ones arriving in a burst (the same wall-clock
		// timestamp would otherwise drop them as duplicates)
		"-fps_mode", "passthrough",
		"-c:v", "libx264", "-profile:v", "baseline", "-level", "3.0", "-pix_fmt", "yuv420p",
		"-tune", "zerolatency", "-preset", "veryfast", "-b:v", "300k", "-maxrate", "400k", "-bufsize", "400k",
		"-g", "15", "-bf", "0", "-x264-params", "repeat-headers=1:aud=1:slices=1:sliced-threads=0:threads=1",
		"-f", "h264", "pipe:1",
	}
}

type sipVideo struct {
	conn rtpTransport
	dst  *net.UDPAddr
	log  *slog.Logger
	ssrc uint32
	t0   time.Time
	path string // ffmpeg

	sendMu sync.Mutex // seq: an old and a new transcoder may overlap
	seq    uint16

	mu      sync.Mutex // feed, close
	cur     *transcoder
	rot     int
	closed  bool
	waiting atomic.Bool // until the next WhatsApp keyframe (start, or after a drop)

	fed, dropped, frames, pkts, keyframes, restarts atomic.Int64
}

// transcoder is one ffmpeg run, for one orientation of the peer's video.
type transcoder struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	in    chan []byte
	stop  chan struct{}
	wg    sync.WaitGroup
}

func newSIPVideo(conn rtpTransport, dst *net.UDPAddr, log *slog.Logger) *sipVideo {
	var b [4]byte
	_, _ = rand.Read(b[:])
	v := &sipVideo{conn: conn, dst: dst, log: log, ssrc: binary.BigEndian.Uint32(b[:]),
		seq: uint16(b[0])<<8 | uint16(b[1])}
	v.waiting.Store(true)
	return v
}

// start checks for ffmpeg; the transcoder itself starts at the peer's first
// keyframe, when its orientation is known.
func (v *sipVideo) start() error {
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return err
	}
	v.path, v.t0 = path, time.Now()
	v.log.Info("video to the phone", "dst", v.dst, "ssrc", v.ssrc)
	return nil
}

func (v *sipVideo) startTranscoder(rotation int) (*transcoder, error) {
	t := &transcoder{cmd: exec.Command(v.path, ffmpegArgs(rotation)...), in: make(chan []byte, 8), stop: make(chan struct{})}
	var err error
	if t.stdin, err = t.cmd.StdinPipe(); err != nil {
		return nil, err
	}
	out, err := t.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	t.cmd.Stderr = &stderr
	if err := t.cmd.Start(); err != nil {
		return nil, err
	}
	t.wg.Add(2)
	go func() { // WhatsApp frames into ffmpeg
		defer t.wg.Done()
		for {
			select {
			case <-t.stop:
				return
			case au := <-t.in:
				if _, err := t.stdin.Write(au); err != nil {
					return
				}
			}
		}
	}()
	go func() { // ffmpeg's frames to the phone
		defer t.wg.Done()
		v.readAUs(out, v.send)
		_ = t.cmd.Wait()
		if stderr.Len() > 0 {
			v.log.Warn("video transcoder", "stderr", stderr.String())
		}
	}()
	return t, nil
}

func (t *transcoder) close() {
	close(t.stop)
	_ = t.stdin.Close()
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	t.wg.Wait()
}

// feed takes one of the peer's frames. Frames start at a keyframe, and a
// dropped frame (transcoder behind) waits for the next one: a decoder
// handed a frame whose reference is missing paints garbage. A keyframe with
// a new orientation restarts the transcoder for it.
func (v *sipVideo) feed(au media.AccessUnit) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return
	}
	if v.waiting.Load() {
		if !au.IDR {
			return
		}
		v.waiting.Store(false)
	}
	if au.IDR && (v.cur == nil || au.Rotation != v.rot) {
		if v.cur != nil {
			v.cur.close()
			v.restarts.Add(1)
		}
		t, err := v.startTranscoder(au.Rotation)
		if err != nil {
			v.cur = nil
			v.waiting.Store(true)
			v.log.Warn("video transcoder not started", "err", err)
			return
		}
		v.cur, v.rot = t, au.Rotation
		v.log.Info("video to the phone: orientation", "rotation", au.Rotation)
	}
	if v.cur == nil {
		return
	}
	select {
	case v.cur.in <- au.Data:
		v.fed.Add(1)
	default:
		v.dropped.Add(1)
		v.waiting.Store(true)
	}
}

func (v *sipVideo) close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return
	}
	v.closed = true
	if v.cur != nil {
		v.cur.close()
		v.cur = nil
	}
	v.log.Info("video to the phone closed", "fed", v.fed.Load(), "dropped", v.dropped.Load(),
		"frames", v.frames.Load(), "keyframes", v.keyframes.Load(), "packets", v.pkts.Load(),
		"restarts", v.restarts.Load())
}

// readAUs splits an Annex-B stream into access units, an access unit
// delimiter (x264 aud=1) starting each; NAL units without start codes.
func (v *sipVideo) readAUs(r io.Reader, emit func([][]byte)) {
	br := bufio.NewReaderSize(r, 64*1024)
	var buf []byte
	var au [][]byte
	chunk := make([]byte, 32*1024)
	for {
		n, err := br.Read(chunk)
		buf = append(buf, chunk[:n]...)
		var nals [][]byte
		nals, buf = splitNALs(buf, err != nil)
		for _, nal := range nals {
			if nal[0]&0x1f == 9 && len(au) > 0 {
				emit(au)
				au = nil
			}
			au = append(au, nal)
		}
		if err != nil {
			if len(au) > 0 {
				emit(au)
			}
			return
		}
	}
}

// splitNALs takes the complete NAL units out of an Annex-B buffer (the last
// one only at the end of the stream); it returns them and the rest.
func splitNALs(buf []byte, final bool) ([][]byte, []byte) {
	var starts []int
	for i := 0; i+2 < len(buf); i++ {
		if buf[i] == 0 && buf[i+1] == 0 && buf[i+2] == 1 {
			starts = append(starts, i+3)
			i += 2
		}
	}
	var out [][]byte
	for k := 0; k+1 < len(starts); k++ {
		nal := bytes.TrimRight(buf[starts[k]:starts[k+1]-3], "\x00")
		if len(nal) > 0 {
			out = append(out, append([]byte(nil), nal...))
		}
	}
	if len(starts) == 0 {
		return out, buf
	}
	last := starts[len(starts)-1]
	if final {
		if nal := buf[last:]; len(nal) > 0 {
			out = append(out, append([]byte(nil), nal...))
		}
		return out, nil
	}
	return out, append([]byte(nil), buf[last-3:]...)
}

// packetizeLegacy: one frame's NAL units as Eyeball "legacy" RTP payloads.
func packetizeLegacy(nals [][]byte) (pays [][]byte, keyframe bool) {
	for _, n := range nals {
		switch t := n[0] & 0x1f; t {
		case 6, 9: // SEI, access unit delimiter: not sent
			continue
		case 5:
			keyframe = true
		}
		if len(n) <= videoMTU {
			pays = append(pays, n)
			continue
		}
		ind := n[0]&0xe0 | 28
		for off := 0; off < len(n); off += videoMTU {
			end := min(off+videoMTU, len(n))
			hdr := n[0] & 0x1f
			if off == 0 {
				hdr |= 0x80
			}
			if end == len(n) {
				hdr |= 0x40
			}
			pays = append(pays, append([]byte{ind, hdr}, n[off:end]...)) // the first keeps n[0]: legacy
		}
	}
	return pays, keyframe
}

func (v *sipVideo) send(nals [][]byte) {
	pays, key := packetizeLegacy(nals)
	if len(pays) == 0 {
		return
	}
	ts := uint32(time.Since(v.t0).Seconds() * videoClock)
	v.sendMu.Lock()
	defer v.sendMu.Unlock()
	for i, p := range pays {
		pkt := make([]byte, 12, 12+len(p))
		pkt[0] = 0x80
		pkt[1] = sipVideoPT
		if i == len(pays)-1 {
			pkt[1] |= 0x80
		}
		binary.BigEndian.PutUint16(pkt[2:], v.seq)
		binary.BigEndian.PutUint32(pkt[4:], ts)
		binary.BigEndian.PutUint32(pkt[8:], v.ssrc)
		v.seq++
		if _, err := v.conn.WriteTo(append(pkt, p...), v.dst); err == nil {
			v.pkts.Add(1)
		}
	}
	v.frames.Add(1)
	if key {
		v.keyframes.Add(1)
	}
}
