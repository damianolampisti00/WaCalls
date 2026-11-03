package main

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"wacalls/internal/voip/media"
)

func TestPacketizeLegacyKeepsTheNALHeaderInTheFirstFragment(t *testing.T) {
	sps, pps := []byte{0x67, 0x42, 0xc0, 0x1e}, []byte{0x68, 0xce, 0x3c, 0x80}
	idr := append([]byte{0x65}, bytes.Repeat([]byte{0xab}, 2*videoMTU+10)...)
	pays, key := packetizeLegacy([][]byte{{0x09, 0xf0}, {0x06, 1, 2}, sps, pps, idr})
	if !key || len(pays) != 5 {
		t.Fatalf("keyframe %t, %d payloads", key, len(pays))
	}
	if !bytes.Equal(pays[0], sps) || !bytes.Equal(pays[1], pps) {
		t.Fatal("SPS and PPS must go as single NAL packets")
	}
	f1, f2, f3 := pays[2], pays[3], pays[4]
	if f1[0] != 0x7c || f1[1] != 0x85 || f1[2] != 0x65 || len(f1) != 2+videoMTU {
		t.Fatalf("first fragment % x", f1[:4])
	}
	if f2[1] != 0x05 || f3[1] != 0x45 {
		t.Fatalf("fragment headers %#x %#x", f2[1], f3[1])
	}
	var back []byte
	for _, f := range [][]byte{f1, f2, f3} {
		back = append(back, f[2:]...)
	}
	if !bytes.Equal(back, idr) {
		t.Fatal("fragments do not rebuild the NAL unit (header byte included)")
	}
}

func TestSplitNALs(t *testing.T) {
	stream := []byte{0, 0, 0, 1, 0x09, 0xf0, 0, 0, 1, 0x67, 1, 2, 0, 0, 0, 1, 0x65, 9}
	nals, rest := splitNALs(stream, false)
	if len(nals) != 2 || nals[0][0] != 0x09 || !bytes.Equal(nals[1], []byte{0x67, 1, 2}) {
		t.Fatalf("nals %x", nals)
	}
	if !bytes.HasPrefix(rest, []byte{0, 0, 1, 0x65}) {
		t.Fatalf("rest % x", rest)
	}
	nals, rest = splitNALs(rest, true)
	if len(nals) != 1 || !bytes.Equal(nals[0], []byte{0x65, 9}) || rest != nil {
		t.Fatalf("final %x %x", nals, rest)
	}
}

func TestVideoOfferAndAnswer(t *testing.T) {
	b := string(buildSDPFull("127.0.0.1", 40000, 1, false, true, codecOpusWB, codecPCMU))
	for _, want := range []string{"m=video 40000 RTP/AVP 109\r\n", "a=rtpmap:109 H264/90000\r\n",
		"a=fmtp:109 profile-level-id=42e01f;packetization-mode=1\r\n"} {
		if !strings.Contains(b, want) {
			t.Fatalf("missing %q in\n%s", want, b)
		}
	}
	ans := "v=0\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\na=ice-ufrag:x\r\na=ice-pwd:y\r\n" +
		"m=audio 45536 RTP/AVP 0\r\na=candidate:1 1 UDP 1 192.168.8.149 45536 typ host\r\na=rtcp:45535\r\n" +
		"m=video 45538 RTP/AVP 109\r\na=candidate:1 1 UDP 1 192.168.8.149 45538 typ host\r\n" +
		"a=candidate:1 2 UDP 1 192.168.8.149 45537 typ host\r\na=rtcp:45537\r\na=rtpmap:109 H264/90000\r\n"
	s, err := parseSDP([]byte(ans))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Video || s.VideoPort != 45538 || s.VideoPT != 109 || s.VideoHostPort != 45538 || s.VideoHostRTCPPort != 45537 ||
		s.VideoRTCPPort != 45537 || s.HostPort != 45536 {
		t.Fatalf("parsed %+v", s)
	}
}

