// videodiag analyses a call capture (WaCalls -wa-call-capture): one report
// instead of one test call per question.
//
//	go run ./cmd/videodiag <capture dir>
//
// It reads media.pcap and keys.json, and writes into the same directory:
//
//	report.txt   streams (direction, SSRC, payload type): packets, seq gaps,
//	             timestamp steps, markers, sizes, RTP header extensions; for
//	             the peer's video the key that decrypts it (the session's, or
//	             an alternative derivation), NAL unit types, frames, IDRs
//	video-<ssrc>.h264   the decrypted video, Annex-B
//	frames/      a few PNG frames (with ffmpeg)
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"wacalls/internal/voip/call"
	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"
)

type pkt struct {
	in   bool
	tUs  int64
	data []byte
}

func readPcap(path string) ([]pkt, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) < 24 || binary.LittleEndian.Uint32(b) != 0xa1b2c3d4 {
		return nil, fmt.Errorf("%s: not a pcap", path)
	}
	var out []pkt
	for off := 24; off+16 <= len(b); {
		sec, usec := binary.LittleEndian.Uint32(b[off:]), binary.LittleEndian.Uint32(b[off+4:])
		n := int(binary.LittleEndian.Uint32(b[off+8:]))
		off += 16
		if off+n > len(b) || n < 28 {
			break
		}
		ip := b[off : off+n]
		dport := binary.BigEndian.Uint16(ip[22:])
		out = append(out, pkt{in: dport == 40000, tUs: int64(sec)*1e6 + int64(usec), data: ip[28:]})
		off += n
	}
	return out, nil
}

type stream struct {
	in              bool
	ssrc            uint32
	pt              byte
	n, markers      int
	bytes           int
	firstSeq        uint16
	lastSeq         uint16
	gaps, reorder   int
	tsSteps         map[uint32]int
	lastTS          uint32
	firstUs, lastUs int64
	ext             map[string]int // "profile/id" -> packets
	sizes           []int
	pkts            [][]byte
}

func isRTP(p []byte) bool { return len(p) >= 12 && p[0]>>6 == 2 }

