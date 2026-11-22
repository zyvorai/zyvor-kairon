// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package migration

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"

	"github.com/zyvorai/kairon/internal/tlsreload"
)

// NewCertWatcher validates that caPath/certPath/keyPath are all provided
// together (fail-closed: this is still checked eagerly at startup, same as
// before this package existed) and loads certPath/keyPath into a
// tlsreload.Watcher. Callers should also run watcher.Run(ctx, interval) in
// a goroutine so a renewed leaf certificate takes effect without
// restarting the migration listener/client -- see internal/tlsreload's
// package doc for what is and isn't reloaded (the CA bundle is not).
func NewCertWatcher(log *slog.Logger, caPath, certPath, keyPath string) (*tlsreload.Watcher, error) {
	for name, value := range map[string]string{"CA": caPath, "certificate": certPath, "key": keyPath} {
		if value == "" {
			return nil, fmt.Errorf("migration TLS %s path is required", name)
		}
	}
	return tlsreload.New(log, certPath, keyPath)
}

func ServerTLSConfig(caPath string, watcher *tlsreload.Watcher) (*tls.Config, error) {
	pool, err := loadCAPool(caPath)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		GetCertificate: watcher.GetCertificate,
		ClientCAs:      pool,
		ClientAuth:     tls.RequireAndVerifyClientCert,
	}, nil
}

func ClientTLSConfig(caPath string, watcher *tlsreload.Watcher, serverName string) (*tls.Config, error) {
	pool, err := loadCAPool(caPath)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:           tls.VersionTLS13,
		GetClientCertificate: watcher.GetClientCertificate,
		RootCAs:              pool,
		ServerName:           serverName,
	}, nil
}

func loadCAPool(caPath string) (*x509.CertPool, error) {
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read migration CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("migration CA %s contains no certificates", caPath)
	}
	return pool, nil
}
