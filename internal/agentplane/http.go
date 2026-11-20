// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agentplane

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/zyvorai/kairon/internal/llm"
	"github.com/zyvorai/kairon/internal/model"
)

// Handler is the read-only agent API. It does not touch the apiserver.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agent/compile-policy", handleCompile)
	mux.HandleFunc("POST /api/v1/agent/explain-drops", handleDrops)
	mux.HandleFunc("POST /api/v1/agent/anomalies", handleAnomalies)
	mux.HandleFunc("POST /api/v1/agent/claims/step", handleStep)
	mux.HandleFunc("POST /api/v1/agent/cpu-label", handleCPU)
	mux.HandleFunc("POST /api/v1/agent/confidential", handleConfidential)
	mux.HandleFunc("POST /api/v1/agent/gateway", handleGateway)
	mux.HandleFunc("POST /api/v1/agent/ask", handleAsk)
	return mux
}

// handleAsk returns a validated proposal from the configured model. 503
// when no model is configured; the proposal is never applied.
func handleAsk(w http.ResponseWriter, r *http.Request) {
	var in AssistRequest
	if err := decodeBody(w, r, &in); err != nil {
		return
	}
	client := llm.FromEnv()
	if client == nil {
		writeErr(w, http.StatusServiceUnavailable, llm.ErrNotConfigured)
		return
	}
	res, err := Assist(r.Context(), client, in)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeOK(w, res)
}

func handleCompile(w http.ResponseWriter, r *http.Request) {
	var in PolicyIntent
	if err := decodeBody(w, r, &in); err != nil {
		return
	}
	pol, err := CompilePolicy(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeOK(w, map[string]any{"apply": false, "policy": pol})
}

func handleDrops(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Machine string `json:"machine,omitempty"`
		Drops   []Drop `json:"drops"`
	}
	if err := decodeBody(w, r, &in); err != nil {
		return
	}
	writeOK(w, map[string]any{"hypotheses": ExplainDrops(in.Drops), "events": EventsFromDrops(in.Machine, in.Drops)})
}

func handleAnomalies(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Machine string `json:"machine"`
		Flows   []Flow `json:"flows"`
		Drops   []Drop `json:"drops"`
	}
	if err := decodeBody(w, r, &in); err != nil {
		return
	}
	findings := Detect(in.Flows, in.Drops)
	writeOK(w, map[string]any{"findings": findings, "events": EventsFromFindings(in.Machine, findings)})
}

func handleStep(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Now   time.Time          `json:"now"`
		Claim model.MachineClaim `json:"claim"`
		Warm  []WarmMachine      `json:"warm"`
	}
	if err := decodeBody(w, r, &in); err != nil {
		return
	}
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	}
	decision, err := StepClaim(in.Now, in.Claim, in.Warm)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeOK(w, decision)
}

func handleCPU(w http.ResponseWriter, r *http.Request) {
	var in NodeCPUReport
	if err := decodeBody(w, r, &in); err != nil {
		return
	}
	up, err := CPULabelUpdate(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeOK(w, up)
}

func handleConfidential(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Requested string      `json:"requested"`
		Node      Attestation `json:"node"`
	}
	if err := decodeBody(w, r, &in); err != nil {
		return
	}
	writeOK(w, ProjectConfidential(in.Requested, in.Node))
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return err
	}
	return nil
}

func writeOK(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func handleGateway(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name     string        `json:"name"`
		Forwards []PortForward `json:"forwards"`
	}
	if err := decodeBody(w, r, &in); err != nil {
		return
	}
	b, err := BindGateway(in.Name, in.Forwards)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeOK(w, map[string]any{"apply": false, "binding": b})
}
