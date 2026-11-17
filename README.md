<div align="center">

# 📞 WaCalls (Go)

**Native WhatsApp voice calls in pure Go, straight from the browser.**
Built for native VoIP media, multi-account (multi-session) operation, and a modern browser client.

[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![React](https://img.shields.io/badge/React-19-61DAFB?logo=react&logoColor=black)](https://react.dev)
[![whatsmeow](https://img.shields.io/badge/whatsmeow-VoIP-25D366?logo=whatsapp&logoColor=white)](https://github.com/tulir/whatsmeow)
[![pion](https://img.shields.io/badge/pion-WebRTC-FF6B6B)](https://github.com/pion/webrtc)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](#license)

[Overview](#overview) · [Architecture](#architecture) · [Quick Start](#quick-start) · [API](#api) · [Security](#security)

</div>

---

> **Fork note:** this fork adds the [Berry Bridge gateway](#berry-bridge-gateway-blackberry-10) for BlackBerry 10 phones, including the native Phone app's SIP/RTP relay. Everything else is upstream WaCalls. This fork's `cmd/server` links the gateway's cgo code, so building it needs the Opus and WebRTC AEC libraries (see [Requirements](#requirements)); the pure-Go/no-cgo notes below describe the upstream browser path.

## Overview

WaCalls pairs one or more WhatsApp accounts via **QR code** and lets you **place and
receive 1:1 voice calls** from any browser on the LAN. The browser microphone is sent
as **raw 16 kHz PCM over a WebRTC data channel** to the Go server, which encodes it with
Meta's **MLow** codec and injects the media into WhatsApp's **SRTP relay** mesh — and the
reverse path brings the peer's audio back to the browser.

The upstream VoIP stack runs **natively in pure Go**: the MLow voice codec, **RTP/SRTP**
packetization, **STUN**, the **WebRTC/SCTP relay** transport and the `<call>` signaling,
integrated with [**whatsmeow**](https://github.com/tulir/whatsmeow) and served to a
**React 19** client. The MLow codec is a vendored pure-Go package, so upstream's plain
`go build` produces a self-contained binary with live audio. This fork's gateway adds
cgo for Opus and WebRTC AEC3 (see [Requirements](#requirements)).

Multiple WhatsApp accounts can be paired and operated side by side, each with its own
pairing QR, connection status, and history. A single account can also run **several
concurrent 1:1 calls** at once — one per browser operator — routed independently by call ID.

> **Status:** stable. Outgoing and incoming 1:1 calls reach `ACTIVE` with bidirectional
> audio, and a single account can hold several of them concurrently. Sessions persist in
> `wacalls.db` (pure-Go SQLite).

---

## Architecture

```
┌──────────────────────────────────────────────────────────────────────────┐
│                          BROWSER (React client)                            │
│   mic + speaker  ·  WebRTC data channel (16 kHz PCM)  ·  HTTP + SSE         │
└───────────────────────────────┬──────────────────────────────────────────┘
                                 │  POST /api/sessions/{sid}/calls/{id}/webrtc  (SDP)
                                 │  GET  /api/events                            (SSE)
                                 ▼
┌──────────────────────────── GO SERVER (cmd/server) ────────────────────────┐
│  SessionManager   registry of accounts (client + CallManager + bridge)     │
│  Broker           SSE hub (sessions, auth, call lifecycle fan-out)          │
│  Bridge           pion WebRTC bridge (16 kHz PCM data channel ⇄ call core)  │
│                                                                            │
│  internal/wa      VoipSocket adapter over whatsmeow                        │
│  internal/voip    call · signaling · media · transport · core · wanode     │
└───────────────┬──────────────────────────────────────┬────────────────────┘
                │ <call> signaling (Signal/USync)       │ SRTP media
                ▼                                        ▼
        ┌───────────────┐                    ┌──────────────────────┐
        │  WhatsApp WS  │                    │   WhatsApp relay      │
        │  (whatsmeow)  │                    │  (SRTP over SCTP/DC)  │
        └───────────────┘                    └──────────────────────┘
```

### Layout

| Path | Responsibility |
|---|---|
| `cmd/server` | HTTP/SSE broker, session manager + store, WebRTC bridge, process lifecycle |
| `internal/wa` | `VoipSocket` — sends/receives `<call>` stanzas via whatsmeow |
| `internal/voip/core` | Domain types, constants, the `VoipSocket` interface |
| `internal/voip/wanode` | Shared WhatsApp-node and JID helpers |
| `internal/voip/media` | MLow codec (vendored pure-Go `mlow/`), RTP, SRTP, SSRC, PCM helpers, key derivation |
| `internal/voip/transport` | SCTP relay, STUN, subscription encoding |
| `internal/voip/signaling` | `<call>` stanza build/parse, call-key crypto, relay-ack parsing |
| `internal/voip/call` | `CallManager` — orchestrates a single call end to end |
| `client/` | React 19 + Vite + Tailwind v4 + shadcn/ui (dialer, call cards, sessions, history) |

---

## How a call flows

The core is `internal/voip/call.CallManager`, which drives a call end to end. Outgoing
call sequence:

```
1. POST .../calls            → CallManager.StartCall(peerJid)
                               generates a callID, builds the <call> offer, sends it

2. Browser opens WebRTC      → POST .../calls/{id}/webrtc (SDP offer)
                               the bridge answers with an SDP answer (pion)

3. Peer accepts              → events.CallAccept → HandleCallAccept
                               server receives <relay> + hop-by-hop keys

4. Relay transport           → STUN binding/allocate on WhatsApp relays
                               ICE + DTLS + SCTP DataChannel connect (pion)

5. SRTP media flowing        → state goes ACTIVE
   ├── uplink   (you → peer): browser 16 kHz PCM (data channel) → MLow encode → SRTP → relay
   └── downlink (peer → you): relay → SRTP → MLow decode → 16 kHz PCM (data channel) → browser

6. Teardown                  → DELETE .../calls/{id} or events.CallTerminate
                               CallManager.EndCall + bridge cleanup
```

Each protocol step (hop-by-hop SRTP key derivation, RTP packetization at `PT=120`/16 kHz,
STUN relay registration, relay-ack and `<call>` stanza parsing) is implemented and covered
by tests in `internal/voip` (`go test ./...`).

---

## Requirements

- **Go 1.26+**
- **Node 22+** and **npm** (only to build/run the React client)
- **cgo** with `pkg-config opus` and `pkg-config webrtc-audio-processing-1` — for the
  Berry Bridge gateway (Opus audio and WebRTC AEC3). The upstream browser path and the
  MLow codec are pure Go, but this fork's `cmd/server` compiles the gateway's cgo files
  too, so `CGO_ENABLED=0` does not link.

---

## Quick Start

```bash
# clone and enter the project
git clone <repo-url> wacalls-go
cd wacalls-go

# Go dependencies
go mod download

# React client dependencies
cd client && npm install && cd ..
```

### Run

```bash
go run ./cmd/server -addr :8080          # add -debug for verbose logs
```

Live audio works out of the box on the browser path — the MLow codec is pure Go.
The fork's gateway additionally links Opus and AEC3 through cgo, so install the
libraries from [Requirements](#requirements) before building `cmd/server`.

Open `http://localhost:8080`, click **New session**, and scan the QR shown in the browser
(it is also printed in the terminal) with **WhatsApp → Linked devices**. Add more accounts
the same way and switch between them in the sidebar.

### React client in dev mode

```bash
cd client
npm run dev      # Vite on :5173, proxies /api → http://localhost:8080
```

For production, build the static client and serve it from the Go server:

```bash
cd client && npm run build && cd ..
go run ./cmd/server -static client/dist -addr :8080
```

### Server flags

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `:8080` | HTTP listen address |
| `-db` | `wacalls.db` | SQLite session database path |
| `-static` | `client/dist` | Static client directory (optional) |
| `-debug` | `false` | Verbose logging (includes whatsmeow's internal log) |
| `-max-calls-per-session` | `8` | Max concurrent calls per session (`0` = unlimited) |

---

## API

All routes are session-scoped. Events stream over a single SSE channel, tagged with the
originating `sessionId`.

| Method | Route | Purpose |
|---|---|---|
| `GET` | `/api/sessions` | List accounts (id, name, jid, status, paired) |
| `POST` | `/api/sessions` | Create an account and begin QR pairing |
| `DELETE` | `/api/sessions/{sid}` | Log out and remove an account |
| `POST` | `/api/sessions/{sid}/logout` | Disconnect an account (keep it for re-pairing) |
| `POST` | `/api/sessions/{sid}/pair` | Re-pair an account (emit a fresh QR) |
| `POST` | `/api/sessions/{sid}/calls` | Start an outgoing call (`{ phone, duration_ms?, record? }`) |
| `POST` | `/api/sessions/{sid}/calls/{id}/webrtc` | Exchange the browser WebRTC SDP |
| `POST` | `/api/sessions/{sid}/calls/{id}/accept` | Accept an incoming call |
| `POST` | `/api/sessions/{sid}/calls/{id}/reject` | Reject an incoming call |
| `DELETE` | `/api/sessions/{sid}/calls/{id}` | End an active call |
| `GET` | `/api/sessions/{sid}/history` | Recent call history (up to 50 records) |
| `GET` | `/api/events` | Server-sent events (sessions, auth, call lifecycle) |

---

## Tests

```bash
go test ./...                 # media stack: SRTP, STUN, RTP, relay-ack, codec, state
cd client && npm run build    # client type-check + production build
```

---

## Security

The API has **no authentication** — anyone with HTTP access can create accounts, place
calls, and read history. **Run it only on a trusted LAN.** `wacalls.db` holds WhatsApp
session credentials (secrets): **do not commit it** and keep it protected.

---

## Berry Bridge gateway (BlackBerry 10)

> This fork adds a second way to take part in calls besides the browser: a
> **BlackBerry 10 phone** running [Berry Bridge](https://github.com/damianolampisti00/BerryBridge)
> becomes the audio and UI terminal of the calls of one WhatsApp session.
> Upstream: [JotaDev66/WaCalls](https://github.com/JotaDev66/WaCalls).

The call core is nearly untouched: `CallManager` already speaks plain 16 kHz
PCM (`FeedCapturedPCM` up, `OnPeerAudio` down) and only gained an
`OnSentPCM` hook (the audio actually sent, for echo cancelling); the browser's WebRTC `Bridge`
and the new `bbBridge` both implement a small `mediaBridge` interface
(`cmd/server/callregistry.go`), and the gateway (`cmd/server/bbgateway.go`)
turns the broker's call events into a simple protocol for the phone:

- **one WebSocket**, `GET /ws` on its **own listener**, authenticated with
  `Authorization: Bearer <token>`;
- **JSON text frames** for control: `call.start` / `accept` / `reject` /
  `hangup` / `mute` from the phone, `hello`, `call.incoming`, `call.state`,
  `call.ended` from the server (caller numbers resolved from LIDs);
- **binary frames** for audio both ways: `kind, flags, 0, 0, seq (u32 LE),
  sender ms (u32 LE)`, then signed 16-bit LE PCM at 16 kHz mono (kind `0x01`)
  or, once the phone asked for `audio.codec: opus`, one 20 ms Opus packet
  (kind `0x03`, ~24 kbit/s instead of 256: this part needs libopus, through
  cgo — `pkg-config opus`);
- a `test.echo` mode that returns the phone's audio as is (transport tests);
- **echo cancelling** with WebRTC's AEC3 (`bbaec.cpp`, through cgo —
  `pkg-config webrtc-audio-processing-1`), both ways:
  - the phone's own earpiece/speaker in its mic (`bbupecho.go`): with flag
    `0x01` the phone puts its playback position (downlink samples played,
    mod 65536) in bytes 2-3 of each mic frame, so the reference is exactly
    what it played, pauses and jitter-buffer refills included. AEC3 needs
    a few seconds before it cancels anything, so a residual echo suppressor
    follows: a mic frame more than 14 dB below what the phone was playing
    is taken for echo and turned down 14 dB (the user's own voice is close
    to the playback level and passes);
  - the user's voice coming back from a hands-free peer (`bbecho.go`), the
    round trip found by an envelope search;
- boost-only AGCs (`bbagc.go`) on the peer's voice and, after the echo
  canceller, on the phone's mic (capped at +12 dB);
- a **pacer** for the mic (`bbuppacer.go`): mobile links deliver it in
  bursts after stalls of up to a second; a 100 ms buffer feeds the call one
  20 ms frame per tick, silence while it refills, and cuts a backlog over
  300 ms back, so the peer hears neither jumps nor crackles;
- `call levels` in the log every 10 s (levels, echo canceller and suppressor, mic timing).

Two fixes outside the gateway: a session whose first connect fails at boot
(network not up yet) is retried with a backoff, since whatsmeow only
reconnects after a successful connection; and call offers older than 45 s,
which WhatsApp delivers on connect after an outage, are ignored instead of
ringing.

Run it next to the normal API:

```bash
head -c 32 /dev/urandom | base64 > bb_token        # 16+ characters
./wacalls -addr 127.0.0.1:8097 -db wacalls.db           -bb-addr 127.0.0.1:8098 -bb-token-file bb_token [-bb-session <id>]
```

Expose **only** `-bb-addr` to the internet, over HTTPS (a reverse proxy or a
Cloudflare tunnel; for a long-lived audio stream prefer HTTP/2 transport over
QUIC). The browser API on `-addr` still has no authentication: keep it on
localhost. Without `-bb-addr` the server behaves exactly like upstream.

### Native Phone app (SIP/RTP relay)

Berry Bridge can also hand calls to the phone's own call engine (EBMService, the
BBM Video engine used as a SIP terminal). The app relays the engine's SIP and RTP
through the same authenticated WebSocket (`bbsiptunnel.go`), so WhatsApp calls
ring and talk in the native Phone app, incoming and outgoing, wherever the tunnel
works — not only on the home Wi-Fi. Outgoing calls are started by the root
`dialhelper` next to EBMService (port 5071); incoming ones ring the Phone app with
a SIP INVITE, and the first client to answer (Phone app, in-app call screen or
browser) claims the call and stops the others.

- **Audio**: `-sip-opus` (default on) offers the engine's OPUS-WB first, with a
  delayed offer (`-sip-delayed-offer`, default on) so the engine offers and picks
  Opus; PCMU 20 ms is the fallback. On video calls WhatsApp sends audio in 120 ms
  two-frame MLow blocks: the decoder splits them and the SIP pre-buffer starts at
  140 ms so block boundaries are not underruns.
- **H.264 video both ways**: the phone camera's Annex-B access units go to
  WhatsApp on their own video SSRC and SRTP send context (`h264packetizer.go`),
  separate from the audio context so both rollover counters stay valid. In the
  other direction the gateway depacketizes the engine's H.264, follows its CVO
  rotation and hands whole access units to the call core; the peer's picture is
  turned upright for the Phone app.
- **Fast answer**: while the phone rings with our non-ICE offer, AnyFirewall probes
  and STUN checks are sent every 50 ms to the engine's media ports, so its
  remote-data flag is set as soon as Answer is tapped, even across the LTE round
  trip.

`-sip-addr` enables the SIP gateway; without it `sip.open` is refused, even when
only the tunnel is used. `-sip-allow` limits LAN registrations to the phone's
networks. `-wa-video` enables WhatsApp's 1:1 H.264 video signaling: the camera
button in Berry Bridge starts a video call that plays in the native Phone app,
with the phone's camera sent back to WhatsApp. `-wa-video-dump <dir>` writes each
call's received WhatsApp video as Annex-B H.264 for analysis.

```bash
./wacalls -addr 127.0.0.1:8097 -db wacalls.db \
          -bb-addr 127.0.0.1:8098 -bb-token-file bb_token \
          -sip-addr 127.0.0.1:5070 -wa-video
```

## Contributors

This project builds on the work of:

<div align="center">

<a href="https://github.com/jotadev66"><img src="https://github.com/jotadev66.png" width="72" height="72" style="border-radius:50%" alt="jotadev66"/></a>
<a href="https://github.com/jobasfernandes"><img src="https://github.com/jobasfernandes.png" width="72" height="72" style="border-radius:50%" alt="jobasfernandes"/></a>
<a href="https://github.com/edgardmessias"><img src="https://github.com/edgardmessias.png" width="72" height="72" style="border-radius:50%" alt="edgardmessias"/></a>
<a href="https://github.com/w3nder"><img src="https://github.com/w3nder.png" width="72" height="72" style="border-radius:50%" alt="w3nder"/></a>

[**@jotadev66**](https://github.com/jotadev66) · [**@jobasfernandes**](https://github.com/jobasfernandes) · [**@edgardmessias**](https://github.com/edgardmessias) · [**@w3nder**](https://github.com/w3nder)

</div>

---

## Acknowledgements

- [**whatsmeow**](https://github.com/tulir/whatsmeow) — Go WhatsApp Web protocol library
- [**pion/webrtc**](https://github.com/pion/webrtc) — pure-Go WebRTC stack (ICE + DTLS + SCTP)
- [**whatsapp-rust**](https://github.com/oxidezap/whatsapp-rust) — reference MLow codec implementation (ported to the vendored pure-Go `internal/voip/media/mlow`)
- [**zapo**](https://github.com/w3nder/zapo) — VoIP media-stack reference

---

## License

[MIT](./LICENSE)
