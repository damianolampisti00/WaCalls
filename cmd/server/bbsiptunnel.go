package main

// SIP over the Berry Bridge link: away from home the phone's native call
// engine (EBMService, see sipgateway.go) cannot reach the SIP gateway, but
// the Berry Bridge app keeps its WebSocket to this server open everywhere.
// So the app relays: the engine talks SIP to 127.0.0.1:5060 and RTP to
// 127.0.0.1:40000 on the phone, the app carries both through the link.
//
// Protocol additions (version 1, see bbgateway.go):
//   {"type":"sip.open","resume":bool}  phone -> server: the engine connected to
//       the relay (resume: it was already connected before the link came back,
//       so it won't REGISTER again soon). The server answers nothing.
//   {"type":"sip.close"}  both ways: the SIP connection is gone.
//   Binary kind 0x10: bytes of the SIP stream, both ways (header as audio).
//   Binary kind 0x11: one RTP datagram. [2:4] = the phone-side UDP port,
//       uint16 LE: phone -> server the engine's sending port, server -> phone
//       the port to send it to (from the engine's SDP). The relay's own port
//       is bbTunnelRTPPort.

import (
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const (
	bbKindSIP       = 0x10
	bbKindRTP       = 0x11
	bbTunnelRTPPort = 40000 // the relay's UDP port on the phone (one call at a time)
)

type tunnelPkt struct {
	port int
	data []byte
}

// tunnelRTP is an rtpTransport over the link.
type tunnelRTP struct {
	port int
	in   chan tunnelPkt
	send func(dstPort int, p []byte) bool
	done chan struct{}
	once sync.Once
}

func newTunnelRTP(send func(int, []byte) bool) *tunnelRTP {
	return &tunnelRTP{port: bbTunnelRTPPort, in: make(chan tunnelPkt, 64), send: send, done: make(chan struct{})}
}

func (t *tunnelRTP) WriteTo(p []byte, dst *net.UDPAddr) (int, error) {
	if !t.send(dst.Port, p) {
		return 0, errors.New("tunnel rtp: link busy or down")
	}
	return len(p), nil
}

func (t *tunnelRTP) ReadFrom(p []byte) (int, *net.UDPAddr, error) {
	select {
	case pk := <-t.in:
		return copy(p, pk.data), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: pk.port}, nil
	case <-t.done:
		return 0, nil, net.ErrClosed
	}
}

func (t *tunnelRTP) Close() error   { t.once.Do(func() { close(t.done) }); return nil }
func (t *tunnelRTP) LocalPort() int { return t.port }

func (t *tunnelRTP) deliver(port int, data []byte) {
	select {
	case t.in <- tunnelPkt{port, data}:
	case <-t.done:
	default: // the call core is behind: drop, like a lost packet
	}
}

// bbSIPTunnel is one SIP connection over a phone link.
type bbSIPTunnel struct {
	pipe net.Conn // our end; the SIP gateway reads the other
	rtp  atomic.Pointer[tunnelRTP]
	once sync.Once
}

func (t *bbSIPTunnel) close() {
	t.once.Do(func() {
		_ = t.pipe.Close()
		if r := t.rtp.Load(); r != nil {
			_ = r.Close()
		}
	})
}

func (c *bbConn) tunnelFrame(kind byte, port int, payload []byte) []byte {
	f := make([]byte, bbHeaderLen, bbHeaderLen+len(payload))
	f[0] = kind
	binary.LittleEndian.PutUint16(f[2:], uint16(port))
	binary.LittleEndian.PutUint32(f[4:], c.tunSeq.Add(1))
	binary.LittleEndian.PutUint32(f[8:], uint32(time.Since(c.start).Milliseconds()))
	return append(f, payload...)
}

// enqueueReliable queues a frame that must not be lost (SIP), waiting a
// little for room like control messages do.
func (c *bbConn) enqueueReliable(data []byte) bool {
	select {
	case c.out <- bbMsg{websocket.MessageBinary, data}:
		return true
	case <-time.After(5 * time.Second):
	case <-c.ctx.Done():
	}
	return false
}

// openSIPTunnel: the phone's engine connected to the app's relay.
func (g *bbGateway) openSIPTunnel(c *bbConn, resume bool) error {
	if g.sipGW == nil {
		return errors.New("SIP gateway not enabled on this server (-sip-addr)")
	}
	ours, theirs := net.Pipe()
	t := &bbSIPTunnel{pipe: ours}
	c.sipMu.Lock()
	old := c.sip
	c.sip = t
	c.sipMu.Unlock()
	if old != nil {
		old.close()
	}
	newMedia := func() (rtpTransport, error) {
		r := newTunnelRTP(func(dst int, p []byte) bool {
			return c.enqueue(bbMsg{websocket.MessageBinary, c.tunnelFrame(bbKindRTP, dst, p)})
		})
		if prev := t.rtp.Swap(r); prev != nil {
			_ = prev.Close()
		}
		return r, nil
	}
	go g.sipGW.serveTunnel(c.ctx, theirs, newMedia, resume)
	go func() { // SIP from the gateway to the phone
		buf := make([]byte, 16*1024)
		for {
			n, err := ours.Read(buf)
			if n > 0 && !c.enqueueReliable(c.tunnelFrame(bbKindSIP, 0, append([]byte(nil), buf[:n]...))) {
				t.close()
			}
			if err != nil {
				c.sipMu.Lock()
				current := c.sip == t
				c.sipMu.Unlock()
				if current && c.ctx.Err() == nil {
					c.sendJSON(map[string]any{"type": "sip.close"})
				}
				return
			}
		}
	}()
	g.log.Info("phone SIP relay open", "resume", resume)
	return nil
}

func (c *bbConn) closeSIPTunnel() {
	c.sipMu.Lock()
	t := c.sip
	c.sip = nil
	c.sipMu.Unlock()
	if t != nil {
		t.close()
	}
}

// onTunnelFrame handles binary kinds 0x10/0x11; false: not a tunnel frame.
func (g *bbGateway) onTunnelFrame(c *bbConn, frame []byte) bool {
	if len(frame) < bbHeaderLen || (frame[0] != bbKindSIP && frame[0] != bbKindRTP) {
		return false
	}
	c.sipMu.Lock()
	t := c.sip
	c.sipMu.Unlock()
	if t == nil {
		return true
	}
	payload := frame[bbHeaderLen:]
	if frame[0] == bbKindSIP {
		_ = t.pipe.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := t.pipe.Write(payload); err != nil {
			t.close()
		}
		return true
	}
	if r := t.rtp.Load(); r != nil {
		r.deliver(int(binary.LittleEndian.Uint16(frame[2:4])), append([]byte(nil), payload...))
	}
	return true
}
