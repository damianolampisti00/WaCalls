package call

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// rtpStats counts what the relay delivers, per payload type and SSRC, with
// the one-byte header extension ids (WhatsApp's 0xdebe profile) seen on each:
// in a video call the peer sends video (PT 97), video FEC (103), MLOW on its
// separate type (122) and audio carried inside the video packets (id 7).
// Logged every 10 s and when the media stops, to see what a call really
// carries before decoding it.
type rtpStats struct {
	mu      sync.Mutex
	streams map[uint64]*rtpStream
	started time.Time
	logged  time.Time
}

type rtpStream struct {
	pt      uint8
	ssrc    uint32
	packets int
	bytes   int
	marker  int
	exts    map[uint8]int
}

func (s *rtpStats) add(data []byte) {
	if len(data) < 12 {
		return
	}
	pt := data[1] & 0x7f
	ssrc := uint32(data[8])<<24 | uint32(data[9])<<16 | uint32(data[10])<<8 | uint32(data[11])
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streams == nil {
		s.streams = map[uint64]*rtpStream{}
		s.started = time.Now()
		s.logged = s.started
	}
	k := uint64(pt)<<32 | uint64(ssrc)
	st := s.streams[k]
	if st == nil {
		st = &rtpStream{pt: pt, ssrc: ssrc, exts: map[uint8]int{}}
		s.streams[k] = st
	}
	st.packets++
	st.bytes += len(data)
	if data[1]&0x80 != 0 {
		st.marker++
	}
	for _, id := range rtpExtensionIDs(data) {
		st.exts[id]++
	}
}

// rtpExtensionIDs lists the ids of a one-byte-header extension block
// (RFC 8285 profile 0xBEDE, or WhatsApp's 0xDEBE).
func rtpExtensionIDs(p []byte) []uint8 {
	if len(p) < 12 || p[0]&0x10 == 0 {
		return nil
	}
	off := 12 + 4*int(p[0]&0x0f)
	if len(p) < off+4 {
		return nil
	}
	profile := uint16(p[off])<<8 | uint16(p[off+1])
	words := int(p[off+2])<<8 | int(p[off+3])
	start, end := off+4, off+4+4*words
	if end > len(p) || (profile != 0xbede && profile != 0xdebe) {
		return nil
	}
	var ids []uint8
	for i := start; i < end; {
		b := p[i]
		if b == 0 { // padding
			i++
			continue
		}
		id, l := b>>4, int(b&0x0f)+1
		if id == 15 {
			break
		}
		ids = append(ids, id)
		i += 1 + l
	}
	return ids
}

// due reports whether a periodic report is due (every 10 s).
func (s *rtpStats) due() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streams == nil || time.Since(s.logged) < 10*time.Second {
		return false
	}
	s.logged = time.Now()
	return true
}

func (s *rtpStats) report(log *slog.Logger, final bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streams == nil {
		return
	}
	keys := make([]uint64, 0, len(s.streams))
	for k := range s.streams {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	var b strings.Builder
	for _, k := range keys {
		st := s.streams[k]
		fmt.Fprintf(&b, " pt%d/%08x:%dpk,%dB,%dM", st.pt, st.ssrc, st.packets, st.bytes, st.marker)
		if len(st.exts) > 0 {
			ids := make([]int, 0, len(st.exts))
			for id := range st.exts {
				ids = append(ids, int(id))
			}
			sort.Ints(ids)
			b.WriteString(",ext")
			for _, id := range ids {
				fmt.Fprintf(&b, "[%d]=%d", id, st.exts[uint8(id)])
			}
		}
	}
	log.Info("call rtp streams", "final", final, "secs", int(time.Since(s.started).Seconds()), "streams", strings.TrimSpace(b.String()))
	if final {
		s.streams = nil
	}
}
