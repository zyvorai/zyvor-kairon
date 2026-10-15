// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/migration"
)

// serveStub listens on a real Unix socket, the same transport kairon-node's
// migration.Adapter uses in production.
func serveStub(t *testing.T) *migration.Adapter {
	t.Helper()
	// Unix socket paths are limited to ~104 bytes on macOS; t.TempDir() is
	// longer than that, so use a short directory under /tmp.
	dir, err := os.MkdirTemp("/tmp", "kst")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "a.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: newStub(slog.New(slog.NewTextHandler(io.Discard, nil))).handler()}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return migration.NewAdapter(socket)
}

func testSession() migration.Session {
	return migration.Session{ID: "sess1", Namespace: "prod", Machine: "db", SourceNode: "n1", TargetNode: "n2", RuntimeID: "vm-1"}
}

func TestStubAdapterLiveMigrationOverUnixSocket(t *testing.T) {
	if testing.Short() {
		t.Skip("the stub simulates a ~6s transfer")
	}
	ad := serveStub(t)
	ctx := context.Background()
	sess := testSession()

	caps, err := ad.Capabilities(ctx)
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	if !caps.SupportsCompression("zstd") {
		t.Fatalf("stub should advertise zstd: %+v", caps)
	}

	prep, err := ad.Prepare(ctx, sess)
	if err != nil || !prep.TransferSupported || prep.Endpoint == "" {
		t.Fatalf("prepare: %+v err=%v", prep, err)
	}
	st, err := ad.Start(ctx, migration.SourceRequest{Session: sess, RuntimeID: sess.RuntimeID, Endpoint: prep.Endpoint})
	if err != nil || st.TransferID == "" {
		t.Fatalf("start: %+v err=%v", st, err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for st.Phase != "completed" {
		if time.Now().After(deadline) {
			t.Fatalf("transfer did not complete: %+v", st)
		}
		time.Sleep(250 * time.Millisecond)
		if st, err = ad.Status(ctx, sess, st.TransferID); err != nil {
			t.Fatalf("status: %v", err)
		}
	}
	if st.RAMRemaining != 0 || st.RAMTransferred != st.RAMTotal {
		t.Fatalf("completed transfer must have moved all RAM: %+v", st)
	}
	if err := ad.Commit(ctx, sess); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestStubAdapterAbortBeforeCommit(t *testing.T) {
	ad := serveStub(t)
	ctx := context.Background()
	sess := testSession()
	if _, err := ad.Prepare(ctx, sess); err != nil {
		t.Fatal(err)
	}
	st, err := ad.Start(ctx, migration.SourceRequest{Session: sess, RuntimeID: sess.RuntimeID})
	if err != nil {
		t.Fatal(err)
	}
	if err := ad.AbortSource(ctx, sess, st.TransferID); err != nil {
		t.Fatalf("abort source: %v", err)
	}
	if err := ad.Abort(ctx, sess); err != nil {
		t.Fatalf("abort destination: %v", err)
	}
}
