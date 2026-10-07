package main

import (
	"context"
	"encoding/binary"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const testToken = "test-token-0123456789"

func dialGateway(t *testing.T) (*bbGateway, *websocket.Conn, context.Context) {
	t.Helper()
	gw := newBBGateway(nil, NewBroker(), testToken, "", slog.Default())
	srv := httptest.NewServer(gw.routes())
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	h := http.Header{}
	h.Set("Authorization", "Bearer "+testToken)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	typ, data, err := ws.Read(ctx)
	if err != nil || typ != websocket.MessageText || !strings.Contains(string(data), `"hello"`) {
		t.Fatalf("expected hello, got %v %s %v", typ, data, err)
	}
	return gw, ws, ctx
}

func TestBBGatewayRejectsWithoutToken(t *testing.T) {
	gw := newBBGateway(nil, NewBroker(), testToken, "", slog.Default())
	srv := httptest.NewServer(gw.routes())
	defer srv.Close()
	for _, auth := range []string{"", "Bearer wrong-token-0123456789"} {
		req, _ := http.NewRequest("GET", srv.URL+"/ws", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("auth %q: status %d, want 401", auth, resp.StatusCode)
		}
	}
}

func TestBBGatewayEchoAndPing(t *testing.T) {
	_, ws, ctx := dialGateway(t)
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"type":"test.echo","on":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"type":"ping","t":42}`)); err != nil {
		t.Fatal(err)
	}
	_, data, err := ws.Read(ctx)
	if err != nil || !strings.Contains(string(data), `"pong"`) || !strings.Contains(string(data), `42`) {
		t.Fatalf("pong: %s %v", data, err)
	}
	frame := make([]byte, bbHeaderLen+640)
	frame[0] = bbKindAudio
	binary.LittleEndian.PutUint32(frame[4:], 7)
	frame[bbHeaderLen] = 0x55
	if err := ws.Write(ctx, websocket.MessageBinary, frame); err != nil {
		t.Fatal(err)
	}
	typ, back, err := ws.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || string(back) != string(frame) {
		t.Fatalf("echo: %v len %d %v", typ, len(back), err)
	}
}

func TestBBBridgeFrames(t *testing.T) {
	gw, ws, ctx := dialGateway(t)
	br := &bbBridge{g: gw}
	pcm := make([]float32, 960)
	pcm[0] = 0.5
	for i := 0; i < 2; i++ {
		if err := br.WritePCM(pcm); err != nil {
			t.Fatal(err)
		}
	}
	for want := uint32(1); want <= 2; want++ {
		typ, data, err := ws.Read(ctx)
		if err != nil || typ != websocket.MessageBinary {
			t.Fatalf("frame: %v %v", typ, err)
		}
		if len(data) != bbHeaderLen+1920 || data[0] != bbKindAudio {
			t.Fatalf("bad frame: len %d kind %d", len(data), data[0])
		}
		if seq := binary.LittleEndian.Uint32(data[4:]); seq != want {
			t.Fatalf("seq %d, want %d", seq, want)
		}
		if s := int16(binary.LittleEndian.Uint16(data[bbHeaderLen:])); s < 16000 || s > 16400 {
			t.Fatalf("first sample %d, want ~16383", s)
		}
	}
}
