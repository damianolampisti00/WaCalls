package main

import (
	"context"
	"log/slog"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
)

// refreshWAVersion makes whatsmeow announce the WhatsApp Web version that
// web.whatsapp.com serves today. The one compiled into whatsmeow ages: once
// WhatsApp stops accepting it, every connect fails with "Client outdated
// (405)" and no calls come in until whatsmeow is updated. Reports whether the
// version changed.
func refreshWAVersion(ctx context.Context, log *slog.Logger) bool {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	latest, err := whatsmeow.GetLatestVersion(ctx, nil)
	if err != nil {
		log.Warn("could not fetch the current WhatsApp Web version", "err", err, "using", store.GetWAVersion().String())
		return false
	}
	cur := store.GetWAVersion()
	if latest.LessThan(cur) || *latest == cur {
		return false
	}
	store.SetWAVersion(*latest)
	log.Info("WhatsApp Web version updated", "from", cur.String(), "to", latest.String())
	return true
}

// onClientOutdated runs when WhatsApp refuses a connect as outdated. whatsmeow
// does not reconnect after that, so fetch the current version (WhatsApp Web
// may publish it a little after the refusals start) and reconnect every
// paired session that is offline. One loop at a time for all sessions.
func (m *SessionManager) onClientOutdated() {
	if !m.outdatedLoop.CompareAndSwap(false, true) {
		return
	}
	defer m.outdatedLoop.Store(false)
	m.log.Error("WhatsApp refused the client as outdated", "version", store.GetWAVersion().String())
	delay := 10 * time.Second
	for !refreshWAVersion(m.appCtx, m.log) {
		select {
		case <-m.appCtx.Done():
			return
		case <-time.After(delay):
		}
		if delay < 30*time.Minute {
			delay *= 2
		}
	}
	m.mu.RLock()
	var offline []*Session
	for _, s := range m.sessions {
		if s.client.Store.ID != nil && !s.client.IsConnected() {
			offline = append(offline, s)
		}
	}
	m.mu.RUnlock()
	for _, s := range offline {
		go m.retryConnect(s)
	}
}
