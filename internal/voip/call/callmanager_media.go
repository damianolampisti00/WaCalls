package call

import (
	"time"
	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"
	"wacalls/internal/voip/transport"
)

func (m *CallManager) initCodec() {
	if m.codec != nil {
		return
	}
	codec, err := media.NewMLowCodec(media.DefaultCodecOptions)
	if err != nil {
		m.log.Warn("MLow codec unavailable — call will run signaling-only (no audio)", "err", err)
		return
	}
	m.codec = codec
}

func (m *CallManager) FeedCapturedPCM(data []float32) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.codec == nil || m.rtpSession == nil || m.srtpSession == nil || !m.relay.HasConnection() {
		return
	}
	m.lastCaptureAt = time.Now()
	frameSize := m.codec.FrameSize()
	if m.encodeBuf == nil {
		m.encodeBuf = make([]float32, frameSize)
		m.encodeBufPos = 0
	}

	offset := 0
	for offset < len(data) {
		toCopy := min(len(data)-offset, frameSize-m.encodeBufPos)
		copy(m.encodeBuf[m.encodeBufPos:], data[offset:offset+toCopy])
		m.encodeBufPos += toCopy
		offset += toCopy
		if m.encodeBufPos < frameSize {
			break
		}
		frame := make([]float32, frameSize)
		copy(frame, m.encodeBuf)
		m.encodeBufPos = 0

		opus, err := m.codec.Encode(frame)
		if err != nil {
			m.log.Debug("encode error", "err", err)
			continue
		}
		m.sendOpusFrameLocked(opus)
		if m.OnSentPCM != nil {
			m.OnSentPCM(frame)
		}
	}
}

// FeedCapturedVideo sends one Annex-B H.264 access unit from the local SIP
// camera stream to WhatsApp. Video has its own SSRC and SRTP packet index;
// reusing the audio context would corrupt both streams' rollover counters.
func (m *CallManager) FeedCapturedVideo(au media.AccessUnit) {
	if len(au.Data) == 0 || len(au.Data) > 1<<20 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.currentCall == nil || m.currentCall.MediaType != core.CallMediaTypeVideo ||
		m.currentCall.StateData.State != core.CallStateActive ||
		m.videoRTP == nil || m.videoSend == nil || !m.relay.HasConnection() {
		return
	}
	packets, err := m.videoRTP.Packetize(au.Data, au.TS, au.Rotation)
	if err != nil {
		m.log.Debug("video packetize error", "err", err)
		return
	}
	for _, pkt := range packets {
		protected, err := m.videoSend.Protect(pkt)
		if err != nil {
			m.log.Debug("video srtp protect error", "err", err)
			continue
		}
		if m.OnRawMedia != nil {
			m.OnRawMedia(true, protected)
		}
		if m.OnSentVideo != nil {
			m.OnSentVideo(protected)
		}
		m.relay.Broadcast(protected)
	}
}

func (m *CallManager) sendOpusFrameLocked(opus []byte) {
	if m.rtpSession == nil || m.srtpSession == nil {
		return
	}
	marker := !m.firstPacketSent
	pkt := m.rtpSession.CreatePacketWithDuration(opus, m.codec.FrameSize(), marker)
	if m.debeEnabled {
		pkt.Header.Extension = true
		pkt.Header.ExtensionProfile = 0xbede
		pkt.Header.ExtensionData = nil
	}
	m.firstPacketSent = true

	srtp, err := m.srtpSession.Protect(pkt)
	if err != nil {
		m.log.Debug("srtp protect error", "err", err)
		return
	}
	if m.OnRawMedia != nil {
		m.OnRawMedia(true, srtp)
	}
	m.relay.Broadcast(srtp)
}

