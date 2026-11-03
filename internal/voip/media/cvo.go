package media

// CVORotation reads the coordination of video orientation (3GPP TS 26.114,
// "urn:3gpp:video-orientation") from a packet's one-byte header extensions:
// WhatsApp sends it as id 3, one byte (C F R1 R0 in the low bits), under the
// profile 0xDEBE (seen 2026-11-03; 0xBEDE is the RFC 8285 one, same layout).
// It returns the rotation the sender signals, in degrees (0, 90, 180, 270):
// R=1 (90) means the picture must be turned 90 degrees counterclockwise to
// stand upright (checked on a real call's frames).
func CVORotation(h *RtpHeader) (deg int, ok bool) {
	if h == nil || !h.Extension || (h.ExtensionProfile != 0xDEBE && h.ExtensionProfile != 0xBEDE) {
		return 0, false
	}
	e := h.ExtensionData
	for i := 0; i < len(e); {
		b := e[i]
		if b == 0 { // padding
			i++
			continue
		}
		id, n := b>>4, int(b&0x0f)+1
		if id == 15 || i+1+n > len(e) {
			return 0, false
		}
		if id == 3 {
			return int(e[i+1]&0x03) * 90, true
		}
		i += 1 + n
	}
	return 0, false
}
