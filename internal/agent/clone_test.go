// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func TestCloneBootDisk(t *testing.T) {
	srcDisk := filepath.Join(t.TempDir(), "src.qcow2")
	if err := os.WriteFile(srcDisk, []byte("source-disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(t.TempDir(), "qemu-img")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nfor a; do src=$out; out=$a; done\ncp \"$src\" \"$out\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := qemuImg
	qemuImg = fake
	t.Cleanup(func() { qemuImg = orig })

	var mu sync.Mutex
	srcPhase := "Running"
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/kairon.zyvor.dev/v1/namespaces/dev/machines/base" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(model.Machine{
			Metadata: model.ObjectMeta{Name: "base", Namespace: "dev"},
			Spec:     model.MachineSpec{NodeName: "n1"},
			Status:   model.MachineStatus{Phase: srcPhase, RuntimeID: "vm-base"},
		})
	}))
	t.Cleanup(ks.Close)
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(fluxvm.Record{IDValue: "vm-base", Status: "halted", Disk: srcDisk})
	}))
	t.Cleanup(fs.Close)
	kc, _ := kube.New(ks.URL, "", "", false)
	kc.HTTP = ks.Client()
	a := &Agent{NodeName: "n1", Kube: kc, Flux: fluxvm.New(fs.URL, ""), ImageCacheDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	m := model.Machine{Metadata: model.ObjectMeta{Name: "copy", Namespace: "dev", Annotations: map[string]string{model.AnnotationCloneFrom: "base"}}}
	ctx := context.Background()
	if _, _, err := a.cloneBootDisk(ctx, m); err == nil || !strings.Contains(err.Error(), "halt it first") {
		t.Fatalf("running source: %v", err)
	}
	mu.Lock()
	srcPhase = "Halted"
	mu.Unlock()
	var path string
	var ready bool
	var err error
	for i := 0; i < 100 && !ready; i++ {
		if path, ready, err = a.cloneBootDisk(ctx, m); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready || path != filepath.Join(a.ImageCacheDir, "clones", "dev_copy.qcow2") {
		t.Fatalf("ready=%v path=%q", ready, path)
	}
	if b, _ := os.ReadFile(path); string(b) != "source-disk" {
		t.Fatalf("copy = %q", b)
	}
	if p2, ready2, _ := a.cloneBootDisk(ctx, m); !ready2 || p2 != path {
		t.Fatal("finished copy must be reused")
	}

	withVol := m
	withVol.Spec.Volumes = []model.MachineVolume{{Name: "root", ClaimName: "c"}}
	if _, _, err := a.cloneBootDisk(ctx, withVol); err == nil {
		t.Fatal("clone with volumes accepted")
	}

	a.sweepClones(map[string]model.Machine{"dev/copy": m})
	if _, err := os.Stat(path); err != nil {
		t.Fatal("sweep removed a live clone")
	}
	a.sweepClones(map[string]model.Machine{})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("sweep kept an orphaned clone")
	}
}
