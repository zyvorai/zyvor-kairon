// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// getFakeServer is an in-memory fake of the list endpoints `kaironctl get
// protectionplans|macnetworks` calls. It never touches a real cluster.
func getFakeServer(t *testing.T, plans []model.MachineProtectionPlan, nets []model.MacNetwork) *kube.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machineprotectionplans":
			_ = json.NewEncoder(w).Encode(model.MachineProtectionPlanList{Items: plans})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/macnetworks":
			_ = json.NewEncoder(w).Encode(model.MacNetworkList{Items: nets})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &kube.Client{BaseURL: srv.URL, HTTP: srv.Client()}
}

func TestCmdGetProtectionPlans(t *testing.T) {
	plan := func(name string, labels map[string]string) model.MachineProtectionPlan {
		return model.MachineProtectionPlan{
			Metadata: model.ObjectMeta{Name: name, Namespace: "prod", Labels: labels},
			Spec:     model.MachineProtectionPlanSpec{Role: "Primary", Machines: []model.ProtectedMachine{{Name: "db"}, {Name: "web"}}},
			Status:   model.MachineProtectionPlanStatus{Phase: "Protecting", Active: true, Epoch: 3},
		}
	}
	kc := getFakeServer(t, []model.MachineProtectionPlan{
		plan("shop", map[string]string{"tier": "gold"}),
		plan("blog", nil),
	}, nil)

	out := captureStdout(t, func() {
		cmdGet(context.Background(), kc, []string{"protectionplans", "-n", "prod"})
	})
	for _, want := range []string{"NAME", "ROLE", "shop", "blog", "Primary", "Protecting"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	out = captureStdout(t, func() {
		cmdGet(context.Background(), kc, []string{"kmpp", "-n", "prod", "--selector", "tier=gold"})
	})
	if !strings.Contains(out, "shop") || strings.Contains(out, "blog") {
		t.Errorf("--selector tier=gold must list only shop:\n%s", out)
	}
}

func TestCmdGetMacNetworksFiltersNamespaceAndSelector(t *testing.T) {
	kc := getFakeServer(t, nil, []model.MacNetwork{
		{Metadata: model.ObjectMeta{Name: "shard-net", Namespace: "ai", Labels: map[string]string{"env": "lab"}},
			Spec:   model.MacNetworkSpec{Fabric: "rack1"},
			Status: model.MacNetworkStatus{Phase: "Ready", Subnet: "10.89.192.0/24", Members: []model.MacNetworkMember{{Node: "mac-1"}, {Node: "mac-2"}}}},
		{Metadata: model.ObjectMeta{Name: "other-net", Namespace: "ai"}},
		{Metadata: model.ObjectMeta{Name: "elsewhere", Namespace: "default"}},
	})

	out := captureStdout(t, func() {
		cmdGet(context.Background(), kc, []string{"macnetworks", "-n", "ai"})
	})
	if !strings.Contains(out, "shard-net") || !strings.Contains(out, "other-net") || !strings.Contains(out, "mac-1,mac-2") {
		t.Errorf("expected both ai networks with members:\n%s", out)
	}
	if strings.Contains(out, "elsewhere") {
		t.Errorf("a network in another namespace must not be listed:\n%s", out)
	}

	out = captureStdout(t, func() {
		cmdGet(context.Background(), kc, []string{"kmn", "-n", "ai", "--selector", "env=lab"})
	})
	if !strings.Contains(out, "shard-net") || strings.Contains(out, "other-net") {
		t.Errorf("--selector env=lab must list only shard-net:\n%s", out)
	}
}
