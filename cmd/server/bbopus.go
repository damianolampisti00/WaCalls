package main

// Opus for the Berry Bridge phone link (libopus through cgo): 16 kHz mono,
// 20 ms packets, ~24 kbit/s each way instead of 256 kbit/s of PCM. The phone
// shares the 2.4 GHz radio between Wi-Fi and a Bluetooth headset, and over a
// weak Wi-Fi the raw PCM stream stalled.

/*
#cgo pkg-config: opus
#include <opus.h>

// opus_encoder_ctl is variadic, which cgo can't call: wrap the settings.
static int bb_opus_setup(OpusEncoder *e, int bitrate) {
	int r = opus_encoder_ctl(e, OPUS_SET_BITRATE(bitrate));
	if (r != OPUS_OK) return r;
	r = opus_encoder_ctl(e, OPUS_SET_COMPLEXITY(8));
	if (r != OPUS_OK) return r;
	return opus_encoder_ctl(e, OPUS_SET_SIGNAL(OPUS_SIGNAL_VOICE));
}
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

const (
	bbOpusRate    = 16000
	bbOpusFrame   = 320 // samples per packet: 20 ms at 16 kHz
	bbOpusBitrate = 24000
	bbOpusMaxPkt  = 1275
)

type bbOpusEncoder struct{ e *C.OpusEncoder }

func newBBOpusEncoder() (*bbOpusEncoder, error) {
	var err C.int
	e := C.opus_encoder_create(bbOpusRate, 1, C.OPUS_APPLICATION_VOIP, &err)
	if err != C.OPUS_OK || e == nil {
		return nil, fmt.Errorf("opus encoder: %d", int(err))
	}
	if r := C.bb_opus_setup(e, bbOpusBitrate); r != C.OPUS_OK {
		C.opus_encoder_destroy(e)
		return nil, fmt.Errorf("opus encoder setup: %d", int(r))
	}
	return &bbOpusEncoder{e: e}, nil
}

// encode turns bbOpusFrame samples into one packet, appended to out.
func (o *bbOpusEncoder) encode(pcm []int16, out []byte) ([]byte, error) {
	if len(pcm) != bbOpusFrame {
		return out, errors.New("opus: wrong frame size")
	}
	var buf [bbOpusMaxPkt]byte
	n := C.opus_encode(o.e, (*C.opus_int16)(unsafe.Pointer(&pcm[0])), bbOpusFrame,
		(*C.uchar)(unsafe.Pointer(&buf[0])), bbOpusMaxPkt)
	if n < 0 {
		return out, fmt.Errorf("opus encode: %d", int(n))
	}
	return append(out, buf[:n]...), nil
}

func (o *bbOpusEncoder) close() {
	if o != nil && o.e != nil {
		C.opus_encoder_destroy(o.e)
		o.e = nil
	}
}

type bbOpusDecoder struct{ d *C.OpusDecoder }

func newBBOpusDecoder() (*bbOpusDecoder, error) {
	var err C.int
	d := C.opus_decoder_create(bbOpusRate, 1, &err)
	if err != C.OPUS_OK || d == nil {
		return nil, fmt.Errorf("opus decoder: %d", int(err))
	}
	return &bbOpusDecoder{d: d}, nil
}

// decode returns the packet's samples (a 120 ms buffer covers any packet).
func (o *bbOpusDecoder) decode(pkt []byte) ([]int16, error) {
	if len(pkt) == 0 {
		return nil, errors.New("opus: empty packet")
	}
	pcm := make([]int16, bbOpusRate*120/1000)
	n := C.opus_decode(o.d, (*C.uchar)(unsafe.Pointer(&pkt[0])), C.opus_int32(len(pkt)),
		(*C.opus_int16)(unsafe.Pointer(&pcm[0])), C.int(len(pcm)), 0)
	if n < 0 {
		return nil, fmt.Errorf("opus decode: %d", int(n))
	}
	return pcm[:n], nil
}

func (o *bbOpusDecoder) close() {
	if o != nil && o.d != nil {
		C.opus_decoder_destroy(o.d)
		o.d = nil
	}
}