// The real transcoder: WhatsApp-like frames (H.264 Main 320x212) in, the
// engine's legacy packets (512x288 baseline, SPS/PPS, keyframes) out.
func TestSIPVideoTranscodesToTheEngine(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	src, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x212:rate=10", "-t", "3",
		"-pix_fmt", "yuv420p", "-c:v", "libx264", "-profile:v", "main", "-bf", "0", "-g", "10",
		"-x264-params", "aud=1:repeat-headers=1", "-f", "h264", "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	var aus []media.AccessUnit
	v0 := &sipVideo{}
	v0.readAUs(bytes.NewReader(src), func(nals [][]byte) {
		var au media.AccessUnit
		for _, n := range nals {
			au.Data = append(append(au.Data, 0, 0, 0, 1), n...)
			if n[0]&0x1f == 5 {
				au.IDR = true
			}
		}
		aus = append(aus, au)
	})
	if len(aus) < 20 || !aus[0].IDR {
		t.Fatalf("source frames %d", len(aus))
	}
	sent := make(chan []byte, 4096)
	tr := newTunnelRTP(func(_ int, p []byte) bool { sent <- append([]byte(nil), p...); return true })
	v := newSIPVideo(tr, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 45538}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := v.start(); err != nil {
		t.Fatal(err)
	}
	defer v.close()
	go func() {
		for _, au := range aus {
			v.feed(au)
			time.Sleep(100 * time.Millisecond)
		}
	}()
	var sps []byte
	seenIDR, frames := false, 0
	deadline := time.After(10 * time.Second)
	for !seenIDR || frames < 10 {
		select {
		case p := <-sent:
			if p[1]&0x7f != sipVideoPT {
				t.Fatalf("payload type %d", p[1]&0x7f)
			}
			pay := p[12:]
			switch pay[0] & 0x1f {
			case 7:
				sps = pay
			case 28:
				if pay[1]&0x80 != 0 && pay[1]&0x1f == 5 {
					if pay[2]&0x1f != 5 {
						t.Fatalf("legacy FU-A start must keep the NAL header: % x", pay[:4])
					}
					seenIDR = true
				}
			case 5:
				seenIDR = true
			}
			if p[1]&0x80 != 0 {
				frames++
			}
		case <-deadline:
			t.Fatalf("after 10 s: sps %t idr %t frames %d", sps != nil, seenIDR, frames)
		}
	}
	if sps == nil || sps[1] != 66 { // profile_idc 66 = baseline
		t.Fatalf("SPS % x", sps)
	}
}

func TestFFmpegArgsTurnThePeerUpright(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("no ffmpeg")
	}
	src, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x212:rate=10", "-t", "1",
		"-pix_fmt", "yuv420p", "-c:v", "libx264", "-f", "h264", "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	for rot, want := range map[int]string{0: "512x288", 90: "288x512", 180: "512x288", 270: "288x512"} {
		cmd := exec.Command("ffmpeg", ffmpegArgs(rot)...)
		cmd.Stdin = bytes.NewReader(src)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("rotation %d: %v", rot, err)
		}
		probe := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height",
			"-of", "csv=s=x:p=0", "-f", "h264", "-")
		probe.Stdin = bytes.NewReader(out)
		got, err := probe.Output()
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(got)) != want {
			t.Fatalf("rotation %d: %q, want %s", rot, got, want)
		}
	}
}

func TestSIPVideoRestartsOnANewOrientation(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	src, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x212:rate=10", "-t", "4",
		"-pix_fmt", "yuv420p", "-c:v", "libx264", "-profile:v", "main", "-bf", "0", "-g", "10",
		"-x264-params", "aud=1:repeat-headers=1", "-f", "h264", "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	var aus []media.AccessUnit
	(&sipVideo{}).readAUs(bytes.NewReader(src), func(nals [][]byte) {
		var au media.AccessUnit
		for _, n := range nals {
			au.Data = append(append(au.Data, 0, 0, 0, 1), n...)
			au.IDR = au.IDR || n[0]&0x1f == 5
		}
		aus = append(aus, au)
	})
	var mu sync.Mutex
	frames := 0
	tr := newTunnelRTP(func(_ int, p []byte) bool {
		if p[1]&0x80 != 0 {
			mu.Lock()
			frames++
			mu.Unlock()
		}
		return true
	})
	v := newSIPVideo(tr, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 45538}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := v.start(); err != nil {
		t.Fatal(err)
	}
	for i, au := range aus {
		if i >= len(aus)/2 {
			au.Rotation = 90 // the peer turned its phone: from the next keyframe
		}
		v.feed(au)
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(time.Second)
	v.close()
	mu.Lock()
	defer mu.Unlock()
	if v.restarts.Load() != 1 || v.rot != 90 {
		t.Fatalf("restarts %d rotation %d", v.restarts.Load(), v.rot)
	}
	if frames < len(aus)/2 {
		t.Fatalf("frames %d of %d", frames, len(aus))
	}
}
