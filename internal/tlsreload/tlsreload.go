// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package tlsreload hot-reloads a TLS leaf certificate/key pair from disk
// on a stdlib-only polling interval -- no fsnotify, no new dependency,
// consistent with this project's Go-stdlib-only posture. Every TLS hop in
// this project (migration mTLS, the VNC console relay, the admission
// webhook) used to load its certificate exactly once at startup and never
// look at it again; a renewed certificate (cert-manager or any other
// rotation tool) only took effect after a manual restart. A Watcher fixes
// that without restarting the listener or recreating the http.Client: Go's
// own tls.Conn.Handshake calls GetCertificate/GetClientCertificate fresh
// on every new connection, so swapping the certificate this package hands
// back takes effect immediately for new connections.
//
// Deliberately out of scope: reloading a CA bundle used to build
// ClientCAs/RootCAs. That still loads once at startup, same as before this
// package existed -- matching the common rotation case (a short-lived leaf
// certificate renewed while the CA itself stays stable for a long time).
// Rotating the CA itself still needs a redeploy.
package tlsreload

import (
	"context"
	"crypto/tls"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Watcher serves the current, most-recently-loaded certificate/key pair
// for certPath/keyPath, refreshed by Run.
type Watcher struct {
	certPath, keyPath string
	log               *slog.Logger

	mu      sync.RWMutex
	cert    tls.Certificate
	modTime time.Time
}

// New loads certPath/keyPath once, synchronously, so a bad certificate/key
// pair is a startup failure -- the same fail-closed posture every other
// TLS hop in this project already has for its initial load -- not a
// surprise on the first connection or the first background reload tick.
func New(log *slog.Logger, certPath, keyPath string) (*Watcher, error) {
	w := &Watcher{certPath: certPath, keyPath: keyPath, log: log}
	if err := w.reload(); err != nil {
		return nil, err
	}
	return w, nil
}

// GetCertificate is a tls.Config.GetCertificate callback: wire it into a
// server-side tls.Config instead of a static Certificates slice.
func (w *Watcher) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	cert := w.cert
	return &cert, nil
}

// GetClientCertificate is a tls.Config.GetClientCertificate callback: wire
// it into a client-side tls.Config for mTLS instead of a static
// Certificates slice.
func (w *Watcher) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return w.GetCertificate(nil)
}

// Run polls certPath/keyPath's modification time every interval and
// reloads on change, until ctx is canceled. A reload failure (e.g. a
// renewal tool caught mid-write, or a key that doesn't match a
// half-written certificate) is logged and the previous, still-valid
// material keeps serving -- a transient read error never tears down an
// otherwise working listener. Meant to be started in its own goroutine,
// tied to the same ctx as the server/client using GetCertificate/
// GetClientCertificate above.
func (w *Watcher) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			changed, err := w.changed()
			if err != nil {
				w.log.Warn("tls reload: stat failed, keeping current certificate", "cert", w.certPath, "error", err)
				continue
			}
			if !changed {
				continue
			}
			if err := w.reload(); err != nil {
				w.log.Warn("tls reload: reload failed, keeping current certificate", "cert", w.certPath, "error", err)
				continue
			}
			w.log.Info("tls certificate reloaded", "cert", w.certPath)
		}
	}
}

func (w *Watcher) changed() (bool, error) {
	latest, err := latestModTime(w.certPath, w.keyPath)
	if err != nil {
		return false, err
	}
	w.mu.RLock()
	prev := w.modTime
	w.mu.RUnlock()
	return latest.After(prev), nil
}

func (w *Watcher) reload() error {
	cert, err := tls.LoadX509KeyPair(w.certPath, w.keyPath)
	if err != nil {
		return err
	}
	latest, err := latestModTime(w.certPath, w.keyPath)
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.cert = cert
	w.modTime = latest
	w.mu.Unlock()
	return nil
}

func latestModTime(paths ...string) (time.Time, error) {
	var latest time.Time
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return time.Time{}, err
		}
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	return latest, nil
}
