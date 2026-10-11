// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"net/http"
	"sort"
	"time"
)

// timelineEntry is one row of GET /api/v1/machines/{ns}/{name}/timeline:
// either a core/v1 Event about the Machine or a status condition's most
// recent transition, normalised to one shape so the dashboard can render a
// single chronological list.
type timelineEntry struct {
	Time    time.Time `json:"time"`
	Source  string    `json:"source"` // "event" or "condition"
	Type    string    `json:"type"`   // Normal/Warning for events, condition type otherwise
	Reason  string    `json:"reason,omitempty"`
	Message string    `json:"message,omitempty"`
	Status  string    `json:"status,omitempty"` // condition status (True/False/Unknown)
	Count   int32     `json:"count,omitempty"`  // event repeat count
	Origin  string    `json:"origin,omitempty"` // reporting component for events
}

// handleMachineTimeline merges the Machine's Kubernetes Events with its
// current status conditions, newest first. It adds no new data source:
// the Events are the same ones `kaironctl events` lists and the conditions
// are the ones already in the Machine object.
func (s *Server) handleMachineTimeline(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("namespace"), r.PathValue("name")
	m, err := s.Kube.GetMachine(r.Context(), ns, name)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	events, err := s.Kube.ListEventsFor(r.Context(), ns, "Machine", name)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	out := make([]timelineEntry, 0, len(events)+len(m.Status.Conditions))
	for _, e := range events {
		ts := e.LastTimestamp
		if ts.IsZero() {
			ts = e.FirstTimestamp
		}
		out = append(out, timelineEntry{
			Time: ts, Source: "event", Type: e.Type, Reason: e.Reason,
			Message: e.Message, Count: e.Count, Origin: e.Source.Component,
		})
	}
	for _, c := range m.Status.Conditions {
		out = append(out, timelineEntry{
			Time: c.LastTransitionTime, Source: "condition", Type: c.Type,
			Reason: c.Reason, Message: c.Message, Status: c.Status,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	writeJSON(w, http.StatusOK, out)
}
