// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func TestBuildCiliumNetworkPolicySpecFromVmPolicy(t *testing.T) {
	p := model.MachineNetworkPolicy{
		Metadata: model.ObjectMeta{Name: "web-edge", Namespace: "default"},
		Spec: model.MachineNetworkPolicySpec{
			Selector: map[string]string{"app": "web"},
			Policy: model.VmNetworkPolicy{
				DefaultAllow: false,
				AllowCidrs:   []string{"10.0.0.0/8"},
				AllowPorts:   []string{"tcp/443", "udp/53"},
			},
		},
	}
	spec, err := buildCiliumNetworkPolicySpec(p)
	if err != nil {
		t.Fatal(err)
	}
	sel := spec["endpointSelector"].(map[string]any)["matchLabels"].(map[string]string)
	if sel["app"] != "web" {
		t.Fatalf("selector=%v", sel)
	}
	egress := spec["egress"].([]map[string]any)
	if len(egress) != 1 {
		t.Fatalf("egress=%v", egress)
	}
}

func TestBuildCiliumNetworkPolicySpecPrefersCNP(t *testing.T) {
	p := model.MachineNetworkPolicy{
		Spec: model.MachineNetworkPolicySpec{
			CNP: map[string]any{
				"spec": map[string]any{
					"endpointSelector": map[string]any{"matchLabels": map[string]any{"x": "y"}},
				},
			},
			Policy: model.VmNetworkPolicy{DefaultAllow: true},
		},
	}
	spec, err := buildCiliumNetworkPolicySpec(p)
	if err != nil {
		t.Fatal(err)
	}
	sel := spec["endpointSelector"].(map[string]any)["matchLabels"].(map[string]any)
	if sel["x"] != "y" {
		t.Fatalf("spec=%v", spec)
	}
}

func TestReconcileCiliumAttachCreatesExternalWorkload(t *testing.T) {
	var created map[string]any
	var statusPatch model.MachineStatus
	var specPatches []map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/apis/cilium.io/v2/ciliumexternalworkloads/"):
			http.Error(w, "not found", http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == "/apis/cilium.io/v2/ciliumexternalworkloads":
			_ = json.NewDecoder(r.Body).Decode(&created)
			created["metadata"].(map[string]any)["uid"] = "cew-uid-1"
			_ = json.NewEncoder(w).Encode(created)
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/machines/web/status"):
			var body struct {
				Status model.MachineStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			statusPatch = body.Status
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/machines/web"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			specPatches = append(specPatches, body)
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer s.Close()
	kc, err := kube.New(s.URL, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	kc.HTTP = s.Client()
	ctl := &Controller{
		Kube:         kc,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		CiliumAttach: true,
	}
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "web", Namespace: "default", Labels: map[string]string{"app": "web"}},
		Spec: model.MachineSpec{
			Network: model.NetworkSpec{Mode: "tap", NetNS: true, CiliumAttach: true},
		},
	}
	if err := ctl.reconcileMachineCiliumAttach(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if created == nil || created["metadata"].(map[string]any)["name"] != "kairon-default-web" {
		t.Fatalf("created=%v", created)
	}
	if statusPatch.Network == nil || statusPatch.Network.Cilium == nil || statusPatch.Network.Cilium.ExternalWorkloadUID != "cew-uid-1" {
		t.Fatalf("status=%+v", statusPatch.Network)
	}
	foundPodUID := false
	for _, p := range specPatches {
		if spec, ok := p["spec"].(map[string]any); ok {
			if net, ok := spec["network"].(map[string]any); ok && net["podUID"] == "cew-uid-1" {
				foundPodUID = true
			}
		}
	}
	if !foundPodUID {
		t.Fatalf("expected podUID patch, got %v", specPatches)
	}
}

func TestExternalWorkloadNameSanitizes(t *testing.T) {
	got := kube.ExternalWorkloadName("Prod_NS", "Web.App")
	if got != "kairon-prod-ns-web-app" {
		t.Fatalf("got %q", got)
	}
}
