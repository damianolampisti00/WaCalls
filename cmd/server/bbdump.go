package main

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"time"
)

// Recording one call's phone echo, to study it offline: off unless the file
// bbDumpFlag exists when a call starts. The flag is removed at once, so only
// that call is recorded. Three raw files (16 kHz mono int16 LE) go to a new
// directory next to the flag:
//
//	mic.raw  the phone's mic as received, before anything
//	ref.raw  the reference given to AEC3 for each mic frame
//	out.raw  after AEC3 and the residual suppressor (before the mic AGC)
//
// The files hold both sides of a private conversation: delete them once
// studied.
var bbDumpFlag string // set by main: <token file's directory>/echo_dump_next

type bbDump struct {
	mic, ref, out *os.File
}

// newBBDumpIfFlagged starts a recording if the flag is there, nil otherwise.
func newBBDumpIfFlagged() *bbDump {
	if bbDumpFlag == "" {
		return nil
	}
	if err := os.Remove(bbDumpFlag); err != nil {
		return nil // no flag (or not ours to remove)
	}
	dir := filepath.Join(filepath.Dir(bbDumpFlag), "echo_dump", time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil
	}
	d := &bbDump{}
	for _, f := range []struct {
		name string
		to   **os.File
	}{{"mic.raw", &d.mic}, {"ref.raw", &d.ref}, {"out.raw", &d.out}} {
		file, err := os.OpenFile(filepath.Join(dir, f.name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			d.close()
			return nil
		}
		*f.to = file
	}
	return d
}

func bbDumpWrite(f *os.File, pcm []float32) {
	if f == nil {
		return
	}
	buf := make([]byte, 2*len(pcm))
	for i, v := range pcm {
		s := math.Max(-1, math.Min(1, float64(v)))
		binary.LittleEndian.PutUint16(buf[2*i:], uint16(int16(s*32767)))
	}
	_, _ = f.Write(buf)
}

func (d *bbDump) close() {
	if d == nil {
		return
	}
	for _, f := range []*os.File{d.mic, d.ref, d.out} {
		if f != nil {
			_ = f.Close()
		}
	}
}
