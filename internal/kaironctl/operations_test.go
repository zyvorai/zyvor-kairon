// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/incidentwork"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/rehearsal"
	"github.com/zyvorai/kairon/internal/scheduler"
)

func TestOperationsCommandsAreRegistered(t *testing.T) {
	for _, args := range [][]string{{"changeplan", "preview", "--help"}, {"changeplan", "apply", "--help"}, {"changeplan", "recover", "--help"}, {"recovery", "verify", "--help"}, {"incident", "rehearse", "--help"}, {"placement", "recommend", "--help"}, {"placement", "collect", "--help"}} {
		cmd := NewRootCmd(nil)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(args, err)
		}
	}
}
func TestPlacementCollectUsesRealCounterRouteAndMeasuredWindow(t *testing.T) {
	old := kubeFlags
	kubeFlags.Kubeconfig = ""
	kubeFlags.Context = ""
	defer func() { kubeFlags = old }()
	machines := []model.Machine{{Metadata: model.ObjectMeta{Name: "db", Namespace: "default", UID: "db-uid"}, Spec: model.MachineSpec{NodeName: "a"}, Status: model.MachineStatus{Phase: "Running", RuntimeID: "db-runtime", GuestIP: "10.0.0.2"}}, {Metadata: model.ObjectMeta{Name: "web", Namespace: "default", UID: "web-uid"}, Spec: model.MachineSpec{NodeName: "b"}, Status: model.MachineStatus{Phase: "Running", RuntimeID: "web-runtime", GuestIP: "10.0.0.3"}}}
	kube := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/kairon.zyvor.dev/v1/machines" {
			t.Errorf("wrong kube route %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(model.MachineList{Items: machines})
	}))
	defer kube.Close()
	t.Setenv("KAIRON_KUBE_URL", kube.URL)
	count := uint64(0)
	firstSeen := time.Now().Add(-time.Hour)
	ui := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/machines/default/db/network-flows" {
			_, _ = w.Write([]byte(`{"items":[]}`))
			return
		}
		if r.URL.Path != "/api/v1/machines/default/web/network-flows" {
			t.Errorf("wrong UI route %s", r.URL.Path)
		}
		count += 1000
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"bytes": count, "srcIP": "10.0.0.3", "dstIP": "10.0.0.2", "firstSeen": firstSeen}}})
	}))
	defer ui.Close()
	t.Setenv("KAIRON_UI_URL", ui.URL)
	dir := t.TempDir()
	input := filepath.Join(dir, "rtts.json")
	output := filepath.Join(dir, "telemetry.json")
	template := scheduler.NetworkTelemetry{SameNodeRTTMillis: 0.1, Links: []scheduler.MeasuredLink{{From: "a", To: "b", RTTMillis: 10, SampledAt: time.Now().UTC()}}}
	b, _ := json.Marshal(template)
	if err := os.WriteFile(input, b, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := NewRootCmd(nil)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"placement", "collect", "--rtts", input, "--output", output, "--window", "1s"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var telemetry scheduler.NetworkTelemetry
	if err := readOperationJSON(output, &telemetry); err != nil {
		t.Fatal(err)
	}
	if len(telemetry.Edges) != 1 || telemetry.Edges[0].From != "default/web" || telemetry.Edges[0].BytesPerSecond <= 0 || telemetry.Edges[0].BytesPerSecond > 1100 || telemetry.Edges[0].To != "default/db" {
		t.Fatal(telemetry)
	}
}

func TestIncidentWorkflowAcceptsExistingDeepBundleFormat(t *testing.T) {
	t.Setenv("KAIRON_UI_URL", "")
	var archive bytes.Buffer
	if _, err := writeIncident(context.Background(), incidentKube(t), &archive, "default", "web", &incidentOpts{Deep: true}, "test"); err != nil {
		t.Fatal(err)
	}
	p := rehearsal.Plan{APIVersion: "rehearsal.kairon.zyvor.dev/v1alpha1", Name: "bundle-format", Namespace: "default", Node: "lab", Bridge: "krh-demo", TTLSeconds: 600, TimeoutSeconds: 120, Consistency: "existing", Members: []rehearsal.Member{{Name: "web", SourceSnapshot: "web-point"}}, Baseline: []rehearsal.Step{{Name: "reproduce", Machine: "web", Path: "/usr/bin/python3", TimeoutSeconds: 10, ExpectedExitCode: 1}}, Changes: []rehearsal.Step{{Name: "fix", Machine: "web", Path: "/usr/bin/python3", TimeoutSeconds: 10}}, Verify: []rehearsal.Step{{Name: "check", Machine: "web", Path: "/usr/bin/python3", TimeoutSeconds: 10}}}
	bound, err := incidentwork.Bind(bytes.NewReader(archive.Bytes()), p)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Incident.SourceUID != "u-1" || bound.Incident.Machine != "web" || bound.Purpose != "incident" {
		t.Fatal(bound.Incident)
	}
}