func (m *CallManager) startSilenceKeepaliveLocked() {
	if m.keepaliveStop != nil || m.codec == nil {
		return
	}
	stop := make(chan struct{})
	m.keepaliveStop = stop
	frameSize := m.codec.FrameSize()
	go func() {
		ticker := time.NewTicker(60 * time.Millisecond)
		defer ticker.Stop()
		silence := make([]float32, frameSize)
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				m.mu.Lock()
				ready := m.codec != nil && m.rtpSession != nil && m.srtpSession != nil && m.relay.HasConnection()
				idle := time.Since(m.lastCaptureAt) > 120*time.Millisecond
				if ready && idle {
					if opus, err := m.codec.Encode(silence); err == nil {
						m.sendOpusFrameLocked(opus)
						if m.OnSentPCM != nil {
							m.OnSentPCM(silence)
						}
					}
				}
				m.mu.Unlock()
			}
		}
	}()
}

func (m *CallManager) onRelayData(data []byte) {
	if m.OnRawMedia != nil {
		m.OnRawMedia(false, data)
	}
	if transport.IsStunPacket(data) {
		return
	}
	if !transport.IsRtpPacket(data) {
		return
	}
	if len(data) < 12 {
		return
	}
	m.stats.add(data)
	if m.stats.due() {
		m.stats.report(m.log, false)
	}
	pt := data[1] & 0x7f
	if pt == core.PayloadTypeWhatsAppH264 {
		m.onPeerVideoRTP(data)
		return
	}
	// 122 ("mlow-1"): where a video call's peer moves MLOW (capability 38,
	// enable_mlow_separate_pt_tx); read exactly like 120.
	if pt != core.PayloadTypeWhatsAppOpus && pt != core.PayloadTypeWhatsAppMLowSeparate {
		return
	}

	m.mu.Lock()
	if m.srtpSession == nil || m.codec == nil {
		m.mu.Unlock()
		return
	}
	ssrc := uint32(data[8])<<24 | uint32(data[9])<<16 | uint32(data[10])<<8 | uint32(data[11])
	if ssrc == m.selfSsrc {
		m.mu.Unlock()
		return
	}
	if !m.actualPeerSet {
		m.actualPeerSet = true
		if !containsSsrc(m.peerSsrcs, ssrc) {
			m.peerSsrcs = []uint32{ssrc}
			m.relay.SetSubscriptionSsrc(ssrc)
			go m.relay.ResendSubscriptions()
		}
	}
	srtp := m.srtpSession
	codec := m.codec
	m.mu.Unlock()

	pkt, err := srtp.Unprotect(data)
	if err != nil {
		m.log.Debug("srtp unprotect error", "err", err)
		return
	}
	if len(pkt.Payload) == 0 {
		return
	}
	pcm, err := codec.Decode(pkt.Payload)
	if err != nil {
		return
	}
	fs := codec.FrameSize()
	if len(pcm) > fs && len(pcm)%fs == 0 {
		if m.OnPeerAudio != nil {
			for off := 0; off < len(pcm); off += fs {
				m.OnPeerAudio(pcm[off : off+fs])
			}
		}
		return
	}
	pcm = media.NormalizeFrame(pcm, fs)
	if m.OnPeerAudio != nil {
		m.OnPeerAudio(pcm)
	}
}

// onPeerVideoRTP decrypts one of the peer's video packets with that
// stream's own SRTP context (same keys as the audio, own ROC per SSRC).
func (m *CallManager) onPeerVideoRTP(data []byte) {
	if m.OnPeerVideo == nil {
		return
	}
	ssrc := uint32(data[8])<<24 | uint32(data[9])<<16 | uint32(data[10])<<8 | uint32(data[11])
	m.mu.Lock()
	sess := m.srtpSession
	if sess == nil || ssrc == m.selfSsrc {
		m.mu.Unlock()
		return
	}
	if m.videoSess != sess {
		m.videoSess, m.videoRecv = sess, map[uint32]*media.SrtpContext{}
	}
	ctx := m.videoRecv[ssrc]
	if ctx == nil {
		var err error
		if ctx, err = sess.NewRecvContext(); err != nil {
			m.mu.Unlock()
			return
		}
		m.videoRecv[ssrc] = ctx
		m.log.Info("peer video stream", "ssrc", ssrc)
	}
	m.mu.Unlock()
	pkt, err := ctx.Unprotect(data)
	if err != nil || len(pkt.Payload) == 0 {
		return
	}
	m.OnPeerVideo(pkt)
}
