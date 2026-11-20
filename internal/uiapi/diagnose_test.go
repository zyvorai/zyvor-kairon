// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package uiapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/agentplane"
	"github.com/zyvorai/kairon/internal/model"
)

func TestAgentDiagnoseMachine(t *testing.T) {
	t.Setenv("KAIRON_LLM_URL", "")
	fk := newFakeKube()
	fk.machines["job-1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "job-1", Namespace: "default"},
		Status: model.MachineStatus{Phase: "Pending", Conditions: []model.Condition{
			{Type: "Scheduled", Status: "False", Reason: "Unschedulable", Message: "no node fits"},
		}},
	}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	h := (&Server{Kube: mustKubeClientAt(t, kubeSrv.URL)}).Handler()

	rr := doJSON(t, h, http.MethodGet, "/api/v1/agent/diagnose/default/machine/job-1", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	var d agentplane.Diagnosis
	if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Healthy || len(d.Causes) == 0 || !strings.Contains(d.Causes[0].Title, "Scheduled") || d.Apply {
		t.Fatalf("%+v", d)
	}
	if rr := doJSON(t, h, http.MethodGet, "/api/v1/agent/diagnose/default/machine/missing", "", nil); rr.Code != http.StatusNotFound {
		t.Fatalf("missing machine: %d", rr.Code)
	}
	if rr := doJSON(t, h, http.MethodGet, "/api/v1/agent/diagnose/default/pod/job-1", "", nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad kind: %d", rr.Code)
	}
}
