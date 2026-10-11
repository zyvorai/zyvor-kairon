// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
)

var (
	// streamReconnectDelay is the pause before re-opening an upstream watch
	// the API server closed (watches are routinely cut after a few minutes).
	streamReconnectDelay = time.Second
	// streamHeartbeat keeps proxies and browsers from idling the response out.
	streamHeartbeat = 20 * time.Second
)

// streamChange is one SSE "change" payload: a deliberately small summary of
// a watch event. The dashboard patches or refetches from it; it never needs
// the full object, and the summary keeps spec contents off the wire.
type streamChange struct {
	Type      string `json:"type"` // ADDED, MODIFIED, DELETED
	Kind      string `json:"kind"` // Machine, MachineMigration
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Phase     string `json:"phase,omitempty"`
	NodeName  string `json:"nodeName,omitempty"`
	Message   string `json:"message,omitempty"`
}

type streamObject struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		NodeName string `json:"nodeName"`
	} `json:"spec"`
	Status struct {
		Phase    string `json:"phase"`
		NodeName string `json:"nodeName"`
		Message  string `json:"message"`
	} `json:"status"`
}

type watchFunc func(ctx context.Context, labelSelector, resourceVersion string, out chan<- kube.WatchEvent) error

// handleStream serves GET /api/v1/stream, a Server-Sent Events feed of
// Machine and MachineMigration changes the caller may see. It carries the
// same bearer auth as every other route, so a browser reads it with fetch
// streaming rather than EventSource (which cannot set headers).
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	if s.NamespaceScopingEnabled && usernameFromContext(r.Context()) == "" {
		writeError(w, http.StatusForbidden, "streaming requires a per-operator identity when namespace scoping is on")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	machines := make(chan kube.WatchEvent, 64)
	migrations := make(chan kube.WatchEvent, 64)
	go watchLoop(ctx, s.Kube.WatchMachines, machines)
	go watchLoop(ctx, s.Kube.WatchMachineMigrations, migrations)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	tick := time.NewTicker(streamHeartbeat)
	defer tick.Stop()
	for {
		var ev kube.WatchEvent
		var kind string
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
			continue
		case ev = <-machines:
			kind = "Machine"
		case ev = <-migrations:
			kind = "MachineMigration"
		}
		change, ok := summarizeWatchEvent(kind, ev)
		if !ok || !s.authorizedForNamespace(ctx, change.Namespace) {
			continue
		}
		b, _ := json.Marshal(change)
		if _, err := fmt.Fprintf(w, "event: change\ndata: %s\n\n", b); err != nil {
			return
		}
		flusher.Flush()
	}
}

// watchLoop keeps one upstream watch open, reconnecting after it ends.
func watchLoop(ctx context.Context, fn watchFunc, out chan<- kube.WatchEvent) {
	for ctx.Err() == nil {
		_ = fn(ctx, "", "", out)
		select {
		case <-ctx.Done():
			return
		case <-time.After(streamReconnectDelay):
		}
	}
}

func summarizeWatchEvent(kind string, ev kube.WatchEvent) (streamChange, bool) {
	switch ev.Type {
	case "ADDED", "MODIFIED", "DELETED":
	default: // BOOKMARK, ERROR
		return streamChange{}, false
	}
	var o streamObject
	if err := json.Unmarshal(ev.Object, &o); err != nil || o.Metadata.Name == "" {
		return streamChange{}, false
	}
	node := o.Status.NodeName
	if node == "" {
		node = o.Spec.NodeName
	}
	return streamChange{
		Type: ev.Type, Kind: kind,
		Namespace: o.Metadata.Namespace, Name: o.Metadata.Name,
		Phase: o.Status.Phase, NodeName: node, Message: o.Status.Message,
	}, true
}