// rtpExt returns the one-byte (0xBEDE) extension elements: id -> length.
func rtpExt(p []byte) (profile uint16, ids map[int]int) {
	if p[0]&0x10 == 0 {
		return 0, nil
	}
	off := 12 + 4*int(p[0]&0x0f)
	if len(p) < off+4 {
		return 0, nil
	}
	profile = binary.BigEndian.Uint16(p[off:])
	words := int(binary.BigEndian.Uint16(p[off+2:]))
	ext := p[off+4:]
	if len(ext) > 4*words {
		ext = ext[:4*words]
	}
	ids = map[int]int{}
	if profile == 0xbede || profile == 0xdebe { // WhatsApp writes the one-byte form as 0xDEBE
		for i := 0; i < len(ext); {
			if ext[i] == 0 {
				i++
				continue
			}
			id, l := int(ext[i]>>4), int(ext[i]&0x0f)+1
			if id == 15 {
				break
			}
			ids[id] = l
			i += 1 + l
		}
	}
	return profile, ids
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: videodiag <capture dir>")
		os.Exit(2)
	}
	dir := os.Args[1]
	pkts, err := readPcap(filepath.Join(dir, "media.pcap"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var keys call.CallKeyInfo
	if b, err := os.ReadFile(filepath.Join(dir, "keys.json")); err == nil {
		_ = json.Unmarshal(b, &keys)
	}
	var rep bytes.Buffer
	w := io.MultiWriter(&rep, os.Stdout)
	fmt.Fprintf(w, "capture %s: %d packets\n", dir, len(pkts))
	fmt.Fprintf(w, "keys: our %s peer %s participants %v self_ssrc %d peer_ssrcs %v\n",
		keys.OurDevice, keys.PeerDevice, keys.Participants, keys.SelfSSRC, keys.PeerSSRCs)

	streams := map[string]*stream{}
	var order []string
	stun, other := map[bool]int{}, map[bool]int{}
	var t0 int64
	for i, p := range pkts {
		if i == 0 {
			t0 = p.tUs
		}
		d := p.data
		if len(d) >= 20 && d[0]&0xc0 == 0 && binary.BigEndian.Uint32(d[4:]) == 0x2112A442 {
			stun[p.in]++
			continue
		}
		if !isRTP(d) {
			other[p.in]++
			continue
		}
		pt := d[1] & 0x7f
		rtcp := pt >= 72 && pt <= 76 // RTCP packet types 200-204 (RTP never uses 72-76)
		ssrc := binary.BigEndian.Uint32(d[8:])
		if rtcp {
			pt, ssrc = d[1], binary.BigEndian.Uint32(d[4:])
		}
		key := fmt.Sprintf("%t/%d/%d", p.in, ssrc, pt)
		s := streams[key]
		if s == nil {
			s = &stream{in: p.in, ssrc: ssrc, pt: pt, tsSteps: map[uint32]int{}, ext: map[string]int{}, firstUs: p.tUs}
			s.firstSeq = binary.BigEndian.Uint16(d[2:])
			streams[key] = s
			order = append(order, key)
		}
		seq, ts := binary.BigEndian.Uint16(d[2:]), binary.BigEndian.Uint32(d[4:])
		if s.n > 0 {
			switch diff := int16(seq - s.lastSeq); {
			case diff > 1:
				s.gaps += int(diff) - 1
			case diff <= 0:
				s.reorder++
			}
			if ts != s.lastTS {
				s.tsSteps[ts-s.lastTS]++
			}
		}
		s.n++
		s.bytes += len(d)
		s.lastSeq, s.lastTS, s.lastUs = seq, ts, p.tUs
		if d[1]&0x80 != 0 && !rtcp {
			s.markers++
		}
		if prof, ids := rtpExt(d); prof != 0 {
			for id, l := range ids {
				s.ext[fmt.Sprintf("%04x/id%d(len%d)", prof, id, l)]++
			}
			if len(ids) == 0 {
				s.ext[fmt.Sprintf("%04x/-", prof)]++
			}
		}
		s.sizes = append(s.sizes, len(d))
		s.pkts = append(s.pkts, d)
	}
	fmt.Fprintf(w, "stun in %d out %d; non-RTP in %d out %d\n\n", stun[true], stun[false], other[true], other[false])

	for _, k := range order {
		s := streams[k]
		dir := "out"
		if s.in {
			dir = "in "
		}
		sort.Ints(s.sizes)
		med := s.sizes[len(s.sizes)/2]
		var steps []string
		for st, n := range s.tsSteps {
			if n*20 >= s.n || len(steps) < 3 {
				steps = append(steps, fmt.Sprintf("%d×%d", st, n))
			}
		}
		sort.Strings(steps)
		var exts []string
		for e, n := range s.ext {
			exts = append(exts, fmt.Sprintf("%s:%d", e, n))
		}
		sort.Strings(exts)
		fmt.Fprintf(w, "%s ssrc %10d pt %3d: %5d pk %7d B, seq %d..%d gaps %d reorder %d, markers %d, size med %d max %d, t %.1f..%.1f s\n",
			dir, s.ssrc, s.pt, s.n, s.bytes, s.firstSeq, s.lastSeq, s.gaps, s.reorder, s.markers, med, s.sizes[len(s.sizes)-1],
			float64(s.firstUs-t0)/1e6, float64(s.lastUs-t0)/1e6)
		fmt.Fprintf(w, "    ts steps %v\n    ext %v\n", steps, exts)
	}

	// The peer's video: which key decrypts it, then the H.264.
	for _, k := range order {
		s := streams[k]
		if !s.in || s.pt != core.PayloadTypeWhatsAppH264 {
			continue
		}
		fmt.Fprintf(w, "\n== video ssrc %d (%d packets)\n", s.ssrc, s.n)
		best, bestScore, bestName := []([]byte)(nil), -1.0, ""
		for _, cand := range keyCandidates(keys) {
			tags := []int{core.SRTPRecvAuthTagLen}
			for _, t := range []int{10, 4, 0} {
				if t != core.SRTPRecvAuthTagLen {
					tags = append(tags, t)
				}
			}
			for _, tag := range tags {
				ctx, err := media.NewSrtpContext(cand.km, tag)
				if err != nil {
					continue
				}
				var pays [][]byte
				for _, d := range s.pkts {
					if p, err := ctx.Unprotect(d); err == nil {
						pays = append(pays, p.Payload)
					}
				}
				sc := nalScore(pays)
				fmt.Fprintf(w, "  key %-40s tag %2d: plausible NAL headers %.0f%%\n", cand.name, tag, 100*sc)
				if sc > bestScore {
					best, bestScore, bestName = nil, sc, fmt.Sprintf("%s tag %d", cand.name, tag)
					ctx2, _ := media.NewSrtpContext(cand.km, tag)
					for _, d := range s.pkts {
						if p, err := ctx2.Unprotect(d); err == nil {
							best = append(best, d[:p.Header.Size()]) // header kept for seq/ts/marker
							best = append(best, p.Payload)
						}
					}
				}
			}
		}
		fmt.Fprintf(w, "  best: %s (%.0f%%)\n", bestName, 100*bestScore)
		if bestScore < 0.5 {
			fmt.Fprintln(w, "  no key gives H.264: decryption is the problem")
			continue
		}
		var dp media.H264Depacketizer
		var types [32]int
		var firstHeads []string
		var aus []media.AccessUnit
		for i := 0; i+1 < len(best); i += 2 {
			h, pay := best[i], best[i+1]
			if len(pay) > 0 {
				types[pay[0]&0x1f]++
			}
			if len(firstHeads) < 10 {
				firstHeads = append(firstHeads, fmt.Sprintf("seq %d m=%t % x", binary.BigEndian.Uint16(h[2:]), h[1]&0x80 != 0, pay[:min(len(pay), 8)]))
			}
			aus = append(aus, dp.Push(binary.BigEndian.Uint16(h[2:]), binary.BigEndian.Uint32(h[4:]), h[1]&0x80 != 0, pay)...)
		}
		var tl []string
		for t, n := range types {
			if n > 0 {
				tl = append(tl, fmt.Sprintf("%d:%d", t, n))
			}
		}
		idr := 0
		for _, a := range aus {
			if a.IDR {
				idr++
			}
		}
		fmt.Fprintf(w, "  NAL/packet types %v; frames %d, with IDR %d\n", tl, len(aus), idr)
		for _, h := range firstHeads {
			fmt.Fprintf(w, "    %s\n", h)
		}
		if len(aus) > 0 {
			describeAU(w, aus[0].Data)
		}
		all := filepath.Join(dir, fmt.Sprintf("video-%d-all.h264", s.ssrc))
		fa, _ := os.Create(all)
		for _, a := range aus {
			_, _ = fa.Write(a.Data)
		}
		fa.Close()
		fmt.Fprintf(w, "  all frames (no IDR wait) %s\n", all)
		out := filepath.Join(dir, fmt.Sprintf("video-%d.h264", s.ssrc))
		f, _ := os.Create(out)
		started := false
		for _, a := range aus {
			if !started && !a.IDR {
				continue
			}
			started = true
			_, _ = f.Write(a.Data)
		}
		f.Close()
		fmt.Fprintf(w, "  written %s\n", out)
		if _, err := exec.LookPath("ffprobe"); err == nil {
			o, _ := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name,profile,level,width,height", "-of", "compact", out).CombinedOutput()
			fmt.Fprintf(w, "  ffprobe: %s", o)
			fr := filepath.Join(dir, "frames")
			_ = os.MkdirAll(fr, 0o700)
			o, err := exec.Command("ffmpeg", "-v", "error", "-y", "-i", out, "-vf", "select=not(mod(n\\,30))", "-fps_mode", "vfr", "-frames:v", "8", filepath.Join(fr, fmt.Sprintf("%d-%%02d.png", s.ssrc))).CombinedOutput()
			fmt.Fprintf(w, "  frames -> %s %v %s\n", fr, err, strings.TrimSpace(string(o)))
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "report.txt"), rep.Bytes(), 0o600)
}

type keyCand struct {
	name string
	km   core.SrtpKeyingMaterial
}

// keyCandidates: the session's receive key, then the derivations from the
// call keys for every device that could be the video's sender.
func keyCandidates(k call.CallKeyInfo) []keyCand {
	var out []keyCand
	if len(k.RecvKey) > 0 {
		out = append(out, keyCand{"session recv", core.SrtpKeyingMaterial{MasterKey: k.RecvKey, MasterSalt: k.RecvSalt}})
	}
	jids := append([]string{k.PeerDevice, k.OurDevice}, k.Participants...)
	seen := map[string]bool{}
	for _, ck := range []struct {
		n string
		k []byte
	}{{"call key", k.EncryptionKey}, {"peer call key", k.PeerCallKey}} {
		if len(ck.k) == 0 {
			continue
		}
		for _, j := range jids {
			if j == "" || seen[ck.n+j] {
				continue
			}
			seen[ck.n+j] = true
			if km, err := media.DerivePerJidSrtpKey(ck.k, j); err == nil {
				out = append(out, keyCand{ck.n + " / " + j, km})
			}
		}
	}
	return out
}

// nalScore: the share of payloads that look like H.264 RTP (F bit clear,
// NAL type 1-23, STAP-A or a consistent FU-A header).
func nalScore(pays [][]byte) float64 {
	if len(pays) == 0 {
		return 0
	}
	ok := 0
	for _, p := range pays {
		if len(p) == 0 || p[0]&0x80 != 0 {
			continue
		}
		switch t := p[0] & 0x1f; {
		case t >= 1 && t <= 23, t == 24:
			ok++
		case t == 28 && len(p) > 1:
			if fu := p[1]; fu&0x20 == 0 && fu&0xc0 != 0xc0 {
				ok++
			}
		}
	}
	return float64(ok) / float64(len(pays))
}

// describeAU lays out one access unit: its NAL units, and inside a NAL unit
// bigger than an SPS/PPS should be, any AVCC-style (4-byte length) list.
func describeAU(w io.Writer, au []byte) {
	fmt.Fprintf(w, "  first frame %d B: % x ...\n", len(au), au[:min(len(au), 48)])
	nals := bytes.Split(au, []byte{0, 0, 0, 1})
	for _, n := range nals {
		if len(n) == 0 {
			continue
		}
		fmt.Fprintf(w, "    NAL type %d, %d B\n", n[0]&0x1f, len(n))
		if t := n[0] & 0x1f; (t == 7 || t == 8) && len(n) > 64 {
			// try a length-prefixed list after the first bytes
			for start := 1; start < 64 && start+4 < len(n); start++ {
				var parts []string
				off := start
				for off+4 <= len(n) {
					l := int(binary.BigEndian.Uint32(n[off:]))
					if l <= 0 || off+4+l > len(n) {
						break
					}
					parts = append(parts, fmt.Sprintf("type %d %dB", n[off+4]&0x1f, l))
					off += 4 + l
				}
				if off == len(n) && len(parts) > 0 {
					fmt.Fprintf(w, "      AVCC list from byte %d: %v\n", start, parts)
				}
			}
			for i := 1; i+1 < len(n); i++ { // other NAL headers that may hide inside
				if n[i] == 0x65 || n[i] == 0x68 || n[i] == 0x25 || n[i] == 0x28 {
					if i < 40 {
						fmt.Fprintf(w, "      byte %d = %#02x\n", i, n[i])
					}
				}
			}
		}
	}
}
