// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

func fakePod(ns, name, app, node string, ready bool, restarts int) map[string]any {
	meta := map[string]any{"name": name, "namespace": ns}
	if app != "" {
		meta["labels"] = map[string]string{"app.kubernetes.io/name": app}
	}
	return map[string]any{
		"metadata": meta,
		"spec":     map[string]any{"nodeName": node, "containers": []map[string]string{{"name": "c"}}},
		"status": map[string]any{
			"phase":             "Running",
			"containerStatuses": []map[string]any{{"ready": ready, "restartCount": restarts}},
		},
	}
}

func TestPodsAndHealth(t *testing.T) {
	fk := newFakeKube()
	fk.podsJSON = []map[string]any{
		fakePod("kairon-system", "kairon-node-a", "kairon-node", "w1", true, 0),
		fakePod("kairon-system", "kairon-node-b", "kairon-node", "w2", false, 3),
		fakePod("kairon-system", "kairon-ui-x", "kairon-ui", "w1", true, 1),
		fakePod("apps", "web-1", "web", "w1", true, 0),
	}
	h := newTestServer(t, fk, "").Handler()

	var pods []podRow
	rr := doJSON(t, h, http.MethodGet, "/api/v1/pods", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("pods: %d %s", rr.Code, rr.Body.String())
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &pods)
	if len(pods) != 3 {
		t.Fatalf("default scope should list only kairon pods, got %d", len(pods))
	}
	rr = doJSON(t, h, http.MethodGet, "/api/v1/pods?scope=all&node=w1", "", nil)
	pods = nil
	_ = json.Unmarshal(rr.Body.Bytes(), &pods)
	if len(pods) != 3 {
		t.Fatalf("scope=all&node=w1 want 3 pods, got %d", len(pods))
	}

	var health []componentHealth
	rr = doJSON(t, h, http.MethodGet, "/api/v1/health", "", nil)
	_ = json.Unmarshal(rr.Body.Bytes(), &health)
	if len(health) != 2 {
		t.Fatalf("want 2 components, got %+v", health)
	}
	node := health[0]
	if node.Component != "kairon-node" || node.Pods != 2 || node.Ready != 1 || node.Restarts != 3 || node.Healthy {
		t.Errorf("kairon-node health = %+v", node)
	}
	if ui := health[1]; ui.Component != "kairon-ui" || !ui.Healthy {
		t.Errorf("kairon-ui health = %+v", ui)
	}
}
