package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"wacalls/internal/voip/signaling"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dbPath := flag.String("db", "wacalls.db", "SQLite session database path")
	staticDir := flag.String("static", "client/dist", "static client directory (optional)")
	debug := flag.Bool("debug", false, "verbose logging")
	maxCalls := flag.Int("max-calls-per-session", 8, "max concurrent calls per session (0 = unlimited)")
	bbAddr := flag.String("bb-addr", "", "Berry Bridge gateway listen address (empty = off), see bbgateway.go")
	bbTokenFile := flag.String("bb-token-file", "", "file holding the Berry Bridge gateway token")
	bbSession := flag.String("bb-session", "", "session the Berry Bridge phone uses (empty = first paired)")
	sipAddr := flag.String("sip-addr", "", "SIP gateway TCP listen address for the phone's native call engine (empty = off), see sipgateway.go")
	sipDomain := flag.String("sip-domain", "wacalls.local", "SIP domain")
	sipMediaIP := flag.String("sip-media-ip", "", "IP for RTP (empty = the phone connection's local address)")
	sipRTPPorts := flag.String("sip-rtp-ports", "40000-40099", "RTP port range")
	sipAllow := flag.String("sip-allow", "127.0.0.0/8,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16", "networks allowed to connect to the SIP gateway")
	sipOpus := flag.Bool("sip-opus", true, "SIP gateway: offer and accept Opus (the phone engine's OPUS-WB) before PCMU")
	sipDelayed := flag.Bool("sip-delayed-offer", true, "SIP gateway, with -sip-opus: ring the phone without SDP so its engine offers (and picks Opus); refused: ring again with our offer")
	waCallCapture := flag.String("wa-call-capture", "", "directory for a full capture of each WhatsApp call (media pcap, stanzas, keys; see callcapture.go), for cmd/videodiag (empty = off)")
	waVideoDump := flag.String("wa-video-dump", "", "directory where each call's received WhatsApp video is written as Annex-B H.264 (empty = off)")
	waVideo := flag.Bool("wa-video", false, "1:1 video stanzas of the clients that render video (h.264/H264, see signaling.VideoSignaling); off = legacy shapes")
	flag.Parse()
	signaling.VideoSignaling = *waVideo
	videoDumpDir = *waVideoDump
	captureDir = *waCallCapture

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := newServer(ctx, *dbPath, *staticDir, *maxCalls, log)
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	defer srv.sessions.disconnectAll()

	refreshWAVersion(ctx, log)
	if err := srv.sessions.Restore(ctx); err != nil {
		log.Error("session restore failed", "err", err)
		os.Exit(1)
	}

	httpSrv := &http.Server{Addr: *addr, Handler: srv.routes()}
	go func() {
		log.Info("HTTP server listening", "addr", *addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server error", "err", err)
		}
	}()

	var sipGW *sipGateway
	if *sipAddr != "" {
		var lo, hi int
		if _, err := fmt.Sscanf(*sipRTPPorts, "%d-%d", &lo, &hi); err != nil || lo <= 0 || hi < lo {
			log.Error("bad -sip-rtp-ports", "value", *sipRTPPorts)
			os.Exit(1)
		}
		allow, err := parseCIDRs(*sipAllow)
		if err != nil {
			log.Error("bad -sip-allow", "err", err)
			os.Exit(1)
		}
		be := &sessionBackend{sessions: srv.sessions, broker: srv.broker, sessionID: *bbSession}
		sipGW = newSIPGateway(be, srv.broker, sipConfig{Domain: *sipDomain, MediaIP: *sipMediaIP,
			RTPLo: lo, RTPHi: hi, Allow: allow,
			StateFile: filepath.Join(filepath.Dir(*dbPath), "sip_tunnel.json"), Opus: *sipOpus, DelayedOffer: *sipDelayed}, log)
		sipGW.watch(ctx) // incoming calls ring the phone over the tunnel even without the LAN listener
		go func() {
			// At boot the LAN address may not be up yet (pm2 starts after
			// network.target): wait for it instead of exiting, or pm2 gives
			// up after a burst of quick restarts and nothing runs.
			var ln net.Listener
			for warned := false; ; {
				var err error
				if ln, err = net.Listen("tcp", *sipAddr); err == nil {
					break
				}
				if !warned {
					log.Warn("SIP gateway: address not available yet, retrying every 2 s", "addr", *sipAddr, "err", err)
					warned = true
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
			}
			log.Info("SIP gateway listening", "addr", *sipAddr)
			if err := sipGW.serve(ctx, ln); err != nil {
				log.Error("SIP gateway error", "err", err)
			}
		}()
	}

	var bbSrv *http.Server
	if *bbAddr != "" {
		token, err := os.ReadFile(*bbTokenFile)
		if err != nil || len(strings.TrimSpace(string(token))) < 16 {
			log.Error("Berry Bridge gateway needs -bb-token-file with a token of 16+ characters", "err", err)
			os.Exit(1)
		}
		bbDumpFlag = filepath.Join(filepath.Dir(*bbTokenFile), "echo_dump_next")
		gw := newBBGateway(srv.sessions, srv.broker, string(token), *bbSession, log)
		if sipGW != nil {
			gw.sipPhone = sipGW.registered
			gw.sipGW = sipGW
		}
		bbSrv = &http.Server{Addr: *bbAddr, Handler: gw.routes()}
		go func() {
			log.Info("Berry Bridge gateway listening", "addr", *bbAddr)
			if err := bbSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("Berry Bridge gateway error", "err", err)
			}
		}()
	}

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	if bbSrv != nil {
		_ = bbSrv.Shutdown(shutdownCtx)
	}
}
