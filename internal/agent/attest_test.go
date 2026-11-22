// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

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

	"github.com/zyvorai/kairon/internal/agentplane"
	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func writeSys(t *testing.T, root, rel, v string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(v+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetectConfidential(t *testing.T) {
	root := t.TempDir()
	if k := DetectConfidential(root); k != "" {
		t.Fatal(k)
	}
	writeSys(t, root, "module/kvm_intel/parameters/tdx", "N")
	if k := DetectConfidential(root); k != "" {
		t.Fatal(k)
	}
	writeSys(t, root, "module/kvm_intel/parameters/tdx", "Y")
	if k := DetectConfidential(root); k != "tdx" {
		t.Fatal(k)
	}
	writeSys(t, root, "module/kvm_amd/parameters/sev_snp", "Y")
	if k := DetectConfidential(root); k != "sev-snp" {
		t.Fatal(k)
	}
}

type attestKube struct {
	mu        sync.Mutex
	labels    map[string]string
	nodePatch []string
	machPatch []string
}

func (k *attestKube) server(t *testing.T) *kube.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		defer k.mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes/n1":
			_ = json.NewEncoder(w).Encode(model.Node{Metadata: model.ObjectMeta{Name: "n1", Labels: k.labels}})
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/nodes/n1":
			k.nodePatch = append(k.nodePatch, string(b))
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/machines/m1"):
			k.machPatch = append(k.machPatch, string(b))
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	kc, _ := kube.New(srv.URL, "", "", false)
	return kc
}

func TestPublishConfidentialCapability(t *testing.T) {
	root := t.TempDir()
	writeSys(t, root, "module/kvm_amd/parameters/sev_snp", "1")
	k := &attestKube{}
	a := &Agent{Kube: k.server(t), NodeName: "n1", SysRoot: root, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a.publishConfidentialCapability(context.Background())
	a.publishConfidentialCapability(context.Background())
	if len(k.nodePatch) != 1 || !strings.Contains(k.nodePatch[0], `"kairon.zyvor.dev/confidential-capable":"sev-snp"`) {
		t.Fatalf("patches = %q", k.nodePatch)
	}

	k.labels = map[string]string{agentplane.LabelConfidentialCapable: "sev-snp"}
	writeSys(t, root, "module/kvm_amd/parameters/sev_snp", "0")
	a.attest.capSeen = false
	a.publishConfidentialCapability(context.Background())
	if len(k.nodePatch) != 2 || !strings.Contains(k.nodePatch[1], `"kairon.zyvor.dev/confidential-capable":null`) {
		t.Fatalf("losing the capability must remove the label: %q", k.nodePatch)
	}
}

func TestServeAttestationPutsGuestReportOnMachine(t *testing.T) {
	var gotCmd string
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Command string `json:"command"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotCmd = body.Command
		_, _ = io.WriteString(w, `{"result":"ok","exit_code":0,"stdout":"AAEC\nAw==\n"}`)
	}))
	defer fs.Close()
	k := &attestKube{}
	a := &Agent{Kube: k.server(t), Flux: fluxvm.New(fs.URL, ""), NodeName: "n1", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "m1", Namespace: "prod", UID: "uid-1", Annotations: map[string]string{
			agentplane.AnnConfidential: "sev-snp", agentplane.AnnAttestationNonce: "n1",
		}},
		Spec:   model.MachineSpec{NodeName: "n1", GuestAgent: model.GuestAgentSpec{Console: true}},
		Status: model.MachineStatus{Phase: "Running", RuntimeID: "vm-1"},
	}
	a.serveAttestation(context.Background(), m)
	a.serveAttestation(context.Background(), m)
	if !strings.HasPrefix(gotCmd, "attest -extended -inform hex -in ") || len(strings.Fields(gotCmd)[5]) != 128 {
		t.Fatalf("guest command = %q", gotCmd)
	}
	if len(k.machPatch) != 1 || !strings.Contains(k.machPatch[0], `"kairon.zyvor.dev/attestation-report":"AAECAw=="`) {
		t.Fatalf("patches = %q (second call inside the retry window must not re-fetch)", k.machPatch)
	}

	m.Metadata.Annotations[agentplane.AnnAttestationReport] = "AAECAw=="
	a.attest.tried = nil
	a.serveAttestation(context.Background(), m)
	if len(k.machPatch) != 1 {
		t.Fatal("a pending report must not be replaced")
	}
	if got := attestGuestCommand("snpguest report --data {data} --kind {kind}", "tdx", "ab"); got != "snpguest report --data ab --kind tdx" {
		t.Fatal(got)
	}
}
